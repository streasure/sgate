//go:build legacy

package gateway

import (
	"fmt"

	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/component"
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/tlog"
)

type ClusterComponent struct {
	component.BaseComponent
	cfg          config.Config
	grpcPort     int
	grpcFunc     func(addr string)
	Discovery    *etcd.Component
	Balancer     *clusterPkg.Balancer
	ConfigCenter clusterPkg.ConfigCenter
	Cluster      *clusterPkg.Cluster
	AlertWebhook *clusterPkg.AlertWebhook
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
	if c.cfg.Etcd.Enabled && c.cfg.Discovery.Enabled {
		c.Discovery = etcd.New(etcd.ComponentConfig{Enabled: true, Etcd: etcdCfg, Discovery: etcd.DiscoveryConfig{Enabled: true, ServiceID: "Logic:" + c.cfg.Zone}})
		if err := c.Discovery.Start(); err != nil {
			return fmt.Errorf("start etcd discovery: %w", err)
		}
	}
	c.Cluster = clusterPkg.NewCluster(c.cfg.Cluster, c.cfg.ServerID, c.cfg.ServerType, c.cfg.Zone)
	c.Cluster.Start()
	if c.Discovery == nil && c.grpcFunc != nil && c.cfg.GRPC.LogicAddr != "" {
		go c.grpcFunc(c.cfg.GRPC.LogicAddr)
	}
	tlog.Info("etcd service discovery configured", "serverType", c.cfg.ServerType, "serverID", c.cfg.ServerID, "zone", c.cfg.Zone)
	return nil
}

func (c *ClusterComponent) Destroy() {
	if c.Discovery != nil {
		c.Discovery.Destroy()
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
