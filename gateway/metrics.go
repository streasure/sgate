package gateway

import (
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// PrometheusMetrics Prometheus指标收集器
type PrometheusMetrics struct {
	mu sync.RWMutex

	// 连接指标
	connectionsTotal   uint64
	connectionsActive  int32
	connectionsCreated uint64
	connectionsClosed  uint64

	// 消息指标
	messagesReceived  uint64
	messagesProcessed uint64
	messagesFailed    uint64
	messagesPerSecond float64

	// 延迟指标
	processingTimeTotal   int64
	processingTimeAverage float64
	processingTime99th    float64

	// 工作池指标
	workerCount    int32
	workerMin      int32
	workerMax      int32
	queueSize      int32
	queueCapacity  int32

	// 速率限制指标
	rateLimitHits    uint64
	rateLimitMisses  uint64
	rateLimitBlocked uint64

	// 系统指标
	goroutines    int
	memoryAlloc   uint64
	memorySys     uint64
	gcCount       uint32
	cpuUsage      float64

	// 时间窗口数据（用于计算每秒消息数）
	messageHistory []messageSnapshot
	historyMu      sync.Mutex
}

// messageSnapshot 消息快照
type messageSnapshot struct {
	timestamp time.Time
	count     uint64
}

// NewPrometheusMetrics 创建Prometheus指标收集器
func NewPrometheusMetrics() *PrometheusMetrics {
	pm := &PrometheusMetrics{
		messageHistory: make([]messageSnapshot, 0, 60), // 保留60秒的历史数据
	}

	// 启动指标收集协程
	go pm.collectMetrics()

	return pm
}

// collectMetrics 定期收集指标
func (pm *PrometheusMetrics) collectMetrics() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		pm.updateSystemMetrics()
		pm.calculateMessagesPerSecond()
	}
}

// updateSystemMetrics 更新系统指标
func (pm *PrometheusMetrics) updateSystemMetrics() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	pm.mu.Lock()
	pm.goroutines = runtime.NumGoroutine()
	pm.memoryAlloc = m.Alloc
	pm.memorySys = m.Sys
	pm.gcCount = m.NumGC
	pm.mu.Unlock()
}

// calculateMessagesPerSecond 计算每秒消息数
func (pm *PrometheusMetrics) calculateMessagesPerSecond() {
	pm.historyMu.Lock()
	defer pm.historyMu.Unlock()

	now := time.Now()
	currentCount := pm.GetMessagesReceived()

	// 添加当前快照
	pm.messageHistory = append(pm.messageHistory, messageSnapshot{
		timestamp: now,
		count:     currentCount,
	})

	// 清理过期数据（保留最近60秒）
	cutoff := now.Add(-60 * time.Second)
	startIdx := 0
	for i, snap := range pm.messageHistory {
		if snap.timestamp.After(cutoff) {
			startIdx = i
			break
		}
	}
	pm.messageHistory = pm.messageHistory[startIdx:]

	// 计算每秒消息数
	if len(pm.messageHistory) >= 2 {
		first := pm.messageHistory[0]
		last := pm.messageHistory[len(pm.messageHistory)-1]
		duration := last.timestamp.Sub(first.timestamp).Seconds()
		if duration > 0 {
			pm.mu.Lock()
			pm.messagesPerSecond = float64(last.count-first.count) / duration
			pm.mu.Unlock()
		}
	}
}

// IncConnectionsTotal 增加连接总数
func (pm *PrometheusMetrics) IncConnectionsTotal() {
	pm.mu.Lock()
	pm.connectionsTotal++
	pm.connectionsCreated++
	pm.mu.Unlock()
}

// DecConnectionsActive 减少活跃连接数
func (pm *PrometheusMetrics) DecConnectionsActive() {
	pm.mu.Lock()
	pm.connectionsActive--
	pm.connectionsClosed++
	pm.mu.Unlock()
}

// SetConnectionsActive 设置活跃连接数
func (pm *PrometheusMetrics) SetConnectionsActive(count int32) {
	pm.mu.Lock()
	pm.connectionsActive = count
	pm.mu.Unlock()
}

// IncMessagesReceived 增加接收消息数
func (pm *PrometheusMetrics) IncMessagesReceived() {
	pm.mu.Lock()
	pm.messagesReceived++
	pm.mu.Unlock()
}

// IncMessagesProcessed 增加处理成功消息数
func (pm *PrometheusMetrics) IncMessagesProcessed() {
	pm.mu.Lock()
	pm.messagesProcessed++
	pm.mu.Unlock()
}

// IncMessagesFailed 增加处理失败消息数
func (pm *PrometheusMetrics) IncMessagesFailed() {
	pm.mu.Lock()
	pm.messagesFailed++
	pm.mu.Unlock()
}

// AddProcessingTime 添加处理时间
func (pm *PrometheusMetrics) AddProcessingTime(duration time.Duration) {
	pm.mu.Lock()
	pm.processingTimeTotal += int64(duration)
	if pm.messagesProcessed > 0 {
		pm.processingTimeAverage = float64(pm.processingTimeTotal) / float64(pm.messagesProcessed)
	}
	pm.mu.Unlock()
}

// SetWorkerMetrics 设置工作池指标
func (pm *PrometheusMetrics) SetWorkerMetrics(count, min, max int32) {
	pm.mu.Lock()
	pm.workerCount = count
	pm.workerMin = min
	pm.workerMax = max
	pm.mu.Unlock()
}

// SetQueueMetrics 设置队列指标
func (pm *PrometheusMetrics) SetQueueMetrics(size, capacity int32) {
	pm.mu.Lock()
	pm.queueSize = size
	pm.queueCapacity = capacity
	pm.mu.Unlock()
}

// IncRateLimitHit 增加速率限制命中数
func (pm *PrometheusMetrics) IncRateLimitHit() {
	pm.mu.Lock()
	pm.rateLimitHits++
	pm.mu.Unlock()
}

// IncRateLimitMiss 增加速率限制未命中数
func (pm *PrometheusMetrics) IncRateLimitMiss() {
	pm.mu.Lock()
	pm.rateLimitMisses++
	pm.mu.Unlock()
}

// IncRateLimitBlocked 增加速率限制阻止数
func (pm *PrometheusMetrics) IncRateLimitBlocked() {
	pm.mu.Lock()
	pm.rateLimitBlocked++
	pm.mu.Unlock()
}

// GetMessagesReceived 获取接收消息数
func (pm *PrometheusMetrics) GetMessagesReceived() uint64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.messagesReceived
}

// GetMessagesPerSecond 获取每秒消息数
func (pm *PrometheusMetrics) GetMessagesPerSecond() float64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.messagesPerSecond
}

// ServeMetricsHTTP 提供Prometheus格式的指标端点
func (pm *PrometheusMetrics) ServeMetricsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// 输出Prometheus格式的指标
	fmt.Fprintf(w, "# HELP sgate_connections_total Total number of connections\n")
	fmt.Fprintf(w, "# TYPE sgate_connections_total counter\n")
	fmt.Fprintf(w, "sgate_connections_total %d\n\n", pm.connectionsTotal)

	fmt.Fprintf(w, "# HELP sgate_connections_active Number of active connections\n")
	fmt.Fprintf(w, "# TYPE sgate_connections_active gauge\n")
	fmt.Fprintf(w, "sgate_connections_active %d\n\n", pm.connectionsActive)

	fmt.Fprintf(w, "# HELP sgate_connections_created Total number of created connections\n")
	fmt.Fprintf(w, "# TYPE sgate_connections_created counter\n")
	fmt.Fprintf(w, "sgate_connections_created %d\n\n", pm.connectionsCreated)

	fmt.Fprintf(w, "# HELP sgate_connections_closed Total number of closed connections\n")
	fmt.Fprintf(w, "# TYPE sgate_connections_closed counter\n")
	fmt.Fprintf(w, "sgate_connections_closed %d\n\n", pm.connectionsClosed)

	fmt.Fprintf(w, "# HELP sgate_messages_received_total Total number of received messages\n")
	fmt.Fprintf(w, "# TYPE sgate_messages_received_total counter\n")
	fmt.Fprintf(w, "sgate_messages_received_total %d\n\n", pm.messagesReceived)

	fmt.Fprintf(w, "# HELP sgate_messages_processed_total Total number of processed messages\n")
	fmt.Fprintf(w, "# TYPE sgate_messages_processed_total counter\n")
	fmt.Fprintf(w, "sgate_messages_processed_total %d\n\n", pm.messagesProcessed)

	fmt.Fprintf(w, "# HELP sgate_messages_failed_total Total number of failed messages\n")
	fmt.Fprintf(w, "# TYPE sgate_messages_failed_total counter\n")
	fmt.Fprintf(w, "sgate_messages_failed_total %d\n\n", pm.messagesFailed)

	fmt.Fprintf(w, "# HELP sgate_messages_per_second Number of messages per second\n")
	fmt.Fprintf(w, "# TYPE sgate_messages_per_second gauge\n")
	fmt.Fprintf(w, "sgate_messages_per_second %.2f\n\n", pm.messagesPerSecond)

	fmt.Fprintf(w, "# HELP sgate_processing_time_average Average processing time in microseconds\n")
	fmt.Fprintf(w, "# TYPE sgate_processing_time_average gauge\n")
	fmt.Fprintf(w, "sgate_processing_time_average %.2f\n\n", pm.processingTimeAverage/1000)

	fmt.Fprintf(w, "# HELP sgate_workers Number of workers\n")
	fmt.Fprintf(w, "# TYPE sgate_workers gauge\n")
	fmt.Fprintf(w, "sgate_workers %d\n\n", pm.workerCount)

	fmt.Fprintf(w, "# HELP sgate_workers_min Minimum number of workers\n")
	fmt.Fprintf(w, "# TYPE sgate_workers_min gauge\n")
	fmt.Fprintf(w, "sgate_workers_min %d\n\n", pm.workerMin)

	fmt.Fprintf(w, "# HELP sgate_workers_max Maximum number of workers\n")
	fmt.Fprintf(w, "# TYPE sgate_workers_max gauge\n")
	fmt.Fprintf(w, "sgate_workers_max %d\n\n", pm.workerMax)

	fmt.Fprintf(w, "# HELP sgate_queue_size Current queue size\n")
	fmt.Fprintf(w, "# TYPE sgate_queue_size gauge\n")
	fmt.Fprintf(w, "sgate_queue_size %d\n\n", pm.queueSize)

	fmt.Fprintf(w, "# HELP sgate_queue_capacity Queue capacity\n")
	fmt.Fprintf(w, "# TYPE sgate_queue_capacity gauge\n")
	fmt.Fprintf(w, "sgate_queue_capacity %d\n\n", pm.queueCapacity)

	fmt.Fprintf(w, "# HELP sgate_rate_limit_hits_total Total number of rate limit hits\n")
	fmt.Fprintf(w, "# TYPE sgate_rate_limit_hits_total counter\n")
	fmt.Fprintf(w, "sgate_rate_limit_hits_total %d\n\n", pm.rateLimitHits)

	fmt.Fprintf(w, "# HELP sgate_rate_limit_blocked_total Total number of rate limit blocks\n")
	fmt.Fprintf(w, "# TYPE sgate_rate_limit_blocked_total counter\n")
	fmt.Fprintf(w, "sgate_rate_limit_blocked_total %d\n\n", pm.rateLimitBlocked)

	fmt.Fprintf(w, "# HELP sgate_goroutines Number of goroutines\n")
	fmt.Fprintf(w, "# TYPE sgate_goroutines gauge\n")
	fmt.Fprintf(w, "sgate_goroutines %d\n\n", pm.goroutines)

	fmt.Fprintf(w, "# HELP sgate_memory_alloc_bytes Allocated memory in bytes\n")
	fmt.Fprintf(w, "# TYPE sgate_memory_alloc_bytes gauge\n")
	fmt.Fprintf(w, "sgate_memory_alloc_bytes %d\n\n", pm.memoryAlloc)

	fmt.Fprintf(w, "# HELP sgate_memory_sys_bytes System memory in bytes\n")
	fmt.Fprintf(w, "# TYPE sgate_memory_sys_bytes gauge\n")
	fmt.Fprintf(w, "sgate_memory_sys_bytes %d\n\n", pm.memorySys)

	fmt.Fprintf(w, "# HELP sgate_gc_count Total number of garbage collections\n")
	fmt.Fprintf(w, "# TYPE sgate_gc_count counter\n")
	fmt.Fprintf(w, "sgate_gc_count %d\n", pm.gcCount)
}

// RegisterPrometheusMetrics 注册Prometheus指标到Gateway
func (g *Gateway) RegisterPrometheusMetrics() {
	// 创建Prometheus指标收集器
	promMetrics := NewPrometheusMetrics()

	// 注册路由
	http.HandleFunc("/metrics", promMetrics.ServeMetricsHTTP)

	// 启动HTTP服务器提供指标端点
	go func() {
		addr := ":9090" // Prometheus指标端口
		tlog.Info("启动Prometheus指标服务器", "addr", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			tlog.Error("Prometheus指标服务器启动失败", "error", err)
		}
	}()
}
