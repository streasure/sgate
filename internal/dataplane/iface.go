package dataplane

import (
	protoGw "github.com/streasure/protocol/gateway"
)

// LogicClientProvider 定义逻辑客户端的连接状态和消息发送能力。
type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protoGw.StreamData) error
}

// UDPDownlink 定义下行 UDP 发送能力（consumer-defined interface，同
// LogicClientProvider 模式，由 *gateway.Gateway 实现、ASSOCIATE 成功时注入，
// 不产生 connection → gateway 反向依赖）。
// 无关联 / 关联不新鲜 / 发送失败一律返回错误——调用方 SendByCmd 回落 WS，
// 降级不丢是硬要求（docs/udp-ws-design.md §5.3）。
type UDPDownlink interface {
	SendUDP(sessionID string, frame []byte) error
}
