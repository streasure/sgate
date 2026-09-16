package internal

import (
	"github.com/streasure/util/component"
)

// GatewayRuntimeComponent 管理网关运行时资源的生命周期：
// overloadProtector、messageIntegrity、tracer 等在 StartServices 中创建的后台 goroutine。
//
// 必须排在安全(100)、观测(200)、流量(300)、集群(400) 之后，
// 确保所有依赖已就绪；同时在 Gateway.Close() 中最先销毁（逆序），
// 确保后台 goroutine 先于连接排空和传输层停止。
type GatewayRuntimeComponent struct {
	component.BaseComponent
	gateway *Gateway
}

func NewGatewayRuntimeComponent(gateway *Gateway) *GatewayRuntimeComponent {
	return &GatewayRuntimeComponent{gateway: gateway}
}

func (c *GatewayRuntimeComponent) Name() string { return "gateway-runtime" }
func (c *GatewayRuntimeComponent) Order() int   { return 500 }
func (c *GatewayRuntimeComponent) Init() error  { return nil }

func (c *GatewayRuntimeComponent) Start() error {
	// Cluster resources are created by ClusterComponent immediately before this
	// component starts, so bind them after the sub-container has advanced to it.
	g := c.gateway
	g.serviceDiscovery = g.clusterComponent.Discovery
	g.gatewayDiscovery = g.clusterComponent.GatewayDiscovery
	g.gatewayEvents = g.clusterComponent.GatewayEvents()
	g.balancer = g.clusterComponent.Balancer
	g.configCenter = g.clusterComponent.ConfigCenter
	g.cluster = g.clusterComponent.Cluster
	g.alertWebhook = g.clusterComponent.AlertWebhook
	c.gateway.StartServices()
	return nil
}

func (c *GatewayRuntimeComponent) Destroy() {
	g := c.gateway
	if g.overloadProtector != nil {
		g.overloadProtector.Stop()
	}
	if g.tracer != nil {
		g.tracer.Stop()
	}
	if g.messageIntegrity != nil {
		g.messageIntegrity.Stop()
	}
}
