package gateway

import (
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

// ObservabilityComponent manages the lifecycle of all observability sub-modules:
// Tracer, OTelTracer, PProfServer, LogSanitizer, LatencyTracker.
type ObservabilityComponent struct {
	component.BaseComponent

	otelCfg   config.OTelTracerConfig
	pprofAddr string

	Tracer         *obs.Tracer
	OTelTracer     *obs.OTelTracer
	LogSanitizer   *obs.LogSanitizer
	LatencyTracker *obs.LatencyTracker
	FilterChain    *types.FilterChain
}

func NewObservabilityComponent(otelCfg config.OTelTracerConfig, pprofAddr string, fc *types.FilterChain) *ObservabilityComponent {
	return &ObservabilityComponent{
		otelCfg:     otelCfg,
		pprofAddr:   pprofAddr,
		FilterChain: fc,
	}
}

func (c *ObservabilityComponent) Name() string { return "observability" }
func (c *ObservabilityComponent) Order() int   { return 200 }

func (c *ObservabilityComponent) Init() error {
	tlog.Info("observability component init")

	c.Tracer = obs.NewTracer(5 * time.Minute)
	c.LatencyTracker = obs.NewLatencyTracker(10000)
	c.LogSanitizer = obs.NewLogSanitizer()

	// OTel distributed tracing
	if c.otelCfg.Enabled {
		c.OTelTracer = obs.NewOTelTracer(c.otelCfg)
		c.FilterChain.AddFilter(&obs.OTelSpanFilter{Tracer: c.OTelTracer})
	}

	return nil
}

func (c *ObservabilityComponent) Start() error {
	// PProf server
	if c.pprofAddr != "" {
		obs.StartPProfServer(c.pprofAddr)
	}

	tlog.Info("observability component started",
		"otel", c.otelCfg.Enabled,
		"pprof", c.pprofAddr)
	return nil
}

func (c *ObservabilityComponent) Destroy() {
	tlog.Info("observability component destroying")
	if c.Tracer != nil {
		c.Tracer.Stop()
	}
	if c.OTelTracer != nil {
		c.OTelTracer.Stop()
	}
	obs.StopPProfServer()
}
