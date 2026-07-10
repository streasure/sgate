package metrics

import (
	"runtime"
	"sync/atomic"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

type Metrics struct {
	connectionsTotal     int64
	connectionsActive    int64
	websocketConnections int64

	messagesReceived  int64
	messagesProcessed int64
	messagesFailed    int64

	totalProcessingTime int64
	maxProcessingTime   int64

	errorCount    int64
	lastErrorTime time.Time

	redisConnections int64
	redisErrors      int64

	cpuUsage    float64
	memoryUsage uint64

	rateLimitCount   int64
	blacklistRejects int64
	whitelistAllows  int64

	bytesReceived int64
	bytesSent     int64

	circuitBreakerFailures int64

	activeConnectionsThreshold int64
	failedMessagesThreshold    int64
	processingTimeThreshold    int64
	redisErrorsThreshold       int64
}

func NewMetrics() *Metrics {
	return &Metrics{
		activeConnectionsThreshold: 100000,
		failedMessagesThreshold:    100,
		processingTimeThreshold:    100,
		redisErrorsThreshold:       10,
	}
}

func (m *Metrics) IncConnectionsTotal()            { atomic.AddInt64(&m.connectionsTotal, 1) }
func (m *Metrics) IncConnectionsActive()           { atomic.AddInt64(&m.connectionsActive, 1) }
func (m *Metrics) DecConnectionsActive()           { atomic.AddInt64(&m.connectionsActive, -1) }
func (m *Metrics) IncMessagesReceived()            { atomic.AddInt64(&m.messagesReceived, 1) }
func (m *Metrics) IncMessagesProcessed()           { atomic.AddInt64(&m.messagesProcessed, 1) }
func (m *Metrics) IncMessagesFailed()              { atomic.AddInt64(&m.messagesFailed, 1) }
func (m *Metrics) IncErrorCount()                  { atomic.AddInt64(&m.errorCount, 1); m.lastErrorTime = time.Now() }
func (m *Metrics) IncRedisConnections()            { atomic.AddInt64(&m.redisConnections, 1) }
func (m *Metrics) DecRedisConnections()            { atomic.AddInt64(&m.redisConnections, -1) }
func (m *Metrics) IncRedisErrors()                 { atomic.AddInt64(&m.redisErrors, 1) }
func (m *Metrics) IncRateLimitCount()              { atomic.AddInt64(&m.rateLimitCount, 1) }
func (m *Metrics) IncBlacklistRejects()            { atomic.AddInt64(&m.blacklistRejects, 1) }
func (m *Metrics) IncWhitelistAllows()             { atomic.AddInt64(&m.whitelistAllows, 1) }
func (m *Metrics) IncWebSocketConnections()        { atomic.AddInt64(&m.websocketConnections, 1) }
func (m *Metrics) DecWebSocketConnections()        { atomic.AddInt64(&m.websocketConnections, -1) }
func (m *Metrics) IncCircuitBreakerFailures()      { atomic.AddInt64(&m.circuitBreakerFailures, 1) }
func (m *Metrics) AddBytesReceived(b int64)        { atomic.AddInt64(&m.bytesReceived, b) }
func (m *Metrics) AddBytesSent(b int64)            { atomic.AddInt64(&m.bytesSent, b) }

func (m *Metrics) AddProcessingTime(duration time.Duration) {
	atomic.AddInt64(&m.totalProcessingTime, duration.Milliseconds())
	for {
		cur := atomic.LoadInt64(&m.maxProcessingTime)
		if duration.Milliseconds() <= cur { break }
		if atomic.CompareAndSwapInt64(&m.maxProcessingTime, cur, duration.Milliseconds()) { break }
	}
}

func (m *Metrics) GetConnectionsTotal() int64      { return atomic.LoadInt64(&m.connectionsTotal) }
func (m *Metrics) GetConnectionsActive() int64     { return atomic.LoadInt64(&m.connectionsActive) }
func (m *Metrics) GetMessagesReceived() int64      { return atomic.LoadInt64(&m.messagesReceived) }
func (m *Metrics) GetMessagesProcessed() int64     { return atomic.LoadInt64(&m.messagesProcessed) }
func (m *Metrics) GetMessagesFailed() int64        { return atomic.LoadInt64(&m.messagesFailed) }
func (m *Metrics) GetMaxProcessingTime() int64     { return atomic.LoadInt64(&m.maxProcessingTime) }
func (m *Metrics) GetErrorCount() int64            { return atomic.LoadInt64(&m.errorCount) }
func (m *Metrics) GetLastErrorTime() time.Time     { return m.lastErrorTime }
func (m *Metrics) GetRedisConnections() int64      { return atomic.LoadInt64(&m.redisConnections) }
func (m *Metrics) GetRedisErrors() int64           { return atomic.LoadInt64(&m.redisErrors) }
func (m *Metrics) GetMemoryUsage() uint64          { return atomic.LoadUint64(&m.memoryUsage) }
func (m *Metrics) GetCPUUsage() float64            { return m.cpuUsage }
func (m *Metrics) GetWebSocketConnections() int64  { return atomic.LoadInt64(&m.websocketConnections) }
func (m *Metrics) GetRateLimitCount() int64        { return atomic.LoadInt64(&m.rateLimitCount) }
func (m *Metrics) GetBlacklistRejects() int64      { return atomic.LoadInt64(&m.blacklistRejects) }
func (m *Metrics) GetWhitelistAllows() int64       { return atomic.LoadInt64(&m.whitelistAllows) }
func (m *Metrics) GetBytesReceived() int64         { return atomic.LoadInt64(&m.bytesReceived) }
func (m *Metrics) GetBytesSent() int64             { return atomic.LoadInt64(&m.bytesSent) }
func (m *Metrics) GetCircuitBreakerFailures() int64 { return atomic.LoadInt64(&m.circuitBreakerFailures) }

func (m *Metrics) GetAverageProcessingTime() float64 {
	processed := atomic.LoadInt64(&m.messagesProcessed)
	if processed == 0 { return 0 }
	return float64(atomic.LoadInt64(&m.totalProcessingTime)) / float64(processed)
}

func (m *Metrics) SetActiveConnectionsThreshold(v int64) { m.activeConnectionsThreshold = v }
func (m *Metrics) SetFailedMessagesThreshold(v int64)    { m.failedMessagesThreshold = v }
func (m *Metrics) SetProcessingTimeThreshold(v int64)    { m.processingTimeThreshold = v }
func (m *Metrics) SetRedisErrorsThreshold(v int64)       { m.redisErrorsThreshold = v }

func (m *Metrics) UpdateSystemMetrics() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	atomic.StoreUint64(&m.memoryUsage, memStats.Alloc)
}

func (m *Metrics) Reset() {
	atomic.StoreInt64(&m.connectionsTotal, 0)
	atomic.StoreInt64(&m.connectionsActive, 0)
	atomic.StoreInt64(&m.websocketConnections, 0)
	atomic.StoreInt64(&m.messagesReceived, 0)
	atomic.StoreInt64(&m.messagesProcessed, 0)
	atomic.StoreInt64(&m.messagesFailed, 0)
	atomic.StoreInt64(&m.totalProcessingTime, 0)
	atomic.StoreInt64(&m.maxProcessingTime, 0)
	atomic.StoreInt64(&m.errorCount, 0)
	atomic.StoreInt64(&m.redisConnections, 0)
	atomic.StoreInt64(&m.redisErrors, 0)
	atomic.StoreInt64(&m.rateLimitCount, 0)
	atomic.StoreInt64(&m.blacklistRejects, 0)
	atomic.StoreInt64(&m.whitelistAllows, 0)
	atomic.StoreInt64(&m.bytesReceived, 0)
	atomic.StoreInt64(&m.bytesSent, 0)
	atomic.StoreInt64(&m.circuitBreakerFailures, 0)
	m.cpuUsage = 0
}

func (m *Metrics) LogMetrics() {
	m.UpdateSystemMetrics()
	tlog.Info("gateway metrics",
		"connectionsActive", m.GetConnectionsActive(),
		"messagesReceived", m.GetMessagesReceived(),
		"messagesProcessed", m.GetMessagesProcessed(),
		"messagesFailed", m.GetMessagesFailed(),
		"avgProcessingTime", m.GetAverageProcessingTime(),
		"memoryUsage", m.GetMemoryUsage(),
	)
	m.CheckAlerts()
}

func (m *Metrics) CheckAlerts() {
	if v := m.GetConnectionsActive(); v > m.activeConnectionsThreshold {
		tlog.Warn("active connections threshold exceeded", "current", v, "threshold", m.activeConnectionsThreshold)
	}
	if v := m.GetMessagesFailed(); v > m.failedMessagesThreshold {
		tlog.Warn("failed messages threshold exceeded", "current", v, "threshold", m.failedMessagesThreshold)
	}
	if v := m.GetAverageProcessingTime(); v > float64(m.processingTimeThreshold) {
		tlog.Warn("processing time threshold exceeded", "current", v, "threshold", m.processingTimeThreshold)
	}
	if v := m.GetRedisErrors(); v > m.redisErrorsThreshold {
		tlog.Warn("redis errors threshold exceeded", "current", v, "threshold", m.redisErrorsThreshold)
	}
}
