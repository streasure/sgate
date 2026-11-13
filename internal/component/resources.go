package component

import (
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/types"
)

// Resources 运行时资源容器：组合根（main）创建一次并注入全部组件与 Gateway，
// 组件在 Init（按 Order 顺序执行）写入，Gateway（Order 最后）读取——
// 替代旧的 package 级全局量 + 导出 getter。组件之间不互持彼此引用，
// 只依赖 main 注入的同一容器，仍按 Order 协作。
// 写入全部发生在 Init 阶段（顺序执行），读取发生在其后，无并发竞争。
type Resources struct {
	// FC 过滤器链：main 创建并按配置 LoadByName 加载 SPI 过滤器，
	// 组件 Init 注册内置过滤器，Gateway 执行。
	FC *types.FilterChain

	// security（Order 100）Init 写入。
	WhitelistBlacklist *security.WhitelistBlacklist
	WAF                *security.WAF
	RateLimiter        *security.RateLimiter
	JWTAuth            *security.JWTAuthFilter
	CircuitBreakerMgr  *security.CircuitBreakerManager

	// observability（Order 200）Init 写入。
	Tracer         *obs.Tracer
	OTelTracer     *obs.OTelTracer
	LatencyTracker *obs.LatencyTracker

	// traffic（Order 300）Init 写入。
	CanaryFilter  *traffic.CanaryFilter
	TrafficMirror *traffic.TrafficMirror
	Degradation   *traffic.DegradationManager

	// cluster（Order 400）Init 写入。
	ConfigCenter cluster.ConfigCenter
	AlertWebhook *cluster.AlertWebhook
}

// NewResources 创建资源容器（过滤器链一并创建；
// SPI 注册表在 NewFilterChain 时快照，故须在组件包 init 注册之后调用——
// 组合根 main 天然满足）。
func NewResources() *Resources {
	return &Resources{FC: types.NewFilterChain()}
}
