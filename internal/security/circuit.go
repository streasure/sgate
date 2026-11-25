package security

import (
	"sync"
	"sync/atomic"
	"time"
)

// CircuitBreakerState 熔断器状态
type CircuitBreakerState int32

const (
	StateClosed CircuitBreakerState = iota
	StateOpen
	StateHalfOpen
)

type CircuitBreaker struct {
	state            atomic.Int32
	failureThreshold int
	successThreshold int
	timeout          time.Duration
	failureCount     atomic.Int32
	successCount     atomic.Int32
	lastFailureTime  atomic.Int64
	trippedCount     atomic.Int64
	mutex            sync.Mutex
	name             string
}

func NewCircuitBreaker(name string, failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if successThreshold <= 0 {
		successThreshold = 3
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	cb := &CircuitBreaker{
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		timeout:          timeout,
		name:             name,
	}
	cb.state.Store(int32(StateClosed))
	cb.lastFailureTime.Store(time.Now().UnixNano())
	return cb
}

func (cb *CircuitBreaker) Allow() bool {
	state := CircuitBreakerState(cb.state.Load())

	switch state {
	case StateClosed:
		return true

	case StateOpen:
		lastFailure := time.Unix(0, cb.lastFailureTime.Load())
		if time.Since(lastFailure) > cb.timeout {
			// 用 CAS 保证只有一个 goroutine 把状态从 Open 切到 HalfOpen
			// 避免多个请求同时进入半开状态
			if cb.state.CompareAndSwap(int32(StateOpen), int32(StateHalfOpen)) {
				cb.successCount.Store(0)
				return true
			}
			// CAS 失败：其他 goroutine 已切换，当前请求按新状态判定
			return cb.Allow()
		}
		return false

	case StateHalfOpen:
		return true

	default:
		return true
	}
}

func (cb *CircuitBreaker) RecordSuccess() {
	state := CircuitBreakerState(cb.state.Load())

	switch state {
	case StateClosed:
		cb.failureCount.Store(0)

	case StateHalfOpen:
		if cb.successCount.Add(1) >= int32(cb.successThreshold) {
			cb.state.Store(int32(StateClosed))
			cb.failureCount.Store(0)
			cb.successCount.Store(0)
		}

	case StateOpen:
	}
}

func (cb *CircuitBreaker) RecordFailure() {
	state := CircuitBreakerState(cb.state.Load())

	switch state {
	case StateClosed:
		cb.failureCount.Add(1)
		cb.lastFailureTime.Store(time.Now().UnixNano())
		if cb.failureCount.Load() >= int32(cb.failureThreshold) {
			if cb.state.CompareAndSwap(int32(StateClosed), int32(StateOpen)) {
				cb.trippedCount.Add(1)
			}
		}

	case StateHalfOpen:
		if cb.state.CompareAndSwap(int32(StateHalfOpen), int32(StateOpen)) {
			cb.lastFailureTime.Store(time.Now().UnixNano())
			cb.trippedCount.Add(1)
		}

	case StateOpen:
		cb.lastFailureTime.Store(time.Now().UnixNano())
	}
}

// GetState 获取熔断器状态
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	return CircuitBreakerState(cb.state.Load())
}

// GetStateString 获取熔断器状态字符串
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

	cb.state.Store(int32(StateClosed))
	cb.failureCount.Store(0)
	cb.successCount.Store(0)
	cb.lastFailureTime.Store(time.Now().UnixNano())
}

// maxBreakers 熔断器条数上限。routeKey 可来自已认证连接的任意 cmd
// （routeKeyCache 写满后 bypass 路径仍为每个新 cmd 产出独立字符串），
// 按需创建若无上界，与 routeKeyCache 同一攻击面换层泄漏。业务 cmd 数
// 远小于上限；写满后新 name 不再创建（返回 nil = 无熔断，调用方判空后
// 按放行处理，与未启用同路径），已驻留 name 不受影响。
const maxBreakers = 1 << 16

// CircuitBreakerManager 熔断器管理器（条数有界，见 maxBreakers）。
type CircuitBreakerManager struct {
	breakers sync.Map
	count    atomic.Int64
}

func NewCircuitBreakerManager() *CircuitBreakerManager {
	return &CircuitBreakerManager{}
}

func (cbm *CircuitBreakerManager) GetCircuitBreaker(name string, failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	if actual, ok := cbm.breakers.Load(name); ok {
		return actual.(*CircuitBreaker)
	}
	// 占位后再 LoadOrStore：并发首建各占一个名额，保证计数不越上限
	//（先查后存的写法在并发下会短暂超限）。
	for {
		n := cbm.count.Load()
		if n >= maxBreakers {
			return nil
		}
		if cbm.count.CompareAndSwap(n, n+1) {
			break
		}
	}
	breaker := NewCircuitBreaker(name, failureThreshold, successThreshold, timeout)
	actual, loaded := cbm.breakers.LoadOrStore(name, breaker)
	if loaded {
		// 并发下他人已抢先创建同名条目，释放多占的名额
		cbm.count.Add(-1)
		return actual.(*CircuitBreaker)
	}
	return breaker
}

func (cbm *CircuitBreakerManager) GetBreaker(name string) (*CircuitBreaker, bool) {
	if breaker, ok := cbm.breakers.Load(name); ok {
		return breaker.(*CircuitBreaker), true
	}
	return nil, false
}

func (cbm *CircuitBreakerManager) RemoveBreaker(name string) {
	if _, loaded := cbm.breakers.LoadAndDelete(name); loaded {
		cbm.count.Add(-1)
	}
}

// GetTrippedCount 返回所有熔断器累计触发次数
func (cbm *CircuitBreakerManager) GetTrippedCount() int64 {
	var total int64
	cbm.breakers.Range(func(_, v any) bool {
		if breaker, ok := v.(*CircuitBreaker); ok {
			total += breaker.trippedCount.Load()
		}
		return true
	})
	return total
}

func (cbm *CircuitBreakerManager) ListBreakers() map[string]*CircuitBreaker {
	breakers := make(map[string]*CircuitBreaker)
	cbm.breakers.Range(func(key, value any) bool {
		breakers[key.(string)] = value.(*CircuitBreaker)
		return true
	})
	return breakers
}
