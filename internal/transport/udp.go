package transport

import (
	"fmt"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
)

// udpProtocol UDP 协议插件实现。
type udpProtocol struct{}

func init() {
	Register(&udpProtocol{})
}

func (p *udpProtocol) Name() string {
	return "udp"
}

func (p *udpProtocol) Start(handler gnet.EventHandler, transport config.Transport, cfg *config.Config) error {
	addr := fmt.Sprintf("udp://:%d", transport.Port)
	options := []gnet.Option{
		gnet.WithMulticore(true),
		gnet.WithReusePort(true),
		gnet.WithSocketRecvBuffer(4 * 1024 * 1024),
		gnet.WithSocketSendBuffer(4 * 1024 * 1024),
		// UDP 无需 Ticker（无连接心跳，动作包即心跳）
		// UDP 安全三件套：maxDatagram 限制单包大小（防内存攻击，默认 1400B MTU 安全值）
		gnet.WithReadBufferCap(1400),
	}

	return gnet.Run(handler, addr, options...)
}
