package gateway

import (
	"fmt"
	"sync"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// TraceSpan 追踪 span
type TraceSpan struct {
	TraceID      string            // 追踪ID
	SpanID       string            // Span ID
	ParentSpanID string            // 父 Span ID
	Name         string            // Span 名称
	StartTime    time.Time         // 开始时间
	EndTime      time.Time         // 结束时间
	Duration     time.Duration     // 持续时间
	Attributes   map[string]string // 附加属性
	Events       []TraceEvent      // 事件
}

// TraceEvent 追踪事件
type TraceEvent struct {
	Timestamp time.Time         // 时间戳
	Name      string            // 事件名称
	Attributes map[string]string // 事件属性
}

// Tracer 追踪器
type Tracer struct {
	traces     map[string][]*TraceSpan // 追踪映射
	mutex      sync.RWMutex            // 互斥锁
	cleanupInterval time.Duration       // 清理间隔
}

// NewTracer 创建追踪器
// 参数:
//   cleanupInterval: 清理间隔
// 返回值:
//   *Tracer: 追踪器实例
func NewTracer(cleanupInterval time.Duration) *Tracer {
	if cleanupInterval == 0 {
		cleanupInterval = 5 * time.Minute // 默认清理间隔
	}

	tracer := &Tracer{
		traces:          make(map[string][]*TraceSpan),
		cleanupInterval: cleanupInterval,
	}

	// 启动清理任务
	go tracer.cleanup()

	return tracer
}

// StartSpan 开始一个 span
// 参数:
//   traceID: 追踪ID
//   spanName: Span 名称
//   parentSpanID: 父 Span ID
// 返回值:
//   *TraceSpan: 追踪 span
func (t *Tracer) StartSpan(traceID, spanName, parentSpanID string) *TraceSpan {
	span := &TraceSpan{
		TraceID:      traceID,
		SpanID:       generateConnectionID(),
		ParentSpanID: parentSpanID,
		Name:         spanName,
		StartTime:    time.Now(),
		Attributes:   make(map[string]string),
		Events:       make([]TraceEvent, 0),
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()

	if _, exists := t.traces[traceID]; !exists {
		t.traces[traceID] = make([]*TraceSpan, 0)
	}

	t.traces[traceID] = append(t.traces[traceID], span)

	return span
}

// EndSpan 结束一个 span
// 参数:
//   span: 追踪 span
func (t *Tracer) EndSpan(span *TraceSpan) {
	span.EndTime = time.Now()
	span.Duration = span.EndTime.Sub(span.StartTime)

	tlog.Debug("Span completed", 
		"traceID", span.TraceID,
		"spanID", span.SpanID,
		"name", span.Name,
		"duration", span.Duration,
	)
}

// AddAttribute 添加属性到 span
// 参数:
//   span: 追踪 span
//   key: 属性键
//   value: 属性值
func (t *Tracer) AddAttribute(span *TraceSpan, key, value string) {
	span.Attributes[key] = value
}

// AddEvent 添加事件到 span
// 参数:
//   span: 追踪 span
//   eventName: 事件名称
//   attributes: 事件属性
func (t *Tracer) AddEvent(span *TraceSpan, eventName string, attributes map[string]string) {
	event := TraceEvent{
		Timestamp:  time.Now(),
		Name:       eventName,
		Attributes: attributes,
	}

	span.Events = append(span.Events, event)
}

// GetTrace 获取完整追踪
// 参数:
//   traceID: 追踪ID
// 返回值:
//   []*TraceSpan: 追踪 span 列表
func (t *Tracer) GetTrace(traceID string) []*TraceSpan {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	traces, exists := t.traces[traceID]
	if !exists {
		return nil
	}

	// 复制一份返回，避免外部修改
	result := make([]*TraceSpan, len(traces))
	copy(result, traces)
	return result
}

// CleanupTrace 清理追踪
// 参数:
//   traceID: 追踪ID
func (t *Tracer) CleanupTrace(traceID string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	delete(t.traces, traceID)
}

// cleanup 清理过期追踪
func (t *Tracer) cleanup() {
	ticker := time.NewTicker(t.cleanupInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		t.cleanupExpiredTraces()
	}
}

// cleanupExpiredTraces 清理过期追踪
func (t *Tracer) cleanupExpiredTraces() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	now := time.Now()
	for traceID, spans := range t.traces {
		// 检查是否所有 span 都已结束且超过1小时
		allEnded := true
		oldestSpan := now

		for _, span := range spans {
			if span.EndTime.IsZero() {
				allEnded = false
				break
			}
			if span.StartTime.Before(oldestSpan) {
				oldestSpan = span.StartTime
			}
		}

		if allEnded && now.Sub(oldestSpan) > 1*time.Hour {
			delete(t.traces, traceID)
			tlog.Debug("清理过期追踪", "traceID", traceID)
		}
	}
}

// GetStats 获取追踪统计信息
// 返回值:
//   map[string]int: 统计信息
func (t *Tracer) GetStats() map[string]int {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	stats := map[string]int{
		"totalTraces": 0,
		"totalSpans":  0,
		"activeTraces": 0,
	}

	stats["totalTraces"] = len(t.traces)

	for _, spans := range t.traces {
		stats["totalSpans"] += len(spans)
		// 检查是否有未结束的 span
		for _, span := range spans {
			if span.EndTime.IsZero() {
				stats["activeTraces"]++
				break
			}
		}
	}

	return stats
}

// GenerateTraceID 生成追踪ID
// 返回值:
//   string: 追踪ID
func GenerateTraceID() string {
	return fmt.Sprintf("trace_%s", generateConnectionID())
}
