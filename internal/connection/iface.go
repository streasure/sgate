package connection

import (
	protoGw "github.com/streasure/protocol/gateway"
)

// LogicClientProvider 定义逻辑客户端的连接状态和消息发送能力。
type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protoGw.StreamData) error
}
