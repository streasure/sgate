package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

var defaultStartTime = time.Now()

type defaultStats struct {
	ConnectionsTotal  int64  `json:"connectionsTotal"`
	ConnectionsActive int64  `json:"connectionsActive"`
	MessagesReceived  int64  `json:"messagesReceived"`
	MessagesForwarded int64  `json:"messagesForwarded"`
	MessagesPushed    int64  `json:"messagesPushed"`
	MessagesDropped   int64  `json:"messagesDropped"`
	Received          int64  `json:"received"`
	Forwarded         int64  `json:"forwarded"`
	PushedToClient    int64  `json:"pushedToClient"`
	DroppedTotal      int64  `json:"droppedTotal"`
	PushDroppedNoConn int64  `json:"pushDroppedNoConn"`
	Goroutines        int    `json:"goroutines"`
	MemoryAlloc       uint64 `json:"memoryAlloc"`
	UptimeSeconds     int64  `json:"uptimeSeconds"`
}

func (g *Gateway) startDefaultObservability(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) {
		writeDefaultJSON(w, http.StatusOK, map[string]interface{}{"status": "alive"})
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		ready := g.grpcSrv.hasLogicConnection()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeDefaultJSON(w, status, map[string]interface{}{"ready": ready})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeDefaultJSON(w, http.StatusOK, map[string]interface{}{"status": "healthy", "uptimeSeconds": int64(time.Since(defaultStartTime).Seconds())})
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeDefaultJSON(w, http.StatusOK, defaultStats{
			ConnectionsTotal: g.connectionsTotal.Load(), ConnectionsActive: g.connectionsActive.Load(),
			MessagesReceived: g.messagesReceived.Load(), MessagesForwarded: g.messagesForwarded.Load(),
			MessagesPushed: g.messagesPushed.Load(), Goroutines: runtime.NumGoroutine(),
			MessagesDropped: g.messagesDropped.Load(),
			MemoryAlloc:     mem.Alloc, UptimeSeconds: int64(time.Since(defaultStartTime).Seconds()),
		})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(w, "sgate_connections_total %d\nsgate_connections_active %d\nsgate_messages_received_total %d\nsgate_messages_forwarded_total %d\nsgate_messages_pushed_total %d\nsgate_messages_dropped_total %d\n",
			g.connectionsTotal.Load(), g.connectionsActive.Load(), g.messagesReceived.Load(), g.messagesForwarded.Load(), g.messagesPushed.Load(), g.messagesDropped.Load())
	})
	g.statsServer = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() { _ = g.statsServer.ListenAndServe() }()
}

func writeDefaultJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
