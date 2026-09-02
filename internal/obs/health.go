package obs

import (
	"time"
)

// HealthStatus 健康状态
type HealthStatus struct {
	Status    string           `json:"status"` // healthy, unhealthy, degraded
	Timestamp time.Time        `json:"timestamp"`
	Version   string           `json:"version"`
	Uptime    time.Duration    `json:"uptime"`
	Checks    map[string]Check `json:"checks"`
	Metrics   HealthMetrics    `json:"metrics"`
}

// Check 健康检查项
type Check struct {
	Status  string `json:"status"` // pass, fail, warn
	Message string `json:"message,omitempty"`
	Latency int64  `json:"latency_ms,omitempty"` // 延迟（毫秒）
}

// HealthMetrics 健康指标
type HealthMetrics struct {
	Connections    int     `json:"connections"`      // 当前连接数
	Goroutines     int     `json:"goroutines"`       // Goroutine数量
	MemoryAlloc    uint64  `json:"memory_alloc_mb"`  // 内存分配（MB）
	MemorySys      uint64  `json:"memory_sys_mb"`    // 系统内存（MB）
	GCCount        uint32  `json:"gc_count"`         // GC次数
	MessagesPerSec float64 `json:"messages_per_sec"` // 每秒消息数
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
