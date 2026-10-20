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
// Tracer, OTelTracer, PProfServer, LatencyTracker.
type ObservabilityComponent struct {
	component.BaseComponent

	ctx context.Context

	otelCfg       config.OTelTracerConfig
	pprofAddr     string
	disableTracer bool

	// 导出字段仅包内使用（资源经 resources.go 全局量发布），不作为外部通路。
	tracer         *obs.Tracer
	otelTracer     *obs.OTelTracer
	latencyTracker *obs.LatencyTracker
}

// NewObservabilityComponent 创建可观测性组件，配置从 config.Get() 读取。
func NewObservabilityComponent() *ObservabilityComponent {
	cfg := config.Get()
	return &ObservabilityComponent{
		ctx:           context.Background(),
		otelCfg:       cfg.OTelTracer,
		pprofAddr:     cfg.Monitoring.PprofAddr,
		disableTracer: cfg.Monitoring.DisableTracer,
	}
}

func (c *ObservabilityComponent) Name() string { return "observability" }
func (c *ObservabilityComponent) Order() int   { return 200 }

func (c *ObservabilityComponent) Init() error {
	tlog.Info(c.ctx, "observability component init")

	if c.disableTracer {
		// 关闭后 component.Tracer() 返回 nil，消息管道走超级快速路径。
		tlog.Info(c.ctx, "internal tracer disabled (monitoring.disableTracer=true)")
	} else {
		c.tracer = obs.NewTracer(5 * time.Minute)
	}
	// 32768 ≈ 1s 窗口（采样步长 16 下 500K msg/s ≈ 31K 样本/s）；
	// 低速率时窗口覆盖更长时间，分位数更稳。内存 ~512KB 可忽略。
	c.latencyTracker = obs.NewLatencyTracker(32768)

	// OpenTelemetry 分布式追踪。
	if c.otelCfg.Enabled {
		c.otelTracer = obs.NewOTelTracer(c.otelCfg)
		types.GetFilterChain().AddFilter(&obs.OTelSpanFilter{Tracer: c.otelTracer})
	}

	setObservabilityResources(c.tracer, c.otelTracer, c.latencyTracker)
	return nil
}

func (c *ObservabilityComponent) Start() error {
	// pprof 性能剖析服务。
	if c.pprofAddr != "" {
		obs.StartPProfServer(c.pprofAddr)
	}

	tlog.Info(c.ctx, "observability component started otel=%v pprof=%s",
		c.otelCfg.Enabled,
		c.pprofAddr)
	return nil
}

func (c *ObservabilityComponent) Destroy() {
	tlog.Info(c.ctx, "observability component destroying")
	if c.tracer != nil {
		c.tracer.Stop()
	}
	if c.otelTracer != nil {
		c.otelTracer.Stop()
	}
	obs.StopPProfServer()
}
