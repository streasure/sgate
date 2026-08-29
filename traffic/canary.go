package traffic

import (
	"hash/fnv"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/util"
	"github.com/streasure/sgate/internal/config"
)

// CanaryFilter 灰度发布过滤器
// 支持：百分比、Header、用户 ID 三种维度灰度
type CanaryFilter struct {
	mu          sync.RWMutex
	percent     int                 // 0-100，灰度比例
	headers     map[string]string   // Header 匹配规则（key=value 时进入灰度）
	userIDs     map[string]struct{} // 指定用户灰度
	targetRoute string              // 灰度命中后切换到的上游路由
	hitCount    atomic.Int64        // 灰度命中累计次数
}

// NewCanaryFilter 构造灰度过滤器
func NewCanaryFilter(cfg config.CanaryConfig) *CanaryFilter {
	f := &CanaryFilter{
		percent:     cfg.Percent,
		headers:     cfg.Headers,
		userIDs:     make(map[string]struct{}),
		targetRoute: cfg.TargetRoute,
	}
	for _, id := range cfg.UserIDs {
		f.userIDs[id] = struct{}{}
	}
	return f
}

func (f *CanaryFilter) Name() string       { return "canary" }
func (f *CanaryFilter) Phase() types.FilterPhase { return types.PhasePostAuth }
func (f *CanaryFilter) Priority() int      { return 200 }

// Process 判断是否命中灰度
func (f *CanaryFilter) Process(fc *types.FilterContext) (bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	hit := false

	// 1) 用户 ID 灰度
	if fc.UserUUID != "" && len(f.userIDs) > 0 {
		if _, ok := f.userIDs[fc.UserUUID]; ok {
			hit = true
		}
	}

	// 2) Header 灰度
	if !hit && len(f.headers) > 0 {
		for k, v := range f.headers {
			if fc.Metadata[k] == v {
				hit = true
				break
			}
		}
	}

	// 3) 百分比灰度（按 connectionID 哈希，保证同一连接稳定分流）
	if !hit && f.percent > 0 {
		h := fnv.New32a()
		h.Write([]byte(fc.ConnectionID))
		if int(h.Sum32()%100) < f.percent {
			hit = true
		}
	}

	if hit {
		fc.Metadata["canary"] = "true"
		f.hitCount.Add(1)
		if f.targetRoute != "" {
			fc.Route = f.targetRoute
		}
	}
	return true, nil
}

// GetHitCount 返回累计灰度命中次数
func (f *CanaryFilter) GetHitCount() int64 {
	return f.hitCount.Load()
}

// UpdateConfig 动态更新灰度配置
func (f *CanaryFilter) UpdateConfig(cfg config.CanaryConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.percent = cfg.Percent
	f.headers = cfg.Headers
	f.userIDs = make(map[string]struct{})
	for _, id := range cfg.UserIDs {
		f.userIDs[id] = struct{}{}
	}
	f.targetRoute = cfg.TargetRoute
}

func init() {
	types.RegisterFilter("canary", func(cfg map[string]interface{}) (types.Filter, error) {
		c := config.CanaryConfig{
			Percent:     util.GetInt(cfg, "percent"),
			TargetRoute: util.GetString(cfg, "targetRoute"),
		}
		if v, ok := cfg["headers"]; ok {
			if mp, ok := v.(map[string]interface{}); ok {
				c.Headers = make(map[string]string)
				for k, v := range mp {
					if s, ok := v.(string); ok {
						c.Headers[k] = s
					}
				}
			}
		}
		if v, ok := cfg["userIDs"]; ok {
			if arr, ok := v.([]interface{}); ok {
				for _, x := range arr {
					if s, ok := x.(string); ok {
						c.UserIDs = append(c.UserIDs, s)
					}
				}
			}
		}
		return NewCanaryFilter(c), nil
	})
}

// 防止 rand 未使用告警
var _ = rand.Intn
