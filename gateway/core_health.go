package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/streasure/sgate/gateway/obs"
	"github.com/streasure/sgate/protobuf"
)

var (
	startTime = time.Now()
	version   = "1.0.0" // 可以通过编译时注入
)

func (g *Gateway) HealthCheck() *obs.HealthStatus {
	status := &obs.HealthStatus{
		Status:    "healthy",
		Timestamp: time.Now(),
		Version:   version,
		Uptime:    time.Since(startTime),
		Checks:    make(map[string]obs.Check),
	}

	checks := []struct {
		name string
		fn   func() obs.Check
	}{
		{"gateway", g.checkGateway},
		{"rate_limiter", g.checkRateLimiter},
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

func (g *Gateway) checkRateLimiter() obs.Check {
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
		MessagesPerSec: float64(g.metrics.GetMessagesReceived()),
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
	json.NewEncoder(w).Encode(response)
}

func (g *Gateway) HandleHealthCheck(connectionID string, payload map[string]string) (*protobuf.Message, error) {
	checkType := payload["type"]
	if checkType == "" {
		checkType = "health"
	}

	var response map[string]string

	switch checkType {
	case "health":
		status := g.HealthCheck()
		response = map[string]string{
			"status":    status.Status,
			"version":   status.Version,
			"uptime":    status.Uptime.String(),
			"timestamp": status.Timestamp.Format(time.RFC3339),
		}
	case "ready":
		status := g.ReadinessCheck()
		response = map[string]string{
			"ready":     "true",
			"timestamp": status.Timestamp.Format(time.RFC3339),
		}
		if !status.Ready {
			response["ready"] = "false"
			response["reason"] = status.Reason
		}
	case "live":
		status := g.LivenessCheck()
		response = map[string]string{
			"alive":     "true",
			"timestamp": status.Timestamp.Format(time.RFC3339),
		}
	default:
		return nil, fmt.Errorf("unknown health check type")
	}

	return NewResponseMessage("health", response), nil
}
