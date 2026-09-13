package gateway

import (
	"fmt"
	"sync"

	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/netutil"
	"github.com/streasure/util/component"
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/tlog"
)

type ClusterComponent struct {
	component.BaseComponent
	cfg              config.Config
	grpcPort         int
	grpcFunc         func(addr string)
	Discovery        *etcd.Component
	GatewayDiscovery *etcd.Component // 发现同一可用区内的其他网关。
	gatewayEvents    []etcd.ServiceEvent
	gatewayEventsMu  sync.RWMutex
	Balancer         *clusterPkg.Balancer
	ConfigCenter     clusterPkg.ConfigCenter
	Cluster          *clusterPkg.Cluster
	AlertWebhook     *clusterPkg.AlertWebhook
}

func NewClusterComponent(cfg config.Config, grpcPort int, grpcFunc func(addr string)) *ClusterComponent {
	return &ClusterComponent{cfg: cfg, grpcPort: grpcPort, grpcFunc: grpcFunc}
}
func (c *ClusterComponent) Name() string { return "cluster" }
func (c *ClusterComponent) Order() int   { return 400 }

func (c *ClusterComponent) Init() error {
	c.Balancer = clusterPkg.NewBalancer(c.cfg.Balancer)
	if c.cfg.ConfigCenter.Enabled {
		c.ConfigCenter = clusterPkg.NewConfigCenter(c.cfg.ConfigCenter, c.cfg.Etcd)
	}
	if c.cfg.Alert.Enabled {
		c.AlertWebhook = clusterPkg.NewAlertWebhook(c.cfg.Alert)
	}
	return nil
}

func (c *ClusterComponent) Start() error {
	etcdCfg := etcd.Config{Endpoints: c.cfg.Etcd.Endpoints, Endpoint: c.cfg.Etcd.Endpoint, Username: c.cfg.Etcd.Username, Password: c.cfg.Etcd.Password, ServicePrefix: c.cfg.Etcd.ServicePrefix}
	if c.cfg.Etcd.Enabled {
		advertiseAddr := fmt.Sprintf("%s:%d", netutil.GetOutboundIPv4(), c.grpcPort)
		compCfg := etcd.ComponentConfig{Enabled: true, Etcd: etcdCfg}
		if c.cfg.Discovery.Enabled {
			compCfg.Discovery = etcd.DiscoveryConfig{Enabled: true, ServiceID: "Logic:" + c.cfg.Zone}
		}
		// 注册网关自身，供其他服务发现。
		compCfg.Registration = etcd.RegistrationConfig{
			Enabled:    true,
			ServiceID:  c.cfg.ServerType + ":" + c.cfg.Zone,
			InstanceID: c.cfg.ServerID,
			Address:    advertiseAddr,
			LeaseTTL:   c.cfg.Etcd.LeaseTTL,
		}
		c.Discovery = etcd.New(compCfg)
		if err := c.Discovery.Start(); err != nil {
			return fmt.Errorf("start etcd: %w", err)
		}
		tlog.Info("etcd registration succeeded",
			"serviceID", c.cfg.ServerType+":"+c.cfg.Zone,
			"instanceID", c.cfg.ServerID,
			"address", advertiseAddr)

		// 可选的网关间发现，监听 Gateway:{zone} 服务变化。
		if c.cfg.Discovery.GatewayDiscovery {
			gwCompCfg := etcd.ComponentConfig{
				Enabled: true,
				Etcd:    etcdCfg,
				Discovery: etcd.DiscoveryConfig{
					Enabled:   true,
					ServiceID: "Gateway:" + c.cfg.Zone,
				},
				// 此组件只负责发现其他网关，不重复注册自身。
			}
			c.GatewayDiscovery = etcd.New(gwCompCfg)
			// 保存服务变化快照，因为网关对象会在生命周期组件启动后才构造。
			c.GatewayDiscovery.OnServiceChange(func(event etcd.ServiceEvent) {
				c.gatewayEventsMu.Lock()
				c.gatewayEvents = append(c.gatewayEvents, event)
				c.gatewayEventsMu.Unlock()
			})
			if err := c.GatewayDiscovery.Start(); err != nil {
				tlog.Warn("gateway discovery etcd start failed, gateway-to-gateway disabled", "error", err)
				c.GatewayDiscovery = nil
			} else {
				tlog.Info("gateway discovery started", "serviceID", "Gateway:"+c.cfg.Zone)
			}
		}
	}
	c.Cluster = clusterPkg.NewCluster(c.cfg.Cluster, c.cfg.ServerID, c.cfg.ServerType, c.cfg.Zone)
	c.Cluster.Start()
	tlog.Info("cluster component started", "serverType", c.cfg.ServerType, "serverID", c.cfg.ServerID, "zone", c.cfg.Zone)
	return nil
}

func (c *ClusterComponent) GatewayEvents() []etcd.ServiceEvent {
	c.gatewayEventsMu.RLock()
	defer c.gatewayEventsMu.RUnlock()
	return append([]etcd.ServiceEvent(nil), c.gatewayEvents...)
}

func (c *ClusterComponent) Destroy() {
	if c.Discovery != nil {
		c.Discovery.Destroy()
	}
	if c.GatewayDiscovery != nil {
		c.GatewayDiscovery.Destroy()
	}
	if c.Cluster != nil {
		c.Cluster.Stop()
	}
	if c.Balancer != nil {
		c.Balancer.Stop()
	}
	if c.ConfigCenter != nil {
		c.ConfigCenter.Stop()
	}
}
