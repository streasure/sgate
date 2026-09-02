package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/streasure/util/prometheus"
	"github.com/streasure/util/tlog"
)

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
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(stats)
		w.Write(data)
	})
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

func (g *Gateway) StopStatsServer() {
	if g.statsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		g.statsServer.Shutdown(ctx)
	}
}
