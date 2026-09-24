package component

import (
	"sync"

	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/util/uetcd"
)

// 全局运行时资源：各组件在 Init/Start 中写入，Gateway（Order 最后）读取。
// 与 types.FilterChain 同模式，组件之间不持有彼此引用，仅按 Order 协作。

var (
	// security（Order 100）Init 写入。
	globalWhitelistBlacklist *security.WhitelistBlacklist
	globalWAF                *security.WAF
	globalRateLimiter        *security.RateLimiter
	globalJWTAuth            *security.JWTAuthFilter
	globalCircuitBreakerMgr  *security.CircuitBreakerManager

	// observability（Order 200）Init 写入。
	globalTracer         *obs.Tracer
	globalOTelTracer     *obs.OTelTracer
	globalLogSanitizer   *obs.LogSanitizer
	globalLatencyTracker *obs.LatencyTracker

	// traffic（Order 300）Init 写入。
	globalCanaryFilter  *traffic.CanaryFilter
	globalTrafficMirror *traffic.TrafficMirror
	globalDegradation   *traffic.DegradationManager

	// cluster（Order 400）Init/Start 写入。
	globalBalancer         *cluster.Balancer
	globalConfigCenter     cluster.ConfigCenter
	globalAlertWebhook     *cluster.AlertWebhook
	globalDiscovery        *uetcd.Component
	globalGatewayDiscovery *uetcd.Component
	globalLoginDiscovery   *uetcd.Component
	globalCluster          *cluster.Cluster

	globalGatewayEventsMu sync.RWMutex
	globalGatewayEvents   []uetcd.ServiceEvent
)

// WhitelistBlacklist 返回安全组件创建的白黑名单。
func WhitelistBlacklist() *security.WhitelistBlacklist { return globalWhitelistBlacklist }

// WAF 返回安全组件创建的 WAF。
func WAF() *security.WAF { return globalWAF }

// RateLimiter 返回安全组件创建的限流器。
func RateLimiter() *security.RateLimiter { return globalRateLimiter }

// JWTAuth 返回安全组件创建的 JWT 鉴权过滤器。
func JWTAuth() *security.JWTAuthFilter { return globalJWTAuth }

// CircuitBreakerMgr 返回安全组件创建的熔断器管理器。
func CircuitBreakerMgr() *security.CircuitBreakerManager { return globalCircuitBreakerMgr }

// Tracer 返回可观测组件创建的本地追踪器。
func Tracer() *obs.Tracer { return globalTracer }

// OTelTracer 返回可观测组件创建的 OpenTelemetry 追踪器。
func OTelTracer() *obs.OTelTracer { return globalOTelTracer }

// LogSanitizer 返回可观测组件创建的日志脱敏器。
func LogSanitizer() *obs.LogSanitizer { return globalLogSanitizer }

// LatencyTracker 返回可观测组件创建的延迟追踪器。
func LatencyTracker() *obs.LatencyTracker { return globalLatencyTracker }

// CanaryFilter 返回流量组件创建的灰度过滤器。
func CanaryFilter() *traffic.CanaryFilter { return globalCanaryFilter }

// TrafficMirror 返回流量组件创建的流量镜像。
func TrafficMirror() *traffic.TrafficMirror { return globalTrafficMirror }

// Degradation 返回流量组件创建的降级管理器。
func Degradation() *traffic.DegradationManager { return globalDegradation }

// Balancer 返回集群组件创建的负载均衡器。
func Balancer() *cluster.Balancer { return globalBalancer }

// ConfigCenter 返回集群组件创建的配置中心。
func ConfigCenter() cluster.ConfigCenter { return globalConfigCenter }

// AlertWebhook 返回集群组件创建的告警 webhook。
func AlertWebhook() *cluster.AlertWebhook { return globalAlertWebhook }

// Discovery 返回集群组件创建的逻辑服务发现。
func Discovery() *uetcd.Component { return globalDiscovery }

// GatewayDiscovery 返回集群组件创建的网关间发现。
func GatewayDiscovery() *uetcd.Component { return globalGatewayDiscovery }

// LoginDiscovery 返回集群组件创建的 loginserver 发现。
func LoginDiscovery() *uetcd.Component { return globalLoginDiscovery }

// ClusterNode 返回集群组件创建的 Leader 选举节点。
func ClusterNode() *cluster.Cluster { return globalCluster }

// GatewayEvents 返回网关间发现已收到的事件快照。
func GatewayEvents() []uetcd.ServiceEvent {
	globalGatewayEventsMu.RLock()
	defer globalGatewayEventsMu.RUnlock()
	return append([]uetcd.ServiceEvent(nil), globalGatewayEvents...)
}

func setSecurityResources(wb *security.WhitelistBlacklist, waf *security.WAF, rl *security.RateLimiter, jwt *security.JWTAuthFilter, cb *security.CircuitBreakerManager) {
	globalWhitelistBlacklist = wb
	globalWAF = waf
	globalRateLimiter = rl
	globalJWTAuth = jwt
	globalCircuitBreakerMgr = cb
}

func setObservabilityResources(tracer *obs.Tracer, otel *obs.OTelTracer, sanitizer *obs.LogSanitizer, latency *obs.LatencyTracker) {
	globalTracer = tracer
	globalOTelTracer = otel
	globalLogSanitizer = sanitizer
	globalLatencyTracker = latency
}

func setTrafficResources(canary *traffic.CanaryFilter, mirror *traffic.TrafficMirror, degradation *traffic.DegradationManager) {
	globalCanaryFilter = canary
	globalTrafficMirror = mirror
	globalDegradation = degradation
}

func setClusterInitResources(balancer *cluster.Balancer, configCenter cluster.ConfigCenter, alertWebhook *cluster.AlertWebhook) {
	globalBalancer = balancer
	globalConfigCenter = configCenter
	globalAlertWebhook = alertWebhook
}

func setClusterStartResources(discovery, gatewayDiscovery, loginDiscovery *uetcd.Component, clusterNode *cluster.Cluster) {
	globalDiscovery = discovery
	globalGatewayDiscovery = gatewayDiscovery
	globalLoginDiscovery = loginDiscovery
	globalCluster = clusterNode
}

func appendGatewayEvent(event uetcd.ServiceEvent) {
	globalGatewayEventsMu.Lock()
	globalGatewayEvents = append(globalGatewayEvents, event)
	globalGatewayEventsMu.Unlock()
}
