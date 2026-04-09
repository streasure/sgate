package metrics

import (
	"runtime"
	"sync/atomic"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// Metrics 监控指标结构
// 字段:
//   connectionsTotal: 总连接数
//   connectionsActive: 活跃连接数
//   messagesReceived: 接收到的消息数
//   messagesProcessed: 处理成功的消息数
//   messagesFailed: 处理失败的消息数
//   totalProcessingTime: 总处理时间（毫秒）
//   maxProcessingTime: 最大处理时间（毫秒）
//   errorCount: 错误计数
//   lastErrorTime: 最后一次错误时间
//   redisConnections: Redis连接数
//   redisErrors: Redis错误数
//   queueLength: 消息队列长度
//   cpuUsage: CPU使用率
//   memoryUsage: 内存使用情况
//   workerCount: 工作线程数
//   websocketConnections: WebSocket连接数
//   rateLimitCount: 速率限制触发次数
//   blacklistRejects: 黑名单拒绝次数
//   whitelistAllows: 白名单允许次数
//   bytesReceived: 接收的字节数
//   bytesSent: 发送的字节数

type Metrics struct {
	// 连接指标
	connectionsTotal      int64 // 总连接数
	connectionsActive     int64 // 活跃连接数
	websocketConnections  int64 // WebSocket连接数

	// 消息指标
	messagesReceived  int64 // 接收到的消息数
	messagesProcessed int64 // 处理成功的消息数
	messagesFailed    int64 // 处理失败的消息数

	// 性能指标
	totalProcessingTime int64 // 总处理时间（毫秒）
	maxProcessingTime   int64 // 最大处理时间（毫秒）

	// 错误指标
	errorCount     int64     // 错误计数
	lastErrorTime  time.Time // 最后一次错误时间

	// Redis指标
	redisConnections int64 // Redis连接数
	redisErrors      int64 // Redis错误数

	// 系统指标
	queueLength    int64   // 消息队列长度
	workerCount    int64   // 工作线程数
	cpuUsage       float64 // CPU使用率
	memoryUsage    uint64  // 内存使用情况（字节）

	// 安全指标
	rateLimitCount    int64 // 速率限制触发次数
	blacklistRejects  int64 // 黑名单拒绝次数
	whitelistAllows   int64 // 白名单允许次数

	// 网络指标
	bytesReceived     int64 // 接收的字节数
	bytesSent         int64 // 发送的字节数

	// 熔断器指标
	circuitBreakerFailures int64 // 熔断器失败次数

	// 告警阈值
	activeConnectionsThreshold int64 // 活跃连接数阈值
	failedMessagesThreshold    int64 // 失败消息数阈值
	processingTimeThreshold    int64 // 处理时间阈值（毫秒）
	redisErrorsThreshold       int64 // Redis错误数阈值
	queueLengthThreshold       int64 // 消息队列长度阈值
}

// NewMetrics 创建监控指标实例
// 返回值:
//
//	*Metrics: 监控指标实例
func NewMetrics() *Metrics {
	return &Metrics{
		// 初始化告警阈值
		activeConnectionsThreshold: 1000, // 活跃连接数阈值
		failedMessagesThreshold:    100,  // 失败消息数阈值
		processingTimeThreshold:    100,  // 处理时间阈值（毫秒）
		redisErrorsThreshold:       10,   // Redis错误数阈值
		queueLengthThreshold:       10000, // 消息队列长度阈值
	}
}

// IncConnectionsTotal 增加总连接数
func (m *Metrics) IncConnectionsTotal() {
	atomic.AddInt64(&m.connectionsTotal, 1)
}

// IncConnectionsActive 增加活跃连接数
func (m *Metrics) IncConnectionsActive() {
	atomic.AddInt64(&m.connectionsActive, 1)
}

// DecConnectionsActive 减少活跃连接数
func (m *Metrics) DecConnectionsActive() {
	atomic.AddInt64(&m.connectionsActive, -1)
}

// IncMessagesReceived 增加接收到的消息数
func (m *Metrics) IncMessagesReceived() {
	atomic.AddInt64(&m.messagesReceived, 1)
}

// IncMessagesProcessed 增加处理成功的消息数
func (m *Metrics) IncMessagesProcessed() {
	atomic.AddInt64(&m.messagesProcessed, 1)
}

// IncMessagesFailed 增加处理失败的消息数
func (m *Metrics) IncMessagesFailed() {
	atomic.AddInt64(&m.messagesFailed, 1)
}

// AddProcessingTime 添加消息处理时间
// 参数:
//
//	duration: 处理时间
func (m *Metrics) AddProcessingTime(duration time.Duration) {
	atomic.AddInt64(&m.totalProcessingTime, duration.Milliseconds())

	// 更新最大处理时间
	for {
		currentMax := atomic.LoadInt64(&m.maxProcessingTime)
		if duration.Milliseconds() <= currentMax {
			break
		}
		if atomic.CompareAndSwapInt64(&m.maxProcessingTime, currentMax, duration.Milliseconds()) {
			break
		}
	}
}

// GetConnectionsTotal 获取总连接数
// 返回值:
//
//	int64: 总连接数
func (m *Metrics) GetConnectionsTotal() int64 {
	return atomic.LoadInt64(&m.connectionsTotal)
}

// GetConnectionsActive 获取活跃连接数
// 返回值:
//
//	int64: 活跃连接数
func (m *Metrics) GetConnectionsActive() int64 {
	return atomic.LoadInt64(&m.connectionsActive)
}

// GetMessagesReceived 获取接收到的消息数
// 返回值:
//
//	int64: 接收到的消息数
func (m *Metrics) GetMessagesReceived() int64 {
	return atomic.LoadInt64(&m.messagesReceived)
}

// GetMessagesProcessed 获取处理成功的消息数
// 返回值:
//
//	int64: 处理成功的消息数
func (m *Metrics) GetMessagesProcessed() int64 {
	return atomic.LoadInt64(&m.messagesProcessed)
}

// GetMessagesFailed 获取处理失败的消息数
// 返回值:
//
//	int64: 处理失败的消息数
func (m *Metrics) GetMessagesFailed() int64 {
	return atomic.LoadInt64(&m.messagesFailed)
}

// GetAverageProcessingTime 获取平均处理时间
// 返回值:
//
//	float64: 平均处理时间（毫秒）
func (m *Metrics) GetAverageProcessingTime() float64 {
	processed := atomic.LoadInt64(&m.messagesProcessed)
	if processed == 0 {
		return 0
	}
	totalTime := atomic.LoadInt64(&m.totalProcessingTime)
	return float64(totalTime) / float64(processed)
}

// GetMaxProcessingTime 获取最大处理时间
// 返回值:
//
//	int64: 最大处理时间（毫秒）
func (m *Metrics) GetMaxProcessingTime() int64 {
	return atomic.LoadInt64(&m.maxProcessingTime)
}

// Reset 重置所有指标
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
	atomic.StoreInt64(&m.queueLength, 0)
	atomic.StoreInt64(&m.workerCount, 0)
	atomic.StoreInt64(&m.rateLimitCount, 0)
	atomic.StoreInt64(&m.blacklistRejects, 0)
	atomic.StoreInt64(&m.whitelistAllows, 0)
	atomic.StoreInt64(&m.bytesReceived, 0)
	atomic.StoreInt64(&m.bytesSent, 0)
	atomic.StoreInt64(&m.circuitBreakerFailures, 0)
	m.cpuUsage = 0
}

// LogMetrics 记录当前指标
func (m *Metrics) LogMetrics() {
	// 更新系统指标
	m.UpdateSystemMetrics()
	
	tlog.Info("网关性能指标",
		"connectionsTotal", m.GetConnectionsTotal(),
		"connectionsActive", m.GetConnectionsActive(),
		"websocketConnections", m.GetWebSocketConnections(),
		"messagesReceived", m.GetMessagesReceived(),
		"messagesProcessed", m.GetMessagesProcessed(),
		"messagesFailed", m.GetMessagesFailed(),
		"circuitBreakerFailures", m.GetCircuitBreakerFailures(),
		"averageProcessingTime", m.GetAverageProcessingTime(),
		"maxProcessingTime", m.GetMaxProcessingTime(),
		"redisConnections", m.GetRedisConnections(),
		"redisErrors", m.GetRedisErrors(),
		"queueLength", m.GetQueueLength(),
		"workerCount", m.GetWorkerCount(),
		"memoryUsage", m.GetMemoryUsage(),
		"cpuUsage", m.GetCPUUsage(),
		"rateLimitCount", m.GetRateLimitCount(),
		"blacklistRejects", m.GetBlacklistRejects(),
		"whitelistAllows", m.GetWhitelistAllows(),
		"bytesReceived", m.GetBytesReceived(),
		"bytesSent", m.GetBytesSent())

	// 检查告警
	m.CheckAlerts()
}

// CheckAlerts 检查告警
func (m *Metrics) CheckAlerts() {
	// 检查活跃连接数
	activeConnections := m.GetConnectionsActive()
	if activeConnections > m.activeConnectionsThreshold {
		tlog.Warn("活跃连接数超过阈值", 
			"current", activeConnections, 
			"threshold", m.activeConnectionsThreshold)
	}

	// 检查失败消息数
	failedMessages := m.GetMessagesFailed()
	if failedMessages > m.failedMessagesThreshold {
		tlog.Warn("失败消息数超过阈值", 
			"current", failedMessages, 
			"threshold", m.failedMessagesThreshold)
	}

	// 检查处理时间
	averageProcessingTime := m.GetAverageProcessingTime()
	if averageProcessingTime > float64(m.processingTimeThreshold) {
		tlog.Warn("平均处理时间超过阈值", 
			"current", averageProcessingTime, 
			"threshold", m.processingTimeThreshold)
	}

	// 检查最大处理时间
	maxProcessingTime := m.GetMaxProcessingTime()
	if maxProcessingTime > m.processingTimeThreshold {
		tlog.Warn("最大处理时间超过阈值", 
			"current", maxProcessingTime, 
			"threshold", m.processingTimeThreshold)
	}

	// 检查Redis错误数
	redisErrors := m.GetRedisErrors()
	if redisErrors > m.redisErrorsThreshold {
		tlog.Warn("Redis错误数超过阈值", 
			"current", redisErrors, 
			"threshold", m.redisErrorsThreshold)
	}

	// 检查消息队列长度
	queueLength := m.GetQueueLength()
	if queueLength > m.queueLengthThreshold {
		tlog.Warn("消息队列长度超过阈值", 
			"current", queueLength, 
			"threshold", m.queueLengthThreshold)
	}
}

// IncErrorCount 增加错误计数
func (m *Metrics) IncErrorCount() {
	atomic.AddInt64(&m.errorCount, 1)
	m.lastErrorTime = time.Now()
}

// GetErrorCount 获取错误计数
func (m *Metrics) GetErrorCount() int64 {
	return atomic.LoadInt64(&m.errorCount)
}

// GetLastErrorTime 获取最后一次错误时间
func (m *Metrics) GetLastErrorTime() time.Time {
	return m.lastErrorTime
}

// SetActiveConnectionsThreshold 设置活跃连接数阈值
func (m *Metrics) SetActiveConnectionsThreshold(threshold int64) {
	m.activeConnectionsThreshold = threshold
}

// SetFailedMessagesThreshold 设置失败消息数阈值
func (m *Metrics) SetFailedMessagesThreshold(threshold int64) {
	m.failedMessagesThreshold = threshold
}

// SetProcessingTimeThreshold 设置处理时间阈值
func (m *Metrics) SetProcessingTimeThreshold(threshold int64) {
	m.processingTimeThreshold = threshold
}

// IncRedisConnections 增加Redis连接数
func (m *Metrics) IncRedisConnections() {
	atomic.AddInt64(&m.redisConnections, 1)
}

// DecRedisConnections 减少Redis连接数
func (m *Metrics) DecRedisConnections() {
	atomic.AddInt64(&m.redisConnections, -1)
}

// IncRedisErrors 增加Redis错误数
func (m *Metrics) IncRedisErrors() {
	atomic.AddInt64(&m.redisErrors, 1)
}

// SetQueueLength 设置消息队列长度
func (m *Metrics) SetQueueLength(length int64) {
	atomic.StoreInt64(&m.queueLength, length)
}

// UpdateSystemMetrics 更新系统指标
func (m *Metrics) UpdateSystemMetrics() {
	// 收集内存使用情况
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	atomic.StoreUint64(&m.memoryUsage, memStats.Alloc)
	
	// 这里简化处理，实际应该使用更准确的CPU使用率计算方法
	// m.cpuUsage = calculateCPUUsage()
}

// GetRedisConnections 获取Redis连接数
func (m *Metrics) GetRedisConnections() int64 {
	return atomic.LoadInt64(&m.redisConnections)
}

// GetRedisErrors 获取Redis错误数
func (m *Metrics) GetRedisErrors() int64 {
	return atomic.LoadInt64(&m.redisErrors)
}

// GetQueueLength 获取消息队列长度
func (m *Metrics) GetQueueLength() int64 {
	return atomic.LoadInt64(&m.queueLength)
}

// GetMemoryUsage 获取内存使用情况
func (m *Metrics) GetMemoryUsage() uint64 {
	return atomic.LoadUint64(&m.memoryUsage)
}

// GetCPUUsage 获取CPU使用率
func (m *Metrics) GetCPUUsage() float64 {
	return m.cpuUsage
}

// SetRedisErrorsThreshold 设置Redis错误数阈值
func (m *Metrics) SetRedisErrorsThreshold(threshold int64) {
	m.redisErrorsThreshold = threshold
}

// SetQueueLengthThreshold 设置消息队列长度阈值
func (m *Metrics) SetQueueLengthThreshold(threshold int64) {
	m.queueLengthThreshold = threshold
}

// IncWebSocketConnections 增加WebSocket连接数
func (m *Metrics) IncWebSocketConnections() {
	atomic.AddInt64(&m.websocketConnections, 1)
}

// DecWebSocketConnections 减少WebSocket连接数
func (m *Metrics) DecWebSocketConnections() {
	atomic.AddInt64(&m.websocketConnections, -1)
}

// GetWebSocketConnections 获取WebSocket连接数
func (m *Metrics) GetWebSocketConnections() int64 {
	return atomic.LoadInt64(&m.websocketConnections)
}

// SetWorkerCount 设置工作线程数
func (m *Metrics) SetWorkerCount(count int64) {
	atomic.StoreInt64(&m.workerCount, count)
}

// GetWorkerCount 获取工作线程数
func (m *Metrics) GetWorkerCount() int64 {
	return atomic.LoadInt64(&m.workerCount)
}

// IncRateLimitCount 增加速率限制触发次数
func (m *Metrics) IncRateLimitCount() {
	atomic.AddInt64(&m.rateLimitCount, 1)
}

// GetRateLimitCount 获取速率限制触发次数
func (m *Metrics) GetRateLimitCount() int64 {
	return atomic.LoadInt64(&m.rateLimitCount)
}

// IncBlacklistRejects 增加黑名单拒绝次数
func (m *Metrics) IncBlacklistRejects() {
	atomic.AddInt64(&m.blacklistRejects, 1)
}

// GetBlacklistRejects 获取黑名单拒绝次数
func (m *Metrics) GetBlacklistRejects() int64 {
	return atomic.LoadInt64(&m.blacklistRejects)
}

// IncWhitelistAllows 增加白名单允许次数
func (m *Metrics) IncWhitelistAllows() {
	atomic.AddInt64(&m.whitelistAllows, 1)
}

// GetWhitelistAllows 获取白名单允许次数
func (m *Metrics) GetWhitelistAllows() int64 {
	return atomic.LoadInt64(&m.whitelistAllows)
}

// AddBytesReceived 增加接收的字节数
func (m *Metrics) AddBytesReceived(bytes int64) {
	atomic.AddInt64(&m.bytesReceived, bytes)
}

// GetBytesReceived 获取接收的字节数
func (m *Metrics) GetBytesReceived() int64 {
	return atomic.LoadInt64(&m.bytesReceived)
}

// AddBytesSent 增加发送的字节数
func (m *Metrics) AddBytesSent(bytes int64) {
	atomic.AddInt64(&m.bytesSent, bytes)
}

// GetBytesSent 获取发送的字节数
func (m *Metrics) GetBytesSent() int64 {
	return atomic.LoadInt64(&m.bytesSent)
}

// IncCircuitBreakerFailures 增加熔断器失败次数
func (m *Metrics) IncCircuitBreakerFailures() {
	atomic.AddInt64(&m.circuitBreakerFailures, 1)
}

// GetCircuitBreakerFailures 获取熔断器失败次数
func (m *Metrics) GetCircuitBreakerFailures() int64 {
	return atomic.LoadInt64(&m.circuitBreakerFailures)
}
