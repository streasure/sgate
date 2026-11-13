package component

import (
	"context"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
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

	// 资源字段包内生命周期管理（Destroy 用），Init 末尾同步写入
	// 注入的 *Resources 发布给 Gateway，不作为外部通路。
	tracer         *obs.Tracer
	otelTracer     *obs.OTelTracer
	latencyTracker *obs.LatencyTracker

	res *Resources
}

// NewObservabilityComponent 创建可观测性组件：配置与资源容器由组合根（main）显式传入。
func NewObservabilityComponent(cfg *config.Config, res *Resources) *ObservabilityComponent {
	return &ObservabilityComponent{
		ctx:           context.Background(),
		otelCfg:       cfg.OTelTracer,
		pprofAddr:     cfg.Monitoring.PprofAddr,
		disableTracer: cfg.Monitoring.DisableTracer,
		res:           res,
	}
}

func (c *ObservabilityComponent) Name() string { return "observability" }
func (c *ObservabilityComponent) Order() int   { return 200 }

func (c *ObservabilityComponent) Init() error {
	tlog.Info(c.ctx, "observability component init")

	if c.disableTracer {
		// 关闭后注入资源的 Tracer 为 nil，消息管道走超级快速路径。
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
		c.res.FC.AddFilter(&obs.OTelSpanFilter{Tracer: c.otelTracer})
	}

	// 发布到注入的资源容器（Gateway Order 最后读取）。
	c.res.Tracer = c.tracer
	c.res.OTelTracer = c.otelTracer
	c.res.LatencyTracker = c.latencyTracker
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
