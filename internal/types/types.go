// Package types 定义 gateway 各子包共享的接口与类型，
// 打破 core ↔ security/traffic 之间的循环依赖。
package types

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// FilterPhase 过滤器执行阶段
type FilterPhase int

const (
	PhasePreAuth  FilterPhase = iota // 鉴权前（黑名单/WAF/限流）
	PhaseAuth                        // 鉴权（JWT/Token）
	PhasePostAuth                    // 鉴权后（灰度/镜像/路由前）
	PhaseForward                     // 转发前（最终修改请求）
)

// FilterContext 过滤器上下文（贯穿整条链）
type FilterContext struct {
	Ctx          context.Context
	ConnectionID string
	RemoteIP     string
	Route        string
	Cmd          int32
	Data         []byte
	UserUUID     string
	Metadata     map[string]string // 透传元数据（如灰度标记、镜像标记）
	// 结果控制
	Abort      bool   // 中止后续过滤器与转发
	DropReason string // 中止原因
	// 镜像副作用
	Mirrored bool
}

// Filter SPI 过滤器接口
// 返回 (continue, error)：continue=false 表示中止链
type Filter interface {
	Name() string
	Phase() FilterPhase
	Priority() int // 数字越小越靠前
	Process(fc *FilterContext) (bool, error)
}

// FilterFactory SPI 工厂：按名称动态构造过滤器
type FilterFactory func(cfg map[string]any) (Filter, error)

// FilterChain 过滤器链
type FilterChain struct {
	mu       sync.RWMutex
	filters  []Filter
	registry map[string]FilterFactory // 全局 SPI 注册表
	regMu    sync.RWMutex
	enabled  atomic.Int32
}

var (
	globalFilterRegistry   = map[string]FilterFactory{}
	globalFilterRegistryMu sync.RWMutex
	globalFilterChain      *FilterChain
)

// RegisterFilter 全局注册过滤器工厂（SPI 入口）
func RegisterFilter(name string, f FilterFactory) {
	globalFilterRegistryMu.Lock()
	defer globalFilterRegistryMu.Unlock()
	globalFilterRegistry[name] = f
}

// InitFilterChain 创建全局过滤器链，应在配置加载后调用一次。
func InitFilterChain() *FilterChain {
	globalFilterChain = NewFilterChain()
	return globalFilterChain
}

// GetFilterChain 返回全局过滤器链，需先调用 InitFilterChain。
func GetFilterChain() *FilterChain {
	return globalFilterChain
}

// NewFilterChain 创建过滤器链
func NewFilterChain() *FilterChain {
	globalFilterRegistryMu.RLock()
	registry := make(map[string]FilterFactory, len(globalFilterRegistry))
	maps.Copy(registry, globalFilterRegistry)
	globalFilterRegistryMu.RUnlock()
	fc := &FilterChain{
		registry: registry,
	}
	fc.enabled.Store(1)
	return fc
}

// Register 注册过滤器工厂到本链
func (fc *FilterChain) Register(name string, f FilterFactory) {
	fc.regMu.Lock()
	defer fc.regMu.Unlock()
	fc.registry[name] = f
}

// LoadByName 按名称动态加载过滤器（SPI）
func (fc *FilterChain) LoadByName(name string, cfg map[string]any) error {
	fc.regMu.RLock()
	factory, ok := fc.registry[name]
	fc.regMu.RUnlock()
	if !ok {
		return ErrFilterNotFound{Name: name}
	}
	f, err := factory(cfg)
	if err != nil {
		return err
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.filters = append(fc.filters, f)
	fc.sortFiltersLocked()
	return nil
}

// AddFilter 直接添加已构造的过滤器
func (fc *FilterChain) AddFilter(f Filter) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.filters = append(fc.filters, f)
	fc.sortFiltersLocked()
}

// Len 返回链上过滤器数量（供网关快速路径判定：链非空时不可跳过过滤器执行）。
func (fc *FilterChain) Len() int {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return len(fc.filters)
}

// RunByPhase 执行指定阶段的过滤器，返回 false 表示链被中止。
// 过滤器返回 error 时 fail-closed：中止链，不再放行。
func (fc *FilterChain) RunByPhase(phase FilterPhase, fcx *FilterContext) bool {
	if fc.enabled.Load() == 0 {
		return true
	}
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	for _, f := range fc.filters {
		if f.Phase() != phase {
			continue
		}
		ok, err := f.Process(fcx)
		if err != nil {
			if fcx.DropReason == "" {
				fcx.DropReason = f.Name() + ": " + err.Error()
			}
			fcx.Abort = true
			return false
		}
		if !ok {
			fcx.Abort = true
			return false
		}
	}
	return true
}

func (fc *FilterChain) sortFiltersLocked() {
	slices.SortStableFunc(fc.filters, func(a, b Filter) int {
		return cmp.Compare(a.Priority(), b.Priority())
	})
}

// ErrFilterNotFound 过滤器未注册错误
type ErrFilterNotFound struct{ Name string }

func (e ErrFilterNotFound) Error() string { return "filter not found: " + e.Name }
