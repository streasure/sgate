package monitor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// ============================================================================
// PrometheusExporter —— Prometheus 指标导出器
// ----------------------------------------------------------------------------
// 职责：
//   1. 启动 HTTP 服务，暴露 /metrics 端点
//   2. 每次 scrape 时调用 StatsProvider.Stats() 获取快照
//   3. 将快照渲染为 Prometheus 文本格式（text/plain, version=0.0.4）
//
// 线程安全：StatsProvider 的 Stats() 方法由 HTTP handler 调用，
// 必须自行保证并发安全（Gateway 通过 atomic/load 实现）。
//
// 生命周期：
//   exporter := NewPrometheusExporter(provider, ":9090", "/metrics")
//   exporter.Start()  // 阻塞前启动 HTTP 服务（非阻塞）
//   ... sgate 运行 ...
//   exporter.Stop()   // 优雅关闭
// ============================================================================

// PrometheusExporter Prometheus 指标导出器
type PrometheusExporter struct {
	provider StatsProvider
	addr     string
	path     string
	server   *http.Server
}

// NewPrometheusExporter 创建 Prometheus 指标导出器
//
//	provider: 实现了 StatsProvider 接口的对象（如 *Gateway）
//	addr:     监听地址（如 ":9090"）
//	path:     指标路径（如 "/metrics"）
func NewPrometheusExporter(provider StatsProvider, addr, path string) *PrometheusExporter {
	if addr == "" {
		addr = ":9090"
	}
	if path == "" {
		path = "/metrics"
	}
	return &PrometheusExporter{
		provider: provider,
		addr:     addr,
		path:     path,
	}
}

// Start 启动 HTTP 指标服务（非阻塞，在独立 goroutine 中运行）
func (e *PrometheusExporter) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc(e.path, e.serveHTTP)

	e.server = &http.Server{Addr: e.addr, Handler: mux}

	go func() {
		tlog.Info("启动 Prometheus 指标服务", "addr", e.addr, "path", e.path)
		if err := e.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			tlog.Error("Prometheus 指标服务启动失败", "error", err)
		}
	}()
}

// Stop 优雅关闭 HTTP 指标服务
func (e *PrometheusExporter) Stop() {
	if e.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		e.server.Shutdown(ctx)
	}
}

// serveHTTP 处理 /metrics 请求，输出 Prometheus 文本格式指标
func (e *PrometheusExporter) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	s := e.provider.Stats()
	fmt.Fprint(w, renderPrometheusText(s))
}

// renderPrometheusText 将 GatewayStats 渲染为 Prometheus 文本格式
// 输出全部 24 个 Grafana 看板引用指标 + 基础系统指标
func renderPrometheusText(s GatewayStats) string {
	var b strings.Builder

	// --- Grafana 看板引用的 24 个指标 ---

	writeMetric(&b, "sgate_messages_received_total", "counter",
		"Total number of received messages", s.MessagesReceived)
	writeMetric(&b, "sgate_messages_forwarded_total", "counter",
		"Total number of forwarded messages", s.MessagesForwarded)
	writeMetric(&b, "sgate_messages_pushed_total", "counter",
		"Total number of pushed messages", s.MessagesPushed)
	writeMetric(&b, "sgate_messages_dropped_overload_total", "counter",
		"Messages dropped due to overload", s.MessagesDroppedOverload)
	writeMetric(&b, "sgate_messages_dropped_full_total", "counter",
		"Messages dropped due to queue full", s.MessagesDroppedFull)
	writeMetric(&b, "sgate_messages_dropped_no_logic_total", "counter",
		"Messages dropped due to no logic server", s.MessagesDroppedNoLogic+s.MessagesDroppedNoLogicNotConn)
	writeMetric(&b, "sgate_messages_dropped_no_conn_total", "counter",
		"Push messages dropped due to no client connection", s.MessagesPushDroppedNoConn)
	writeMetric(&b, "sgate_messages_dropped_blacklist_total", "counter",
		"Messages dropped by whitelist/blacklist", s.MessagesDroppedBlacklist)
	writeMetric(&b, "sgate_messages_dropped_rate_limit_total", "counter",
		"Messages dropped by rate limiter", s.MessagesDroppedRateLimit)
	writeMetric(&b, "sgate_messages_dropped_waf_total", "counter",
		"Messages dropped by WAF", s.MessagesDroppedWAF)
	writeMetric(&b, "sgate_messages_dropped_circuit_total", "counter",
		"Messages dropped by circuit breaker", s.MessagesDroppedCircuit)
	writeMetric(&b, "sgate_messages_dropped_integrity_total", "counter",
		"Messages dropped by integrity check", s.MessagesDroppedIntegrity)
	writeMetric(&b, "sgate_messages_dropped_filter_chain_total", "counter",
		"Messages dropped by filter chain", s.MessagesDroppedFilterChain)

	writeMetricFloat(&b, "sgate_latency_p50_us", "gauge",
		"P50 latency in microseconds", s.LatencyP50Us)
	writeMetricFloat(&b, "sgate_latency_p95_us", "gauge",
		"P95 latency in microseconds", s.LatencyP95Us)
	writeMetricFloat(&b, "sgate_latency_p99_us", "gauge",
		"P99 latency in microseconds", s.LatencyP99Us)
	writeMetricFloat(&b, "sgate_latency_max_us", "gauge",
		"Max latency in microseconds", s.LatencyMaxUs)

	writeMetricFloat(&b, "sgate_cpu_percent", "gauge",
		"CPU usage percentage", s.CPUUsagePercent)
	writeMetricFloat(&b, "sgate_mem_percent", "gauge",
		"Memory usage percentage", s.MemUsagePercent)

	writeMetric(&b, "sgate_waf_blocked_total", "counter",
		"Total WAF blocked requests", s.WAFBlocked)
	writeMetric(&b, "sgate_active_connections", "gauge",
		"Number of active connections", s.ConnectionsActive)
	writeMetric(&b, "sgate_is_leader", "gauge",
		"Whether this node is cluster leader (1=yes, 0=no)", s.IsLeader)
	writeMetric(&b, "sgate_circuit_breaker_tripped_total", "counter",
		"Total circuit breaker trips", s.CircuitBreakerTripped)
	writeMetric(&b, "sgate_degradation_triggered_total", "counter",
		"Total degradation triggers", s.DegradationTriggered)
	writeMetric(&b, "sgate_canary_hit_total", "counter",
		"Total canary release hits", s.CanaryHit)
	writeMetric(&b, "sgate_traffic_mirror_forwarded_total", "counter",
		"Total mirrored messages forwarded", s.TrafficMirrorForwarded)
	writeMetric(&b, "sgate_traffic_mirror_dropped_total", "counter",
		"Total mirrored messages dropped", s.TrafficMirrorDropped)
	writeMetric(&b, "sgate_alert_sent_total", "counter",
		"Total alerts sent", s.AlertSent)
	writeMetric(&b, "sgate_alert_dropped_total", "counter",
		"Total alerts dropped", s.AlertDropped)

	// --- 基础系统指标（补充，便于排障）---

	writeMetric(&b, "sgate_connections_total", "counter",
		"Total number of connections", s.ConnectionsTotal)
	writeMetric(&b, "sgate_connections_created", "counter",
		"Total number of created connections", s.ConnectionsCreated)
	writeMetric(&b, "sgate_connections_closed", "counter",
		"Total number of closed connections", s.ConnectionsClosed)
	writeMetricFloat(&b, "sgate_messages_per_second", "gauge",
		"Number of messages per second", s.MessagesPerSecond)
	writeMetricFloat(&b, "sgate_processing_time_average", "gauge",
		"Average processing time in microseconds", s.ProcessingTimeAvgUs)
	writeMetric(&b, "sgate_rate_limit_hits_total", "counter",
		"Total number of rate limit hits", s.RateLimitHits)
	writeMetric(&b, "sgate_rate_limit_blocked_total", "counter",
		"Total number of rate limit blocks", s.RateLimitBlocked)
	writeMetric(&b, "sgate_messages_processed_total", "counter",
		"Total number of processed messages", s.MessagesProcessed)
	writeMetric(&b, "sgate_messages_failed_total", "counter",
		"Total number of failed messages", s.MessagesFailed)
	writeMetric(&b, "sgate_goroutines", "gauge",
		"Number of goroutines", s.Goroutines)
	writeMetric(&b, "sgate_memory_alloc_bytes", "gauge",
		"Allocated memory in bytes", s.MemoryAlloc)
	writeMetric(&b, "sgate_memory_sys_bytes", "gauge",
		"System memory in bytes", s.MemorySys)
	writeMetric(&b, "sgate_gc_count", "counter",
		"Total number of garbage collections", s.GCCount)

	return b.String()
}

// writeMetric 写入一个整数型 Prometheus 指标
func writeMetric(b *strings.Builder, name, typ, help string, value interface{}) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	fmt.Fprintf(b, "%s %v\n\n", name, value)
}

// writeMetricFloat 写入一个浮点型 Prometheus 指标（保留 2 位小数）
// 注意：value 会被转换为 float64 再格式化，避免 int64 用 %.2f 输出 %!f 错误
func writeMetricFloat(b *strings.Builder, name, typ, help string, value interface{}) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	fmt.Fprintf(b, "%s %.2f\n\n", name, toFloat64(value))
}

// toFloat64 把 interface{} 转为 float64（支持 int64/int32/uint64/float64 等）
func toFloat64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case uint64:
		return float64(x)
	case uint32:
		return float64(x)
	case uint:
		return float64(x)
	default:
		return 0
	}
}
