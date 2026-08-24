package monitor

// ============================================================================
// monitor 包 —— sgate 可插拔监控导出模块
// ----------------------------------------------------------------------------
// 设计目标：
//   1. 独立 import：不依赖 gateway 包，任何 Go 项目都可 import 使用
//   2. 可插拔：Gateway 通过 monitoring.prometheus.enabled 配置开关决定是否启动
//   3. 零侵入：关闭时 sgate 单体正常运行，不暴露 /metrics 端点
//
// 使用方式：
//   import "github.com/streasure/sgate/monitor"
//
//   exporter := monitor.NewPrometheusExporter(gateway, ":9090", "/metrics")
//   exporter.Start()   // 启动 HTTP 指标服务
//   exporter.Stop()    // 优雅关闭
//
// Gateway 只需实现 StatsProvider 接口（返回 GatewayStats 快照），
// 即可被 PrometheusExporter 采集并输出 Prometheus 文本格式指标。
// ============================================================================

// GatewayStats 网关运行时统计快照
// 由实现了 StatsProvider 接口的对象填充后传给 PrometheusExporter 渲染。
// 所有字段均为值类型（非指针），保证快照的一致性。
type GatewayStats struct {
	// --- 连接指标 ---
	ConnectionsTotal   uint64 // 连接累计总数
	ConnectionsActive  int64  // 当前活跃连接数
	ConnectionsCreated uint64 // 已创建连接数
	ConnectionsClosed  uint64 // 已关闭连接数

	// --- 消息指标 ---
	MessagesReceived              int64   // 接收消息总数
	MessagesForwarded             int64   // 转发到 logic server 的消息数
	MessagesPushed                int64   // 推送到客户端的消息数
	MessagesPerSecond             float64 // 每秒消息数（滚动窗口估算）
	MessagesDroppedOverload       int64   // 过载丢弃数
	MessagesDroppedFull           int64   // 队列满丢弃数
	MessagesDroppedNoLogic        int64   // 无 logic server 丢弃数
	MessagesDroppedNoLogicNotConn int64   // 无 logic server 且无连接丢弃数
	MessagesPushDroppedNoConn     int64   // 推送时无客户端连接丢弃数
	MessagesProcessed             int64   // 处理成功消息数
	MessagesFailed                int64   // 处理失败消息数
	// 细分丢弃原因
	MessagesDroppedBlacklist   int64 // 黑名单/白名单拦截
	MessagesDroppedRateLimit   int64 // 限流拦截
	MessagesDroppedWAF         int64 // WAF 拦截
	MessagesDroppedCircuit     int64 // 熔断器拦截
	MessagesDroppedIntegrity   int64 // 完整性校验失败
	MessagesDroppedFilterChain int64 // filter chain 中止

	// --- 延迟指标（微秒）---
	LatencyP50Us        int64   // P50 延迟
	LatencyP95Us        int64   // P95 延迟
	LatencyP99Us        int64   // P99 延迟
	LatencyMaxUs        int64   // 最大延迟
	ProcessingTimeAvgUs float64 // 平均处理时间（微秒）

	// --- 安全防护指标 ---
	WAFBlocked            int64  // WAF 拦截数
	RateLimitHits         uint64 // 限流命中数
	RateLimitBlocked      uint64 // 限流拦截数
	CircuitBreakerTripped int64  // 熔断触发次数
	DegradationTriggered  int64  // 降级触发次数

	// --- 集群指标 ---
	IsLeader int64 // 是否为 Leader（1=是, 0=否）

	// --- 灰度 / 镜像 ---
	CanaryHit              int64 // 灰度命中数
	TrafficMirrorForwarded int64 // 镜像转发数
	TrafficMirrorDropped   int64 // 镜像丢弃数

	// --- 告警 ---
	AlertSent    int64 // 告警发送数
	AlertDropped int64 // 告警丢弃数

	// --- 系统指标 ---
	CPUUsagePercent float64 // CPU 使用率 %
	MemUsagePercent float64 // 内存使用率 %
	Goroutines      int     // goroutine 数量
	MemoryAlloc     uint64  // 已分配内存（字节）
	MemorySys       uint64  // 系统内存（字节）
	GCCount         uint32  // GC 次数
}

// StatsProvider 由网关实现，返回当前统计快照。
// PrometheusExporter 在每次 /metrics 请求时调用此方法获取最新数据。
type StatsProvider interface {
	Stats() GatewayStats
}
