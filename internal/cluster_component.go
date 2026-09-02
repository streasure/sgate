package gateway

import (
	"context"
	"fmt"

	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/component"
	"github.com/streasure/util/nacos"
	"github.com/streasure/util/tlog"
)

// ClusterComponent manages the lifecycle of all cluster sub-modules:
// ServiceDiscovery, Balancer, ConfigCenter, Cluster, AlertWebhook.
type ClusterComponent struct {
	component.BaseComponent

	cfg       config.Config
	grpcPort  int
	logicAddr string
	grpcFunc  func(addr string) // function to pre-warm logic connection

	Discovery    *nacos.Discovery
	Balancer     *clusterPkg.Balancer
	ConfigCenter clusterPkg.ConfigCenter
	Cluster      *clusterPkg.Cluster
	AlertWebhook *clusterPkg.AlertWebhook
}

func NewClusterComponent(cfg config.Config, grpcPort int, grpcFunc func(addr string)) *ClusterComponent {
	return &ClusterComponent{
		cfg:       cfg,
		grpcPort:  grpcPort,
		logicAddr: cfg.GRPC.LogicAddr,
		grpcFunc:  grpcFunc,
	}
}

func (c *ClusterComponent) Name() string { return "cluster" }
func (c *ClusterComponent) Order() int   { return 400 }

func (c *ClusterComponent) Init() error {
	tlog.Info("cluster component init")

	// Balancer
	c.Balancer = clusterPkg.NewBalancer(c.cfg.Balancer)

	// Config Center
	if c.cfg.ConfigCenter.Enabled {
		c.ConfigCenter = clusterPkg.NewConfigCenter(c.cfg.ConfigCenter)
	}

	// Alert Webhook
	if c.cfg.Alert.Enabled {
		c.AlertWebhook = clusterPkg.NewAlertWebhook(c.cfg.Alert)
	}

	return nil
}

func (c *ClusterComponent) Start() error {
	tlog.Info("cluster component starting")

	// Start Service Discovery
	if c.cfg.Discovery.Enabled && c.cfg.ConfigCenter.Endpoint != "" {
		c.Discovery = nacos.NewDiscovery(nacos.DiscoveryConfig{
			Enabled: true,
			Nacos: nacos.Config{
				Endpoint:       c.cfg.ConfigCenter.Endpoint,
				NamingEndpoint: c.cfg.ConfigCenter.NamingEndpoint,
				Namespace:      c.cfg.ConfigCenter.Namespace,
				Group:          c.cfg.ConfigCenter.Group,
				Username:       c.cfg.ConfigCenter.Username,
				Password:       c.cfg.ConfigCenter.Password,
				APIVersion:     c.cfg.ConfigCenter.APIVersion,
			},
			Service: nacos.NamingConfig{
				ServiceName: c.cfg.Discovery.ServiceName,
			},
			Zone: c.cfg.Discovery.Zone,
		})
		if err := c.Discovery.Start(); err != nil {
			tlog.Error("service discovery start failed", "error", err)
		}
		tlog.Info("service discovery enabled (nacos)",
			"endpoint", c.cfg.ConfigCenter.Endpoint,
			"serviceName", c.cfg.Discovery.ServiceName,
		)
	}

	// Start Cluster (Nacos registration + leader election)
	if c.cfg.Cluster.Enabled && c.cfg.ConfigCenter.Endpoint != "" {
		zone := c.cfg.Zone
		if zone == "" {
			zone = "default"
		}
		c.Cluster = clusterPkg.NewCluster(c.cfg.Cluster, nacos.Config{
			Endpoint:       c.cfg.ConfigCenter.Endpoint,
			NamingEndpoint: c.cfg.ConfigCenter.NamingEndpoint,
			Namespace:      c.cfg.ConfigCenter.Namespace,
			Group:          c.cfg.ConfigCenter.Group,
			Username:       c.cfg.ConfigCenter.Username,
			Password:       c.cfg.ConfigCenter.Password,
			APIVersion:     c.cfg.ConfigCenter.APIVersion,
		}, zone, c.grpcPort)
		c.Cluster.Start(context.Background())
	}

	// Static connection pre-warm (no discovery)
	if c.Discovery == nil {
		tlog.Info("service discovery disabled, using static logic server connection")
		if c.grpcFunc != nil {
			addr := c.logicAddr
			if addr == "" {
				addr = fmt.Sprintf("localhost:%d", c.grpcPort)
			}
			go c.grpcFunc(addr)
		}
	}

	return nil
}

func (c *ClusterComponent) Destroy() {
	tlog.Info("cluster component destroying")
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
