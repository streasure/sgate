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
	clusterMode := c.cfg.Cluster.Mode
	if clusterMode == "" {
		clusterMode = "standalone"
	}

	etcdCfg := etcd.Config{Endpoints: c.cfg.Etcd.Endpoints, Endpoint: c.cfg.Etcd.Endpoint, Username: c.cfg.Etcd.Username, Password: c.cfg.Etcd.Password, ServicePrefix: c.cfg.Etcd.ServicePrefix}
	if c.cfg.Etcd.Enabled {
		compCfg := etcd.ComponentConfig{Enabled: true, Etcd: etcdCfg}

		// 逻辑服务发现（单体和集群模式均需要）
		if c.cfg.Discovery.Enabled {
			compCfg.Discovery = etcd.DiscoveryConfig{Enabled: true, ServiceID: "Logic:" + c.cfg.Zone}
		}

		// 集群模式下注册网关自身，供其他网关发现
		if clusterMode == "cluster" {
			advertiseAddr := fmt.Sprintf("%s:%d", netutil.GetOutboundIPv4(), c.grpcPort)
			compCfg.Registration = etcd.RegistrationConfig{
				Enabled:    true,
				ServiceID:  c.cfg.ServerType + ":" + c.cfg.Zone,
				InstanceID: c.cfg.ServerID,
				Address:    advertiseAddr,
				LeaseTTL:   c.cfg.Etcd.LeaseTTL,
			}
		}

		c.Discovery = etcd.New(compCfg)
		if err := c.Discovery.Start(); err != nil {
			return fmt.Errorf("start etcd: %w", err)
		}

		if clusterMode == "cluster" {
			advertiseAddr := fmt.Sprintf("%s:%d", netutil.GetOutboundIPv4(), c.grpcPort)
			tlog.Info("etcd 注册成功（集群模式）",
				"serviceID", c.cfg.ServerType+":"+c.cfg.Zone,
				"instanceID", c.cfg.ServerID,
				"address", advertiseAddr)
		} else {
			tlog.Info("etcd 逻辑服务发现已启动（单体模式，网关自身不注册）",
				"serviceID", "Logic:"+c.cfg.Zone)
		}

		// 网关间发现仅在集群模式下启用
		if clusterMode == "cluster" && c.cfg.Discovery.GatewayDiscovery {
			gwCompCfg := etcd.ComponentConfig{
				Enabled: true,
				Etcd:    etcdCfg,
				Discovery: etcd.DiscoveryConfig{
					Enabled:   true,
					ServiceID: "Gateway:" + c.cfg.Zone,
				},
			}
			c.GatewayDiscovery = etcd.New(gwCompCfg)
			c.GatewayDiscovery.OnServiceChange(func(event etcd.ServiceEvent) {
				c.gatewayEventsMu.Lock()
				c.gatewayEvents = append(c.gatewayEvents, event)
				c.gatewayEventsMu.Unlock()
			})
			if err := c.GatewayDiscovery.Start(); err != nil {
				tlog.Warn("网关间发现启动失败，网关间协作已禁用", "error", err)
				c.GatewayDiscovery = nil
			} else {
				tlog.Info("网关间发现已启动", "serviceID", "Gateway:"+c.cfg.Zone)
			}
		}
	}

	// Leader 选举仅在集群模式下启用
	if clusterMode == "cluster" {
		c.Cluster = clusterPkg.NewCluster(c.cfg.Cluster, c.cfg.ServerID, c.cfg.ServerType, c.cfg.Zone)
		c.Cluster.Start()
	}

	tlog.Info("集群组件已启动",
		"mode", clusterMode,
		"serverType", c.cfg.ServerType,
		"serverID", c.cfg.ServerID,
		"zone", c.cfg.Zone)
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
