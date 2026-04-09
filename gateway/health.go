package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
)

// HealthStatus 健康状态
type HealthStatus struct {
	Status    string            `json:"status"`     // healthy, unhealthy, degraded
	Timestamp time.Time         `json:"timestamp"`
	Version   string            `json:"version"`
	Uptime    time.Duration     `json:"uptime"`
	Checks    map[string]Check  `json:"checks"`
	Metrics   HealthMetrics     `json:"metrics"`
}

// Check 健康检查项
type Check struct {
	Status  string `json:"status"`  // pass, fail, warn
	Message string `json:"message,omitempty"`
	Latency int64  `json:"latency_ms,omitempty"` // 延迟（毫秒）
}

// HealthMetrics 健康指标
type HealthMetrics struct {
	Connections     int `json:"connections"`      // 当前连接数
	Goroutines      int `json:"goroutines"`       // Goroutine数量
	MemoryAlloc     uint64 `json:"memory_alloc_mb"`  // 内存分配（MB）
	MemorySys       uint64 `json:"memory_sys_mb"`    // 系统内存（MB）
	GCCount         uint32 `json:"gc_count"`         // GC次数
	MessagesPerSec  float64 `json:"messages_per_sec"` // 每秒消息数
}

// ReadinessStatus 就绪状态
type ReadinessStatus struct {
	Ready     bool      `json:"ready"`
	Timestamp time.Time `json:"timestamp"`
	Reason    string    `json:"reason,omitempty"`
}

// LivenessStatus 存活状态
type LivenessStatus struct {
	Alive     bool      `json:"alive"`
	Timestamp time.Time `json:"timestamp"`
}

var (
	startTime = time.Now()
	version   = "1.0.0" // 可以通过编译时注入
)

// HealthCheck 健康检查
func (g *Gateway) HealthCheck() *HealthStatus {
	status := &HealthStatus{
		Status:    "healthy",
		Timestamp: time.Now(),
		Version:   version,
		Uptime:    time.Since(startTime),
		Checks:    make(map[string]Check),
	}

	// 检查各项依赖
	checks := []struct {
		name string
		fn   func() Check
	}{
		{"gateway", g.checkGateway},
		{"rate_limiter", g.checkRateLimiter},
		{"worker_pool", g.checkWorkerPool},
		{"message_queue", g.checkMessageQueue},
	}

	for _, check := range checks {
		status.Checks[check.name] = check.fn()
	}

	// 如果有任何检查失败，状态为 unhealthy
	for _, check := range status.Checks {
		if check.Status == "fail" {
			status.Status = "unhealthy"
			break
		}
		if check.Status == "warn" && status.Status == "healthy" {
			status.Status = "degraded"
		}
	}

	// 收集指标
	status.Metrics = g.collectMetrics()

	return status
}

// ReadinessCheck 就绪检查
func (g *Gateway) ReadinessCheck() *ReadinessStatus {
	status := &ReadinessStatus{
		Ready:     true,
		Timestamp: time.Now(),
	}

	// 检查是否已启动足够时间
	if time.Since(startTime) < 5*time.Second {
		status.Ready = false
		status.Reason = "Service is still initializing"
		return status
	}

	// 检查工作池是否就绪
	if g.workerCount < int(g.minWorkers.Load()) {
		status.Ready = false
		status.Reason = "Worker pool is not ready"
		return status
	}

	// 检查消息队列是否已满
	if len(g.messagePool) >= cap(g.messagePool)-100 {
		status.Ready = false
		status.Reason = "Message queue is almost full"
		return status
	}

	return status
}

// LivenessCheck 存活检查
func (g *Gateway) LivenessCheck() *LivenessStatus {
	return &LivenessStatus{
		Alive:     true,
		Timestamp: time.Now(),
	}
}

// checkGateway 检查网关状态
func (g *Gateway) checkGateway() Check {
	return Check{
		Status:  "pass",
		Message: "Gateway is running",
	}
}

// checkRateLimiter 检查速率限制器
func (g *Gateway) checkRateLimiter() Check {
	if g.rateLimiter == nil {
		return Check{
			Status:  "fail",
			Message: "Rate limiter is not initialized",
		}
	}
	return Check{
		Status:  "pass",
		Message: "Rate limiter is healthy",
	}
}

// checkWorkerPool 检查工作池
func (g *Gateway) checkWorkerPool() Check {
	if g.workerCount < int(g.minWorkers.Load()) {
		return Check{
			Status:  "warn",
			Message: "Worker count is below minimum",
		}
	}
	return Check{
		Status:  "pass",
		Message: "Worker pool is healthy",
	}
}

// checkMessageQueue 检查消息队列
func (g *Gateway) checkMessageQueue() Check {
	queueLen := len(g.messagePool)
	queueCap := cap(g.messagePool)
	
	if queueLen >= queueCap-100 {
		return Check{
			Status:  "fail",
			Message: "Message queue is almost full",
		}
	}
	
	if queueLen > queueCap/2 {
		return Check{
			Status:  "warn",
			Message: "Message queue is more than half full",
		}
	}
	
	return Check{
		Status:  "pass",
		Message: "Message queue is healthy",
	}
}

// collectMetrics 收集指标
func (g *Gateway) collectMetrics() HealthMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return HealthMetrics{
		Connections:    g.connectionManager.GetConnectionCount(),
		Goroutines:     runtime.NumGoroutine(),
		MemoryAlloc:    m.Alloc / 1024 / 1024, // MB
		MemorySys:      m.Sys / 1024 / 1024,   // MB
		GCCount:        m.NumGC,
		MessagesPerSec: float64(g.metrics.GetMessagesReceived()),
	}
}

// ServeHealthHTTP 提供HTTP健康检查端点
func (g *Gateway) ServeHealthHTTP(w http.ResponseWriter, r *http.Request) {
	var response interface{}
	var statusCode int

	switch r.URL.Path {
	case "/health":
		response = g.HealthCheck()
		statusCode = http.StatusOK
		if resp, ok := response.(*HealthStatus); ok && resp.Status == "unhealthy" {
			statusCode = http.StatusServiceUnavailable
		}
	case "/ready":
		response = g.ReadinessCheck()
		statusCode = http.StatusOK
		if resp, ok := response.(*ReadinessStatus); ok && !resp.Ready {
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

// HandleHealthCheck 处理健康检查请求（通过消息路由）
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
