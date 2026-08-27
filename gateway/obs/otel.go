package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway/types"
	"github.com/streasure/sgate/gateway/util"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// OTelSpan OpenTelemetry 风格 span
type OTelSpan struct {
	TraceID       string            `json:"traceId"`
	SpanID        string            `json:"id"`
	ParentID      string            `json:"parentId,omitempty"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind,omitempty"`
	Timestamp     int64             `json:"timestamp"`
	Duration      int64             `json:"duration"` // 微秒
	Tags          map[string]string `json:"tags,omitempty"`
	LocalEndpoint map[string]string `json:"localEndpoint,omitempty"`
	start         time.Time
}

// OTelTracer 标准 OpenTelemetry / Zipkin v2 风格追踪导出器
// 通过 HTTP 上报 span 到 Zipkin / Jaeger / OTel collector
type OTelTracer struct {
	mu            sync.Mutex
	endpoint      string // Zipkin v2 API URL（如 http://zipkin:9411/api/v2/spans）
	serviceName   string
	localEndpoint map[string]string
	httpClient    *http.Client
	queue         chan *OTelSpan
	sampled       atomic.Int32 // 采样率 1/采样率
	dropped       atomic.Int64
	exported      atomic.Int64
	stopCh        chan struct{}
}

// NewOTelTracer 创建追踪导出器
func NewOTelTracer(cfg config.OTelTracerConfig) *OTelTracer {
	t := &OTelTracer{
		endpoint:      cfg.Endpoint,
		serviceName:   cfg.ServiceName,
		localEndpoint: map[string]string{"serviceName": cfg.ServiceName},
		httpClient:    &http.Client{Timeout: 3 * time.Second},
		queue:         make(chan *OTelSpan, util.MaxInt(cfg.QueueSize, 1024)),
		stopCh:        make(chan struct{}),
	}
	if cfg.SampleRate > 0 {
		t.sampled.Store(int32(cfg.SampleRate))
	} else {
		t.sampled.Store(1) // 默认全采样（开发期）
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		go t.exportLoop()
	}
	return t
}

// StartSpan 开启一个 span
func (t *OTelTracer) StartSpan(traceID, name, parentID string) *OTelSpan {
	if !t.shouldSample(traceID) {
		return nil
	}
	return &OTelSpan{
		TraceID:       traceID,
		SpanID:        GenerateTraceID(),
		ParentID:      parentID,
		Name:          name,
		Kind:          "CLIENT",
		Timestamp:     time.Now().UnixMicro(),
		start:         time.Now(),
		LocalEndpoint: t.localEndpoint,
		Tags:          map[string]string{},
	}
}

// SetTag 设置 span 标签
func (t *OTelTracer) SetTag(span *OTelSpan, key, value string) {
	if span == nil {
		return
	}
	span.Tags[key] = value
}

// EndSpan 结束 span 并异步上报
func (t *OTelTracer) EndSpan(span *OTelSpan) {
	if span == nil {
		return
	}
	span.Duration = time.Since(span.start).Microseconds()
	select {
	case t.queue <- span:
		t.exported.Add(1)
	default:
		t.dropped.Add(1)
	}
}

func (t *OTelTracer) shouldSample(traceID string) bool {
	sr := t.sampled.Load()
	if sr <= 1 {
		return true
	}
	// 简单采样：traceID 哈希取模
	h := util.SimpleHash(traceID)
	return h%uint32(sr) == 0
}

func (t *OTelTracer) exportLoop() {
	batch := make([]*OTelSpan, 0, 64)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			t.flush(batch)
			return
		case span := <-t.queue:
			batch = append(batch, span)
			if len(batch) >= 64 {
				t.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				t.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (t *OTelTracer) flush(spans []*OTelSpan) {
	if len(spans) == 0 || t.endpoint == "" {
		return
	}
	body, err := json.Marshal(spans)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		tlog.Debug("otel trace export failed", "error", err)
		return
	}
	resp.Body.Close()
}

// Stats 追踪统计
func (t *OTelTracer) Stats() (exported, dropped int64) {
	return t.exported.Load(), t.dropped.Load()
}

// Stop 停止追踪
func (t *OTelTracer) Stop() {
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
}

// OTelSpanFilter OTel 追踪过滤器
type OTelSpanFilter struct {
	Tracer *OTelTracer
}

func (f *OTelSpanFilter) Name() string       { return "otel-tracer" }
func (f *OTelSpanFilter) Phase() types.FilterPhase { return types.PhaseForward }
func (f *OTelSpanFilter) Priority() int           { return 50 }

func (f *OTelSpanFilter) Process(fc *types.FilterContext) (bool, error) {
	if f.Tracer == nil {
		return true, nil
	}
	traceID := fc.Metadata["trace-id"]
	if traceID == "" {
		traceID = GenerateTraceID()
	}
	span := f.Tracer.StartSpan(traceID, "sgate.forward", "")
	if span != nil {
		f.Tracer.SetTag(span, "route", fc.Route)
		f.Tracer.SetTag(span, "conn", fc.ConnectionID)
		f.Tracer.SetTag(span, "ip", fc.RemoteIP)
		f.Tracer.SetTag(span, "user", fc.UserUUID)
		// 异步 EndSpan（用 timer 模拟 duration 计算）
		go func(s *OTelSpan) {
			time.Sleep(time.Microsecond) // 占位，主 span 在转发完成时 End
			f.Tracer.EndSpan(s)
		}(span)
	}
	return true, nil
}

// traceIDFromHeaders 从客户端传入的 trace-id（支持 W3C traceparent）
func extractTraceID(headers map[string]string) string {
	tp := headers["traceparent"]
	if tp == "" {
		return ""
	}
	// traceparent: 00-<trace-id>-<span-id>-<flags>
	parts := bytes.Split([]byte(tp), []byte("-"))
	if len(parts) >= 2 {
		return string(parts[1])
	}
	return ""
}

func init() {
	types.RegisterFilter("otel-tracer", func(cfg map[string]interface{}) (types.Filter, error) {
		c := config.OTelTracerConfig{
			Endpoint:    util.GetString(cfg, "endpoint"),
			ServiceName: util.GetString(cfg, "serviceName"),
			SampleRate:  util.GetInt(cfg, "sampleRate"),
			QueueSize:   util.GetInt(cfg, "queueSize"),
			Workers:     util.GetInt(cfg, "workers"),
		}
		if c.ServiceName == "" {
			c.ServiceName = "sgate"
		}
		t := NewOTelTracer(c)
		return &OTelSpanFilter{Tracer: t}, nil
	})
}
