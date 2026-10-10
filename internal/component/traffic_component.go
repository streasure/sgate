package component

import (
	"context"
	"fmt"
	"os"

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
	wasmCfg        config.WasmRuntimeConfig

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
		wasmCfg:        cfg.WasmRuntime,
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

	// WASM 模块加载：启动时从文件读入并实例化，供 filterChain 的 wasm-filter 调用。
	// 任一模块加载失败返回错误使组件 Init 失败（fail-fast，不静默降级）。
	if c.wasmCfg.Enabled {
		if err := c.loadWasmModules(); err != nil {
			return err
		}
	}

	setTrafficResources(c.CanaryFilter, c.TrafficMirror, c.Degradation)
	return nil
}

// loadWasmModules 读取并加载配置中声明的所有 WASM 模块。
func (c *TrafficComponent) loadWasmModules() error {
	rt := traffic.GetWasmRuntime()
	if rt == nil {
		return fmt.Errorf("wasm runtime enabled but no runtime implementation available")
	}
	for _, m := range c.wasmCfg.Modules {
		if m.Name == "" || m.Path == "" {
			return fmt.Errorf("wasm module entry requires name and path (name=%q path=%q)", m.Name, m.Path)
		}
		bytes, err := os.ReadFile(m.Path)
		if err != nil {
			return fmt.Errorf("read wasm module %q from %q: %w", m.Name, m.Path, err)
		}
		if err := rt.LoadModule(m.Name, bytes); err != nil {
			return fmt.Errorf("load wasm module %q: %w", m.Name, err)
		}
		tlog.Info(context.TODO(), "wasm module loaded name=%s path=%s runtime=%s", m.Name, m.Path, rt.Type())
	}
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
	if c.wasmCfg.Enabled {
		if rt := traffic.GetWasmRuntime(); rt != nil {
			for _, m := range c.wasmCfg.Modules {
				if err := rt.UnloadModule(m.Name); err != nil {
					tlog.Warn(context.TODO(), "unload wasm module failed name=%s error=%v", m.Name, err)
				}
			}
		}
	}
}
