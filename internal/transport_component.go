package gateway

import (
	"fmt"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

type TransportComponent struct {
	component.BaseComponent
	gw         *Gateway
	transports []config.Transport
}

func NewTransportComponent(gw *Gateway, transports []config.Transport) *TransportComponent {
	return &TransportComponent{gw: gw, transports: transports}
}

func (c *TransportComponent) SetGateway(gw *Gateway) { c.gw = gw }

func (c *TransportComponent) Name() string { return "gateway-transports" }
func (c *TransportComponent) Order() int   { return 500 }

func (c *TransportComponent) Init() error { return nil }

func (c *TransportComponent) Start() error {
	c.StartTransports()
	return nil
}

func (c *TransportComponent) StartTransports() {
	if c.gw == nil {
		tlog.Warn("transport component: gateway is nil, skipping transport start")
		return
	}
	for _, transport := range c.transports {
		port := fmt.Sprintf("%d", transport.Port)
		transportType := transport.Type
		c.gw.SetTransportType(port, transportType)

		// TCP and WebSocket share gnet's stream transport.
		addr := fmt.Sprintf("%s://:%d", transport.Protocol, transport.Port)
		go func(addr, transportType string) {
			options := []gnet.Option{
				gnet.WithMulticore(true),
				gnet.WithReusePort(true),
				gnet.WithReadBufferCap(262144),
				gnet.WithWriteBufferCap(262144),
				gnet.WithSocketRecvBuffer(4 * 1024 * 1024),
				gnet.WithSocketSendBuffer(4 * 1024 * 1024),
			}
			if transportType == "" || transportType == "websocket" {
				options = append(options, gnet.WithTCPNoDelay(gnet.TCPNoDelay))
			}
			tlog.Info("starting gateway transport", "addr", addr, "type", transportType)
			if err := gnet.Run(c.gw, addr, options...); err != nil {
				tlog.Error("gateway transport stopped", "addr", addr, "error", err)
			}
		}(addr, transportType)
	}
}

func (c *TransportComponent) Destroy() {
	if c.gw != nil {
		c.gw.Close()
	}
}
