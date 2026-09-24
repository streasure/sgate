package component

import (
	"context"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

// TrafficComponent 管理灰度、流量镜像、降级、eBPF 和 Wasm 等流量组件的生命周期。
type TrafficComponent struct {
	component.BaseComponent

	canaryCfg      config.CanaryConfig
	mirrorCfg      config.TrafficMirrorConfig
	degradationCfg config.DegradationConfig

	CanaryFilter  *traffic.CanaryFilter
	TrafficMirror *traffic.TrafficMirror
	Degradation   *traffic.DegradationManager
}

// NewTrafficComponent 创建流量组件，配置从 config.Get() 读取。
func NewTrafficComponent() *TrafficComponent {
	cfg := config.Get()
	return &TrafficComponent{
		canaryCfg:      cfg.Canary,
		mirrorCfg:      cfg.TrafficMirror,
		degradationCfg: cfg.Degradation,
	}
}

func (c *TrafficComponent) Name() string { return "traffic" }
func (c *TrafficComponent) Order() int   { return 300 }

func (c *TrafficComponent) Init() error {
	tlog.Info(context.TODO(), "traffic component init")

	// 灰度过滤器。
	if c.canaryCfg.Enabled {
		c.CanaryFilter = traffic.NewCanaryFilter(c.canaryCfg)
		types.GetFilterChain().AddFilter(c.CanaryFilter)
	}

	// 流量镜像。
	if c.mirrorCfg.Enabled {
		c.TrafficMirror = traffic.NewTrafficMirror(c.mirrorCfg)
		types.GetFilterChain().AddFilter(&traffic.MirrorFilter{TM: c.TrafficMirror})
	}

	// 降级管理器。
	if c.degradationCfg.Enabled {
		c.Degradation = traffic.NewDegradationManager(c.degradationCfg.Rules)
		types.GetFilterChain().AddFilter(c.Degradation)
	}

	setTrafficResources(c.CanaryFilter, c.TrafficMirror, c.Degradation)
	return nil
}

func (c *TrafficComponent) Start() error {
	tlog.Info(context.TODO(), "traffic component started canary=%v mirror=%v degradation=%v",
		c.canaryCfg.Enabled,
		c.mirrorCfg.Enabled,
		c.degradationCfg.Enabled)
	return nil
}

func (c *TrafficComponent) Destroy() {
	tlog.Info(context.TODO(), "traffic component destroying")
	if c.TrafficMirror != nil {
		c.TrafficMirror.Stop()
	}
}
