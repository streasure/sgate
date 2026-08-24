package gateway

import (
	"sync"
	"sync/atomic"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// DegradationManager 降级管理器
// 功能：当后端不可用或错误率飙升时，按规则返回兜底响应而非透传，避免雪崩
type DegradationManager struct {
	mu             sync.RWMutex
	rules          map[string]*degradeRule // key = route
	enabled        atomic.Int32
	triggeredCount atomic.Int64
}

type degradeRule struct {
	route          string
	errorThreshold float64 // 错误率阈值（0-1）
	windowSize     int     // 滑动窗口大小
	recentErrors   []bool  // 错误窗口
	pos            int
	count          int
	errorCount     int
	fallbackData   []byte // 兜底响应数据
	degraded       atomic.Int32
	coolDown       time.Duration
	lastDegrade    atomic.Int64
}

// NewDegradationManager 创建降级管理器
func NewDegradationManager(rules []DegradationRuleConfig) *DegradationManager {
	m := &DegradationManager{rules: make(map[string]*degradeRule)}
	m.enabled.Store(1)
	for _, rc := range rules {
		r := &degradeRule{
			route:          rc.Route,
			errorThreshold: rc.ErrorThreshold,
			windowSize:     rc.WindowSize,
			recentErrors:   make([]bool, rc.WindowSize),
			fallbackData:   []byte(rc.FallbackData),
			coolDown:       parseDurationDefault(rc.CoolDown, 30*time.Second),
		}
		if r.errorThreshold <= 0 {
			r.errorThreshold = 0.5
		}
		if r.windowSize <= 0 {
			r.windowSize = 100
			r.recentErrors = make([]bool, 100)
		}
		m.rules[rc.Route] = r
	}
	return m
}

func (m *DegradationManager) Name() string       { return "degradation" }
func (m *DegradationManager) Phase() FilterPhase { return PhaseForward }
func (m *DegradationManager) Priority() int      { return 500 }

// Process 检查是否应降级（返回兜底而非转发）
func (m *DegradationManager) Process(fc *FilterContext) (bool, error) {
	if m.enabled.Load() == 0 {
		return true, nil
	}
	m.mu.RLock()
	r, ok := m.rules[fc.Route]
	m.mu.RUnlock()
	if !ok {
		return true, nil
	}
	if r.degraded.Load() == 1 {
		// 还在冷却期内
		last := time.Unix(r.lastDegrade.Load(), 0)
		if time.Since(last) < r.coolDown {
			// 替换为兜底数据
			fc.Data = r.fallbackData
			fc.Metadata["degraded"] = "true"
			// 中止后续过滤器：直接走兜底
			fc.Abort = false // 继续转发兜底数据
			return true, nil
		}
		// 冷却到期，恢复
		r.degraded.Store(0)
	}
	return true, nil
}

// RecordResult 记录请求结果
// 注意：写操作（r.pos/r.recentErrors/r.count/r.errorCount）必须用写锁
func (m *DegradationManager) RecordResult(route string, isError bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rules[route]
	if !ok {
		return
	}
	pos := r.pos % r.windowSize
	if r.count >= r.windowSize {
		// 滑动窗口
		if r.recentErrors[pos] {
			r.errorCount--
		}
	}
	r.recentErrors[pos] = isError
	if isError {
		r.errorCount++
	}
	r.pos++
	r.count++
	// 计算错误率
	if r.count >= r.windowSize {
		rate := float64(r.errorCount) / float64(r.windowSize)
		if rate >= r.errorThreshold {
			if r.degraded.CompareAndSwap(0, 1) {
				r.lastDegrade.Store(time.Now().Unix())
				m.triggeredCount.Add(1)
				tlog.Warn("degradation triggered",
					"route", route,
					"errorRate", rate,
					"threshold", r.errorThreshold)
			}
		}
	}
}

func (m *DegradationManager) Enable() { m.enabled.Store(1) }

// GetTriggeredCount 返回累计触发降级次数
func (m *DegradationManager) GetTriggeredCount() int64 {
	return m.triggeredCount.Load()
}
func (m *DegradationManager) Disable() { m.enabled.Store(0) }

// AddRule 动态添加降级规则
func (m *DegradationManager) AddRule(rc DegradationRuleConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[rc.Route] = &degradeRule{
		route:          rc.Route,
		errorThreshold: rc.ErrorThreshold,
		windowSize:     rc.WindowSize,
		recentErrors:   make([]bool, rc.WindowSize),
		fallbackData:   []byte(rc.FallbackData),
		coolDown:       parseDurationDefault(rc.CoolDown, 30*time.Second),
	}
}

func init() {
	RegisterFilter("degradation", func(cfg map[string]interface{}) (Filter, error) {
		rules := []DegradationRuleConfig{}
		if v, ok := cfg["rules"]; ok {
			if arr, ok := v.([]interface{}); ok {
				for _, x := range arr {
					if mp, ok := x.(map[string]interface{}); ok {
						rules = append(rules, DegradationRuleConfig{
							Route:          getString(mp, "route"),
							ErrorThreshold: getFloat(mp, "errorThreshold"),
							WindowSize:     getInt(mp, "windowSize"),
							FallbackData:   getString(mp, "fallbackData"),
							CoolDown:       getString(mp, "coolDown"),
						})
					}
				}
			}
		}
		return NewDegradationManager(rules), nil
	})
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return float64(x)
		}
	}
	return 0
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case int:
			return x
		case float64:
			return int(x)
		}
	}
	return 0
}
