package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/streasure/sgate/monitor"
	tlog "github.com/streasure/treasure-slog"
)

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

// messageRateTracker 滚动窗口消息速率计算器
// 在 Stats() 被调用时计算最近 window 内的每秒消息数
type messageRateTracker struct {
	mu        sync.Mutex
	samples   []rateSample
	window    time.Duration
	lastCount int64
	lastTime  time.Time
}

type rateSample struct {
	timestamp time.Time
	count     int64
}

func newMessageRateTracker(window time.Duration) *messageRateTracker {
	return &messageRateTracker{
		samples: make([]rateSample, 0, 64),
		window:  window,
	}
}

// record 记录当前消息总数快照（由 Stats() 调用）
func (r *messageRateTracker) record(now time.Time, currentCount int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.samples = append(r.samples, rateSample{timestamp: now, count: currentCount})

	// 清理过期数据
	cutoff := now.Add(-r.window)
	startIdx := 0
	for i, s := range r.samples {
		if s.timestamp.After(cutoff) {
			startIdx = i
			break
		}
	}
	r.samples = r.samples[startIdx:]
}

// rate 计算每秒消息数
func (r *messageRateTracker) rate() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.samples) < 2 {
		return 0
	}
	first := r.samples[0]
	last := r.samples[len(r.samples)-1]
	duration := last.timestamp.Sub(first.timestamp).Seconds()
	if duration <= 0 {
		return 0
	}
	return float64(last.count-first.count) / duration
}

// Stats 实现 monitor.StatsProvider 接口，返回当前网关运行时统计快照。
// 每次 Prometheus scrape 时由 monitor.PrometheusExporter 调用。
func (g *Gateway) Stats() monitor.GatewayStats {
	var s monitor.GatewayStats

	// --- 连接指标 ---
	s.ConnectionsTotal = uint64(g.metrics.GetConnectionsTotal())
	s.ConnectionsActive = g.metrics.GetConnectionsActive()

	// --- 消息指标 ---
	s.MessagesReceived = g.messagesReceived.Load()
	s.MessagesForwarded = g.messagesForwarded.Load()
	s.MessagesPushed = g.messagesPushedToClient.Load()
	s.MessagesDroppedOverload = g.messagesDroppedOverload.Load()
	s.MessagesDroppedFull = g.messagesDroppedFull.Load()
	s.MessagesDroppedNoLogic = g.messagesDroppedNoLogic.Load()
	s.MessagesDroppedNoLogicNotConn = g.messagesDroppedNoLogicNotConnected.Load()
	s.MessagesPushDroppedNoConn = g.messagesPushDroppedNoConn.Load()
	s.MessagesDroppedBlacklist = g.messagesDroppedBlacklist.Load()
	s.MessagesDroppedRateLimit = g.messagesDroppedRateLimit.Load()
	s.MessagesDroppedWAF = g.messagesDroppedWAF.Load()
	s.MessagesDroppedCircuit = g.messagesDroppedCircuit.Load()
	s.MessagesDroppedIntegrity = g.messagesDroppedIntegrity.Load()
	s.MessagesDroppedFilterChain = g.messagesDroppedFilterChain.Load()
	s.MessagesProcessed = g.metrics.GetMessagesProcessed()
	s.MessagesFailed = g.metrics.GetMessagesFailed()

	// 消息速率（滚动窗口估算）
	now := time.Now()
	if g.msgRate != nil {
		g.msgRate.record(now, s.MessagesReceived)
		s.MessagesPerSecond = g.msgRate.rate()
	}

	// --- 延迟指标 ---
	if g.latencyTracker != nil {
		ls := g.latencyTracker.GetStats()
		s.LatencyP50Us = ls.P50.Microseconds()
		s.LatencyP95Us = ls.P95.Microseconds()
		s.LatencyP99Us = ls.P99.Microseconds()
		s.LatencyMaxUs = ls.Max.Microseconds()
	}

	// --- 安全防护指标 ---
	if g.waf != nil {
		s.WAFBlocked = g.waf.GetBlockedCount()
	}
	if g.circuitBreakerMgr != nil {
		s.CircuitBreakerTripped = g.circuitBreakerMgr.GetTrippedCount()
	}
	if g.degradation != nil {
		s.DegradationTriggered = g.degradation.GetTriggeredCount()
	}

	// --- 集群指标 ---
	if g.cluster != nil && g.cluster.IsLeader() {
		s.IsLeader = 1
	}

	// --- 灰度 / 镜像 ---
	if g.canaryFilter != nil {
		s.CanaryHit = g.canaryFilter.GetHitCount()
	}
	if g.trafficMirror != nil {
		s.TrafficMirrorForwarded, s.TrafficMirrorDropped = g.trafficMirror.Stats()
	}

	// --- 告警 ---
	if g.alertWebhook != nil {
		s.AlertSent, s.AlertDropped = g.alertWebhook.Stats()
	}

	// --- 系统指标 ---
	if g.overloadProtector != nil {
		s.CPUUsagePercent, s.MemUsagePercent, _, _ = g.overloadProtector.Stats()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s.Goroutines = runtime.NumGoroutine()
	s.MemoryAlloc = m.Alloc
	s.MemorySys = m.Sys
	s.GCCount = m.NumGC

	return s
}

// statsPayload 是 /stats 端点返回的 JSON 结构
type statsPayload struct {
	Received              int64   `json:"received"`
	Forwarded             int64   `json:"forwarded"`
	DroppedOverload       int64   `json:"droppedOverload"`
	DroppedFull           int64   `json:"droppedFull"`
	DroppedNoLogic        int64   `json:"droppedNoLogic"`
	DroppedNoLogicNotConn int64   `json:"droppedNoLogicNotConnected"`
	DroppedBlacklist      int64   `json:"droppedBlacklist"`
	DroppedRateLimit      int64   `json:"droppedRateLimit"`
	DroppedWAF            int64   `json:"droppedWAF"`
	DroppedCircuit        int64   `json:"droppedCircuit"`
	DroppedIntegrity      int64   `json:"droppedIntegrity"`
	DroppedFilterChain    int64   `json:"droppedFilterChain"`
	DroppedTotal          int64   `json:"droppedTotal"`
	PushedToClient        int64   `json:"pushedToClient"`
	PushDroppedNoConn     int64   `json:"pushDroppedNoConn"`
	Overloaded            bool    `json:"overloaded"`
	CPUPercent            float64 `json:"cpuPercent"`
	MemPercent            float64 `json:"memPercent"`
	OverloadDropped       int64   `json:"overloadDropped"`
	ActiveConnections     int64   `json:"activeConnections"`
	WAFBlocked            int64   `json:"wafBlocked"`
	IsLeader              bool    `json:"isLeader"`
	NodeID                string  `json:"nodeID,omitempty"`
	LatencyP50Us          int64   `json:"latencyP50Us"`
	LatencyP95Us          int64   `json:"latencyP95Us"`
	LatencyP99Us          int64   `json:"latencyP99Us"`
	LatencyMaxUs          int64   `json:"latencyMaxUs"`
}

// StartStatsServer 启动统计 HTTP 服务，暴露 /stats /health /ready /live
// 此服务与 Prometheus /metrics 独立，始终启动（用于 K8s probe 和运维查看）
func (g *Gateway) StartStatsServer(addr string) {
	if addr == "" {
		addr = ":9091"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		cpuPct, memPct, overloaded, dropped := g.overloadProtector.Stats()
		dropOverload := g.messagesDroppedOverload.Load()
		dropFull := g.messagesDroppedFull.Load()
		dropNoLogic := g.messagesDroppedNoLogic.Load()
		dropNoLogicNotConn := g.messagesDroppedNoLogicNotConnected.Load()
		dropBlacklist := g.messagesDroppedBlacklist.Load()
		dropRateLimit := g.messagesDroppedRateLimit.Load()
		dropWAF := g.messagesDroppedWAF.Load()
		dropCircuit := g.messagesDroppedCircuit.Load()
		dropIntegrity := g.messagesDroppedIntegrity.Load()
		dropFilterChain := g.messagesDroppedFilterChain.Load()
		stats := statsPayload{
			Received:              g.messagesReceived.Load(),
			Forwarded:             g.messagesForwarded.Load(),
			DroppedOverload:       dropOverload,
			DroppedFull:           dropFull,
			DroppedNoLogic:        dropNoLogic,
			DroppedNoLogicNotConn: dropNoLogicNotConn,
			DroppedBlacklist:      dropBlacklist,
			DroppedRateLimit:      dropRateLimit,
			DroppedWAF:            dropWAF,
			DroppedCircuit:        dropCircuit,
			DroppedIntegrity:      dropIntegrity,
			DroppedFilterChain:    dropFilterChain,
			DroppedTotal:          dropOverload + dropFull + dropNoLogic + dropNoLogicNotConn + dropBlacklist + dropRateLimit + dropWAF + dropCircuit + dropIntegrity + dropFilterChain,
			PushedToClient:        g.messagesPushedToClient.Load(),
			PushDroppedNoConn:     g.messagesPushDroppedNoConn.Load(),
			Overloaded:            overloaded,
			CPUPercent:            cpuPct,
			MemPercent:            memPct,
			OverloadDropped:       dropped,
			ActiveConnections:     g.metrics.GetConnectionsActive(),
		}
		if g.waf != nil {
			stats.WAFBlocked = g.waf.GetBlockedCount()
		}
		if g.cluster != nil {
			stats.IsLeader = g.cluster.IsLeader()
			stats.NodeID = g.cluster.GetNodeID()
		}
		if g.latencyTracker != nil {
			latStats := g.latencyTracker.GetStats()
			stats.LatencyP50Us = latStats.P50.Microseconds()
			stats.LatencyP95Us = latStats.P95.Microseconds()
			stats.LatencyP99Us = latStats.P99.Microseconds()
			stats.LatencyMaxUs = latStats.Max.Microseconds()
		}
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(stats)
		w.Write(data)
	})
	// 健康检查 HTTP 端点（与 stats 同端口，便于 K8s probe 对接）
	mux.HandleFunc("/health", g.ServeHealthHTTP)
	mux.HandleFunc("/ready", g.ServeHealthHTTP)
	mux.HandleFunc("/live", g.ServeHealthHTTP)
	srv := &http.Server{Addr: addr, Handler: mux}
	g.statsServer = srv
	go func() {
		tlog.Info("starting stats server", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			tlog.Error("stats server failed", "error", err)
		}
	}()
}

// StopStatsServer 停止统计 HTTP 服务
func (g *Gateway) StopStatsServer() {
	if g.statsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		g.statsServer.Shutdown(ctx)
	}
}
