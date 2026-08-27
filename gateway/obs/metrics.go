package obs

// ============================================================================
// gateway/metrics.go —— 网关统计与监控对接
// ----------------------------------------------------------------------------
// Prometheus 指标导出逻辑已抽到独立可 import 的 monitor 包：
//   github.com/streasure/sgate/monitor
//
// Gateway 通过实现 monitor.StatsProvider 接口（Stats() 方法）向 monitor
// 提供运行时快照，monitor.PrometheusExporter 负责渲染 /metrics 端点。
//
// 当 monitoring.prometheus.enabled=false 时，Gateway 不创建 exporter，
// sgate 单体正常运行（仅不暴露 /metrics 端点）。
// ============================================================================
