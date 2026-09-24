package component

import (
	"context"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

// ObservabilityComponent 管理所有可观测性子模块的生命周期：
// Tracer, OTelTracer, PProfServer, LogSanitizer, LatencyTracker.
type ObservabilityComponent struct {
	component.BaseComponent

	otelCfg   config.OTelTracerConfig
	pprofAddr string

	Tracer         *obs.Tracer
	OTelTracer     *obs.OTelTracer
	LogSanitizer   *obs.LogSanitizer
	LatencyTracker *obs.LatencyTracker
}

// NewObservabilityComponent 创建可观测性组件，配置从 config.Get() 读取。
func NewObservabilityComponent() *ObservabilityComponent {
	cfg := config.Get()
	return &ObservabilityComponent{
		otelCfg:   cfg.OTelTracer,
		pprofAddr: cfg.Monitoring.PprofAddr,
	}
}

func (c *ObservabilityComponent) Name() string { return "observability" }
func (c *ObservabilityComponent) Order() int   { return 200 }

func (c *ObservabilityComponent) Init() error {
	tlog.Info(context.TODO(), "observability component init")

	c.Tracer = obs.NewTracer(5 * time.Minute)
	c.LatencyTracker = obs.NewLatencyTracker(10000)
	c.LogSanitizer = obs.NewLogSanitizer()

	// OpenTelemetry 分布式追踪。
	if c.otelCfg.Enabled {
		c.OTelTracer = obs.NewOTelTracer(c.otelCfg)
		types.GetFilterChain().AddFilter(&obs.OTelSpanFilter{Tracer: c.OTelTracer})
	}

	setObservabilityResources(c.Tracer, c.OTelTracer, c.LogSanitizer, c.LatencyTracker)
	return nil
}

func (c *ObservabilityComponent) Start() error {
	// pprof 性能剖析服务。
	if c.pprofAddr != "" {
		obs.StartPProfServer(c.pprofAddr)
	}

	tlog.Info(context.TODO(), "observability component started otel=%v pprof=%s",
		c.otelCfg.Enabled,
		c.pprofAddr)
	return nil
}

func (c *ObservabilityComponent) Destroy() {
	tlog.Info(context.TODO(), "observability component destroying")
	if c.Tracer != nil {
		c.Tracer.Stop()
	}
	if c.OTelTracer != nil {
		c.OTelTracer.Stop()
	}
	obs.StopPProfServer()
}
