package dataplane

import (
	"errors"

	protoGw "github.com/streasure/protocol/gateway"
)

// ErrNotConnected 表示逻辑链路未连接。定义在消费侧（dataplane）：消息管道用它
// 构造转发失败结果，logiclink 发送路径返回同一哨兵值（别名引用，errors.Is 恒等），
// 使 dataplane 不必反向依赖 logiclink。
var ErrNotConnected = errors.New("未连接到逻辑服")

// LogicClientProvider 定义逻辑客户端的连接状态和消息发送能力。
type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protoGw.StreamData) error
}

// LogicLink 定义单条逻辑链路的业务视图（*logiclink.LogicClient 实现，
// 组合方在构造/启动期注入 Engine；dataplane 只消费接口不依赖 logiclink）。
type LogicLink interface {
	IsConnected() bool
	Close()
}

// LogicLinkPool 定义逻辑客户端池的业务视图（*logiclink.LogicClientPool 实现，
// 组合方在 Start 期注入 Engine）。GetClient 返回 LogicClientProvider 供
// 消息管道向指定逻辑服转发；ZoneOf 供维护态判定目标服所属分组。
type LogicLinkPool interface {
	IsConnected() bool
	GetClient(serverID string) LogicClientProvider
	ZoneOf(serverID string) string
	Close()
}

// UDPDownlink 定义下行 UDP 发送能力（consumer-defined interface，同
// LogicClientProvider 模式，由 *Engine 实现、ASSOCIATE 成功时注入，
// 不产生 connection → dataplane 反向依赖）。
// 无关联 / 关联不新鲜 / 发送失败一律返回错误——调用方 SendByCmd 回落 WS，
// 降级不丢是硬要求（docs/udp-ws-design.md §5.3）。
type UDPDownlink interface {
	SendUDP(sessionID string, frame []byte) error
}
