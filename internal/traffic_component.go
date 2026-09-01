package gateway

import (
	"github.com/streasure/util/component"
	"github.com/streasure/sgate/traffic"
	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// TrafficComponent manages the lifecycle of all traffic sub-modules:
// CanaryFilter, TrafficMirror, DegradationManager, eBPF, Wasm.
type TrafficComponent struct {
	component.BaseComponent

	canaryCfg        config.CanaryConfig
	mirrorCfg        config.TrafficMirrorConfig
	degradationCfg   config.DegradationConfig
	filterChain      *types.FilterChain

	CanaryFilter     *traffic.CanaryFilter
	TrafficMirror    *traffic.TrafficMirror
	Degradation      *traffic.DegradationManager
}

func NewTrafficComponent(canaryCfg config.CanaryConfig, mirrorCfg config.TrafficMirrorConfig, degradationCfg config.DegradationConfig, fc *types.FilterChain) *TrafficComponent {
	return &TrafficComponent{
		canaryCfg:      canaryCfg,
		mirrorCfg:      mirrorCfg,
		degradationCfg: degradationCfg,
		filterChain:    fc,
	}
}

func (c *TrafficComponent) Name() string { return "traffic" }
func (c *TrafficComponent) Order() int   { return 300 }

func (c *TrafficComponent) Init() error {
	tlog.Info("traffic component init")

	// Canary
	if c.canaryCfg.Enabled {
		c.CanaryFilter = traffic.NewCanaryFilter(c.canaryCfg)
		c.filterChain.AddFilter(c.CanaryFilter)
	}

	// Traffic Mirror
	if c.mirrorCfg.Enabled {
		c.TrafficMirror = traffic.NewTrafficMirror(c.mirrorCfg)
		c.filterChain.AddFilter(&traffic.MirrorFilter{TM: c.TrafficMirror})
	}

	// Degradation
	if c.degradationCfg.Enabled {
		c.Degradation = traffic.NewDegradationManager(c.degradationCfg.Rules)
		c.filterChain.AddFilter(c.Degradation)
	}

	return nil
}

func (c *TrafficComponent) Start() error {
	tlog.Info("traffic component started",
		"canary", c.canaryCfg.Enabled,
		"mirror", c.mirrorCfg.Enabled,
		"degradation", c.degradationCfg.Enabled)
	return nil
}

func (c *TrafficComponent) Destroy() {
	tlog.Info("traffic component destroying")
	// TrafficMirror manages its own worker goroutines
}
