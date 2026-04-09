package gateway

import (
	"sync"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// CircuitBreakerState 熔断器状态
type CircuitBreakerState int

const (
	// StateClosed 闭合状态，正常处理请求
	StateClosed CircuitBreakerState = iota
	// StateOpen 打开状态，拒绝所有请求
	StateOpen
	// StateHalfOpen 半开状态，允许部分请求通过以测试服务是否恢复
	StateHalfOpen
)

// CircuitBreaker 熔断器
// 功能: 实现熔断器模式，防止系统过载，保护下游服务
// 字段:
//   state: 熔断器状态
//   failureThreshold: 失败阈值，超过此值则打开熔断器
//   successThreshold: 成功阈值，在半开状态下达到此值则关闭熔断器
//   timeout: 超时时间，打开状态下经过此时间后进入半开状态
//   failureCount: 失败计数
//   successCount: 成功计数
//   lastFailureTime: 最后失败时间
//   mutex: 互斥锁
//   name: 熔断器名称
type CircuitBreaker struct {
	state            CircuitBreakerState // 熔断器状态
	failureThreshold int                 // 失败阈值
	successThreshold int                 // 成功阈值
	timeout          time.Duration       // 超时时间
	failureCount     int                 // 失败计数
	successCount     int                 // 成功计数
	lastFailureTime  time.Time           // 最后失败时间
	mutex            sync.RWMutex        // 互斥锁
	name             string              // 熔断器名称
}

// NewCircuitBreaker 创建熔断器
// 参数:
//   name: 熔断器名称
//   failureThreshold: 失败阈值
//   successThreshold: 成功阈值
//   timeout: 超时时间
// 返回值:
//   *CircuitBreaker: 熔断器实例
func NewCircuitBreaker(name string, failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5 // 默认失败阈值
	}
	if successThreshold <= 0 {
		successThreshold = 3 // 默认成功阈值
	}
	if timeout <= 0 {
		timeout = 30 * time.Second // 默认超时时间
	}

	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		timeout:          timeout,
		failureCount:     0,
		successCount:     0,
		lastFailureTime:  time.Now(),
		name:             name,
	}
}

// Allow 检查是否允许请求通过
// 返回值:
//   bool: 是否允许请求通过
func (cb *CircuitBreaker) Allow() bool {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	switch cb.state {
	case StateClosed:
		// 闭合状态，允许请求通过
		return true

	case StateOpen:
		// 打开状态，检查是否超时
		if time.Since(cb.lastFailureTime) > cb.timeout {
			// 超时，进入半开状态
			cb.state = StateHalfOpen
			cb.successCount = 0
			tlog.Info("Circuit breaker changed state", 
				"name", cb.name, 
				"from", "open", 
				"to", "half-open")
			return true
		}
		// 未超时，拒绝请求
		return false

	case StateHalfOpen:
		// 半开状态，允许请求通过
		return true

	default:
		// 未知状态，默认允许
		return true
	}
}

// RecordSuccess 记录成功
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	switch cb.state {
	case StateClosed:
		// 闭合状态，重置失败计数
		cb.failureCount = 0

	case StateHalfOpen:
		// 半开状态，增加成功计数
		cb.successCount++
		if cb.successCount >= cb.successThreshold {
			// 达到成功阈值，关闭熔断器
			cb.state = StateClosed
			cb.failureCount = 0
			cb.successCount = 0
			tlog.Info("Circuit breaker changed state", 
				"name", cb.name, 
				"from", "half-open", 
				"to", "closed")
		}

	case StateOpen:
		// 打开状态，忽略
	}
}

// RecordFailure 记录失败
func (cb *CircuitBreaker) RecordFailure() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	switch cb.state {
	case StateClosed:
		// 闭合状态，增加失败计数
		cb.failureCount++
		cb.lastFailureTime = time.Now()
		if cb.failureCount >= cb.failureThreshold {
			// 达到失败阈值，打开熔断器
			cb.state = StateOpen
			tlog.Info("Circuit breaker changed state", 
				"name", cb.name, 
				"from", "closed", 
				"to", "open")
		}

	case StateHalfOpen:
		// 半开状态，失败则打开熔断器
		cb.state = StateOpen
		cb.lastFailureTime = time.Now()
		tlog.Info("Circuit breaker changed state", 
			"name", cb.name, 
			"from", "half-open", 
			"to", "open")

	case StateOpen:
		// 打开状态，更新最后失败时间
		cb.lastFailureTime = time.Now()
	}
}

// GetState 获取熔断器状态
// 返回值:
//   CircuitBreakerState: 熔断器状态
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	cb.mutex.RLock()
	defer cb.mutex.RUnlock()

	return cb.state
}

// GetStateString 获取熔断器状态字符串
// 返回值:
//   string: 状态字符串
func (cb *CircuitBreaker) GetStateString() string {
	switch cb.GetState() {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Reset 重置熔断器
func (cb *CircuitBreaker) Reset() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.state = StateClosed
	cb.failureCount = 0
	cb.successCount = 0
	cb.lastFailureTime = time.Now()
	tlog.Info("Circuit breaker reset", "name", cb.name)
}

// CircuitBreakerManager 熔断器管理器
// 功能: 管理多个熔断器，按服务或路由分组
// 字段:
//   breakers: 熔断器映射
//   mutex: 互斥锁
type CircuitBreakerManager struct {
	breakers map[string]*CircuitBreaker // 熔断器映射
	mutex    sync.RWMutex              // 互斥锁
}

// NewCircuitBreakerManager 创建熔断器管理器
// 返回值:
//   *CircuitBreakerManager: 熔断器管理器实例
func NewCircuitBreakerManager() *CircuitBreakerManager {
	return &CircuitBreakerManager{
		breakers: make(map[string]*CircuitBreaker),
	}
}

// GetCircuitBreaker 获取或创建熔断器
// 参数:
//   name: 熔断器名称
//   failureThreshold: 失败阈值
//   successThreshold: 成功阈值
//   timeout: 超时时间
// 返回值:
//   *CircuitBreaker: 熔断器实例
func (cbm *CircuitBreakerManager) GetCircuitBreaker(name string, failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	cbm.mutex.Lock()
	defer cbm.mutex.Unlock()

	if breaker, exists := cbm.breakers[name]; exists {
		return breaker
	}

	// 创建新的熔断器
	breaker := NewCircuitBreaker(name, failureThreshold, successThreshold, timeout)
	cbm.breakers[name] = breaker
	return breaker
}

// GetBreaker 获取熔断器
// 参数:
//   name: 熔断器名称
// 返回值:
//   *CircuitBreaker: 熔断器实例
//   bool: 是否存在
func (cbm *CircuitBreakerManager) GetBreaker(name string) (*CircuitBreaker, bool) {
	cbm.mutex.RLock()
	defer cbm.mutex.RUnlock()

	breaker, exists := cbm.breakers[name]
	return breaker, exists
}

// RemoveBreaker 移除熔断器
// 参数:
//   name: 熔断器名称
func (cbm *CircuitBreakerManager) RemoveBreaker(name string) {
	cbm.mutex.Lock()
	defer cbm.mutex.Unlock()

	delete(cbm.breakers, name)
}

// ListBreakers 列出所有熔断器
// 返回值:
//   map[string]*CircuitBreaker: 熔断器映射
func (cbm *CircuitBreakerManager) ListBreakers() map[string]*CircuitBreaker {
	cbm.mutex.RLock()
	defer cbm.mutex.RUnlock()

	// 复制一份返回，避免外部修改
	breakers := make(map[string]*CircuitBreaker, len(cbm.breakers))
	for name, breaker := range cbm.breakers {
		breakers[name] = breaker
	}
	return breakers
}
