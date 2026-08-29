package gateway

import (
	"fmt"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/util/component"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// TransportComponent owns all client-facing gnet listeners.
// It is intentionally separate from Gateway so transports can be added or removed
// without changing the gateway's internal service lifecycle.
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

// Init implements component.Component. Transport has no init work.
func (c *TransportComponent) Init() error { return nil }

// Start implements component.Component. Starts gnet listeners.
func (c *TransportComponent) Start() error {
	c.StartTransports()
	return nil
}

// StartTransports launches all configured gnet listeners (TCP/UDP/WebSocket).
// Safe to call explicitly when using manual component lifecycle.
func (c *TransportComponent) StartTransports() {
	if c.gw == nil {
		tlog.Warn("transport component: gateway is nil, skipping transport start")
		return
	}
	for _, transport := range c.transports {
		addr := fmt.Sprintf("%s://:%d", transport.Protocol, transport.Port)
		port := fmt.Sprintf("%d", transport.Port)
		transportType := transport.Type
		c.gw.SetTransportType(port, transportType)

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

// Destroy implements component.Component.
func (c *TransportComponent) Destroy() {
	if c.gw != nil {
		c.gw.Close()
	}
}
