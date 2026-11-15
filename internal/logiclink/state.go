package logiclink

import (
	"errors"

	"github.com/streasure/sgate/internal/dataplane"
)

type LogicConnectionState int32

const (
	LogicStateDisconnected LogicConnectionState = iota // 未连接
	LogicStateConnected                                // 已连接
)

func (s LogicConnectionState) String() string {
	switch s {
	case LogicStateDisconnected:
		return "Disconnected"
	case LogicStateConnected:
		return "Connected"
	default:
		return "Unknown"
	}
}

// 错误定义
var (
	// ErrNotConnected 与 dataplane 共享同一哨兵值（dataplane 消费方 errors.Is 判定，
	// 依赖方向 logiclink → dataplane 单向，见 dataplane/iface.go）。
	ErrNotConnected      = dataplane.ErrNotConnected
	ErrConnectionClosing = errors.New("连接正在关闭")
	ErrQueueFull         = errors.New("发送队列已满")
	ErrSendTimeout       = errors.New("发送超时")
	ErrBackpressure      = errors.New("背压已激活")
)

// StreamShard 单个流分片，封装一个 gRPC 流连接及其发送通道
