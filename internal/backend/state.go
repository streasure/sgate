package backend

import (
	"errors"
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

// StreamShard 单个流分片，封装一个 gRPC 流连接及其发送通道
