package protocol

import (
	"fmt"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
)

// tcpProtocol TCP 协议插件实现。
type tcpProtocol struct{}

func init() {
	Register(&tcpProtocol{})
}

func (p *tcpProtocol) Name() string {
	return "tcp"
}

func (p *tcpProtocol) Start(handler gnet.EventHandler, transport config.Transport, cfg *config.Config) error {
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
	// TCP 禁用 Nagle 算法（减少小包延迟）
	options = append(options, gnet.WithTCPNoDelay(gnet.TCPNoDelay))

	return gnet.Run(handler, addr, options...)
}
