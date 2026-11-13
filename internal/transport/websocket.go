package transport

import (
	"fmt"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
)

// websocketProtocol WebSocket 协议插件实现。
type websocketProtocol struct{}

func init() {
	Register(&websocketProtocol{})
}

func (p *websocketProtocol) Name() string {
	return "websocket"
}

func (p *websocketProtocol) Start(handler gnet.EventHandler, transport config.Transport, cfg *config.Config) error {
	addr := fmt.Sprintf("tcp://:%d", transport.Port)
	options := []gnet.Option{
		gnet.WithMulticore(true),
		gnet.WithReusePort(true),
		gnet.WithReadBufferCap(262144),
		gnet.WithWriteBufferCap(262144),
		gnet.WithSocketRecvBuffer(4 * 1024 * 1024),
		gnet.WithSocketSendBuffer(4 * 1024 * 1024),
		gnet.WithTicker(true), // OnTick 必需：msgRate 采样与 /stats 速率依赖此
	}
	// WebSocket 禁用 Nagle 算法
	options = append(options, gnet.WithTCPNoDelay(gnet.TCPNoDelay))

	return gnet.Run(handler, addr, options...)
}
