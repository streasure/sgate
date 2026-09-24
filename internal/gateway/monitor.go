package gateway

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	json "github.com/bytedance/sonic"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/util/prometheus"
	"github.com/streasure/util/tlog"
	"gopkg.in/yaml.v3"
)

// ===== 监控 / 健康检查 / 配置热更新（合并自 stats.go, health.go, config_watcher.go）=====

type rateSample struct {
	timestamp time.Time
	count     int64
}

type messageRateTracker struct {
	mu        sync.Mutex
	samples   []rateSample
	window    time.Duration
	lastCount int64
	lastTime  time.Time
}

func newMessageRateTracker(window time.Duration) *messageRateTracker {
	return &messageRateTracker{
		samples: make([]rateSample, 0, 64),
		window:  window,
	}
}

func (r *messageRateTracker) record(now time.Time, currentCount int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.samples = append(r.samples, rateSample{timestamp: now, count: currentCount})

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

func (g *Gateway) Stats() prometheus.Stats {
	var s prometheus.Stats

	s.ConnectionsTotal = uint64(g.connectionsTotal.Load())
	s.ConnectionsActive = g.connectionsActive.Load()

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
	s.MessagesDroppedAuth = g.messagesDroppedAuth.Load()
	s.MessagesProcessed = g.messagesProcessed.Load()
	s.MessagesFailed = g.messagesFailed.Load()

	now := time.Now()
	if g.msgRate != nil {
		g.msgRate.record(now, s.MessagesReceived)
		s.MessagesPerSecond = g.msgRate.rate()
	}

	if g.latencyTracker != nil {
		ls := g.latencyTracker.GetStats()
		s.LatencyP50Us = ls.P50.Microseconds()
		s.LatencyP95Us = ls.P95.Microseconds()
		s.LatencyP99Us = ls.P99.Microseconds()
		s.LatencyMaxUs = ls.Max.Microseconds()
	}

	if g.waf != nil {
		s.WAFBlocked = g.waf.GetBlockedCount()
	}
	if g.circuitBreakerMgr != nil {
		s.CircuitBreakerTripped = g.circuitBreakerMgr.GetTrippedCount()
	}
	if g.degradation != nil {
		s.DegradationTriggered = g.degradation.GetTriggeredCount()
	}

	if g.cluster != nil && g.cluster.IsLeader() {
		s.IsLeader = 1
	}

	if g.canaryFilter != nil {
		s.CanaryHit = g.canaryFilter.GetHitCount()
	}
	if g.trafficMirror != nil {
		s.TrafficMirrorForwarded, s.TrafficMirrorDropped = g.trafficMirror.Stats()
	}

	if g.alertWebhook != nil {
		s.AlertSent, s.AlertDropped = g.alertWebhook.Stats()
	}

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
	DroppedAuth           int64   `json:"droppedAuth"`
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
	// P1: 连接生命周期指标
	AvgConnectionDurationMs float64 `json:"avgConnectionDurationMs"`     // 平均连接存活时长（毫秒）
	ConnectionDurationP50Ms float64 `json:"connectionDurationP50Ms"`     // 连接存活时长 P50（毫秒）
	ConnectionDurationP95Ms float64 `json:"connectionDurationP95Ms"`     // 连接存活时长 P95（毫秒）
	ConnectionDurationP99Ms float64 `json:"connectionDurationP99Ms"`     // 连接存活时长 P99（毫秒）
	IPConnectionCount       int     `json:"ipConnectionCount,omitempty"` // 当前 IP 连接数（调试用）
}

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
		dropAuth := g.messagesDroppedAuth.Load()
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
			DroppedAuth:           dropAuth,
			DroppedTotal:          dropOverload + dropFull + dropNoLogic + dropNoLogicNotConn + dropBlacklist + dropRateLimit + dropWAF + dropCircuit + dropIntegrity + dropFilterChain + dropAuth,
			PushedToClient:        g.messagesPushedToClient.Load(),
			PushDroppedNoConn:     g.messagesPushDroppedNoConn.Load(),
			Overloaded:            overloaded,
			CPUPercent:            cpuPct,
			MemPercent:            memPct,
			OverloadDropped:       dropped,
			ActiveConnections:     g.connectionsActive.Load(),
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
		// P1: 连接生命周期指标
		if count := g.connectionDurationCount.Load(); count > 0 {
			stats.AvgConnectionDurationMs = float64(g.connectionDurationSum.Load()) / float64(count)
		}
		if g.connectionDurationTracker != nil {
			connDurStats := g.connectionDurationTracker.GetStats()
			stats.ConnectionDurationP50Ms = connDurStats.P50.Seconds() * 1000
			stats.ConnectionDurationP95Ms = connDurStats.P95.Seconds() * 1000
			stats.ConnectionDurationP99Ms = connDurStats.P99.Seconds() * 1000
		}
		w.Header().Set("Content-Type", "application/json")
		data, err := json.Marshal(stats)
		if err != nil {
			http.Error(w, "failed to encode stats", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/health", g.ServeHealthHTTP)
	mux.HandleFunc("/ready", g.ServeHealthHTTP)
	mux.HandleFunc("/live", g.ServeHealthHTTP)
	srv := &http.Server{Addr: addr, Handler: mux}
	g.statsServer = srv
	go func() {
		tlog.Info(context.TODO(), "starting stats server addr=%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			tlog.Error(context.TODO(), "stats server failed error=%v", err)
		}
	}()
}

func (g *Gateway) StopStatsServer() {
	if g.statsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		g.statsServer.Shutdown(ctx)
	}
}

var (
	startTime = time.Now()
)

func (g *Gateway) HealthCheck() *obs.HealthStatus {
	status := &obs.HealthStatus{
		Status:    "healthy",
		Timestamp: time.Now(),
		Version:   BuildVersion,
		Uptime:    time.Since(startTime),
		Checks:    make(map[string]obs.Check),
	}

	checks := []struct {
		name string
		fn   func() obs.Check
	}{
		{"gateway", g.checkGateway},
		{"overload_protector", g.checkOverloadProtector},
		{"logic_server", g.checkWorkerPool},
	}

	for _, check := range checks {
		status.Checks[check.name] = check.fn()
	}

	for _, check := range status.Checks {
		if check.Status == "fail" {
			status.Status = "unhealthy"
			break
		}
		if check.Status == "warn" && status.Status == "healthy" {
			status.Status = "degraded"
		}
	}

	status.Metrics = g.collectMetrics()

	return status
}

func (g *Gateway) ReadinessCheck() *obs.ReadinessStatus {
	status := &obs.ReadinessStatus{
		Ready:     true,
		Timestamp: time.Now(),
	}

	if time.Since(startTime) < 5*time.Second {
		status.Ready = false
		status.Reason = "Service is still initializing"
		return status
	}

	if !g.isLogicConnected() {
		status.Ready = false
		status.Reason = "Logic server not connected"
		return status
	}

	return status
}

func (g *Gateway) LivenessCheck() *obs.LivenessStatus {
	return &obs.LivenessStatus{
		Alive:     true,
		Timestamp: time.Now(),
	}
}

func (g *Gateway) checkGateway() obs.Check {
	if g.connectionManager == nil {
		return obs.Check{Status: "fail", Message: "connection manager not initialized"}
	}
	if g.overloadProtector == nil {
		return obs.Check{Status: "fail", Message: "overload protector not initialized"}
	}
	if g.overloadProtector.IsOverloaded() {
		return obs.Check{Status: "warn", Message: "gateway overloaded"}
	}
	return obs.Check{
		Status:  "pass",
		Message: "Gateway is running",
	}
}

func (g *Gateway) checkOverloadProtector() obs.Check {
	if g.overloadProtector == nil {
		return obs.Check{Status: "fail", Message: "overload protector not initialized"}
	}
	cpuPct, memPct, overloaded, dropped := g.overloadProtector.Stats()
	msg := fmt.Sprintf("overload protector active (cpu=%.1f%%, mem=%.1f%%, overloaded=%v, dropped=%d)", cpuPct, memPct, overloaded, dropped)
	status := "pass"
	if overloaded {
		status = "warn"
	}
	return obs.Check{Status: status, Message: msg}
}

func (g *Gateway) checkWorkerPool() obs.Check {
	if !g.isLogicConnected() {
		return obs.Check{
			Status:  "fail",
			Message: "Logic server not connected",
		}
	}
	return obs.Check{
		Status:  "pass",
		Message: "Logic server connected",
	}
}

func (g *Gateway) collectMetrics() obs.HealthMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return obs.HealthMetrics{
		Connections:    g.connectionManager.GetConnectionCount(),
		Goroutines:     runtime.NumGoroutine(),
		MemoryAlloc:    m.Alloc / 1024 / 1024,
		MemorySys:      m.Sys / 1024 / 1024,
		GCCount:        m.NumGC,
		MessagesPerSec: g.msgRate.rate(),
	}
}

func (g *Gateway) ServeHealthHTTP(w http.ResponseWriter, r *http.Request) {
	var response interface{}
	var statusCode int

	switch r.URL.Path {
	case "/health":
		response = g.HealthCheck()
		statusCode = http.StatusOK
		if resp, ok := response.(*obs.HealthStatus); ok && resp.Status == "unhealthy" {
			statusCode = http.StatusServiceUnavailable
		}
	case "/ready":
		response = g.ReadinessCheck()
		statusCode = http.StatusOK
		if resp, ok := response.(*obs.ReadinessStatus); ok && !resp.Ready {
			statusCode = http.StatusServiceUnavailable
		}
	case "/live":
		response = g.LivenessCheck()
		statusCode = http.StatusOK
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	data, _ := json.Marshal(response)
	w.Write(data)
}

// startConfigCenterWatcher 启动配置中心监听并桥接到现有 handleConfigUpdate
func (g *Gateway) startConfigCenterWatcher() {
	if g.configCenter == nil {
		return
	}
	ch, err := g.configCenter.Watch(g.ctx)
	if err != nil {
		tlog.Error(context.TODO(), "config center watch failed error=%v", err)
		return
	}
	go func() {
		for yamlBytes := range ch {
			if len(yamlBytes) == 0 {
				continue
			}
			currentCfg := g.cfg.Load().(*config.Config)
			newCfg := *currentCfg
			if err := yaml.Unmarshal(yamlBytes, &newCfg); err != nil {
				tlog.Warn(context.TODO(), "config center content parse failed error=%v", err)
				continue
			}
			select {
			case g.configUpdateChan <- &newCfg:
			case <-g.stopChan:
				return
			}
			tlog.Info(context.TODO(), "config updated from config center type=%s",
				g.configCenter.Type())
		}
	}()
}
