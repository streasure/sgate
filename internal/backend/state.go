package backend

import (
	"errors"
	"time"
)

type LogicConnectionState int32

const (
	LogicStateDisconnected LogicConnectionState = iota // 未连接
	LogicStateConnecting                               // 连接中
	LogicStateConnected                                // 已连接
	LogicStateReconnecting                             // 重连中
)

func (s LogicConnectionState) String() string {
	switch s {
	case LogicStateDisconnected:
		return "Disconnected"
	case LogicStateConnecting:
		return "Connecting"
	case LogicStateConnected:
		return "Connected"
	case LogicStateReconnecting:
		return "Reconnecting"
	default:
		return "Unknown"
	}
}

// 错误定义
var (
	ErrNotConnected      = errors.New("未连接到逻辑服")
	ErrConnectionClosing = errors.New("连接正在关闭")
	ErrQueueFull         = errors.New("发送队列已满")
	ErrSendTimeout       = errors.New("发送超时")
	ErrBackpressure      = errors.New("背压已激活")
)

// ReconnectConfig 重连配置，控制指数退避策略
type ReconnectConfig struct {
	InitialInterval time.Duration // 初始重连间隔
	MaxInterval     time.Duration // 最大重连间隔
	MaxAttempts     int           // 最大重连尝试次数（0表示无限）
	Multiplier      float64       // 退避倍数
}

// DefaultReconnectConfig 默认重连配置
var DefaultReconnectConfig = ReconnectConfig{
	InitialInterval: 1 * time.Second,
	MaxInterval:     30 * time.Second,
	MaxAttempts:     0,
	Multiplier:      2.0,
}

// HealthCheckConfig 健康检查配置
type HealthCheckConfig struct {
	Interval    time.Duration // 检查间隔
	Timeout     time.Duration // 超时时间
	MaxFailures int           // 最大失败次数，超过则触发重连
	// Enabled 控制是否对逻辑服做主动健康检查（ping）。
	// 默认 true：主动 ping 并在连续失败超阈值后重连，保障容灾切换。
	Enabled bool
}

// DefaultHealthCheckConfig 默认健康检查配置
var DefaultHealthCheckConfig = HealthCheckConfig{
	Interval:    5 * time.Second,
	Timeout:     3 * time.Second,
	MaxFailures: 3,
	Enabled:     true,
}

// StreamShard 单个流分片，封装一个 gRPC 流连接及其发送通道
