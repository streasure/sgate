package component

import (
	"context"
	"fmt"

	json "github.com/bytedance/sonic"

	"github.com/streasure/protocol/enums"
	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/component"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/udiscovery"
	"github.com/streasure/util/uetcd"
)

type ClusterComponent struct {
	component.BaseComponent
	ctx          context.Context
	cfg          config.Config
	grpcPort     int
	discovery    *uetcd.Component
	configCenter clusterPkg.ConfigCenter
	alertWebhook *clusterPkg.AlertWebhook
}

// NewClusterComponent 创建集群组件，配置从 config.Get() 读取。
func NewClusterComponent() *ClusterComponent {
	cfg := config.Get()
	return &ClusterComponent{
		ctx:      context.Background(),
		cfg:      *cfg,
		grpcPort: cfg.GRPC.Port,
	}
}
func (c *ClusterComponent) Name() string { return "cluster" }
func (c *ClusterComponent) Order() int   { return 400 }

func (c *ClusterComponent) Init() error {
	if c.cfg.ConfigCenter.Enabled {
		// 配置中心后端是 etcd；etcd 默认接入，直接创建
		c.configCenter = clusterPkg.NewConfigCenter(c.cfg.ConfigCenter, c.cfg.Etcd)
	}
	if c.cfg.Alert.Enabled {
		c.alertWebhook = clusterPkg.NewAlertWebhook(c.cfg.Alert)
	}
	setClusterInitResources(c.configCenter, c.alertWebhook)
	return nil
}

// buildRegisterAddress 根据配置构建 JSON 格式的注册地址（契约见 netutil.RegisterAddress）。
func buildRegisterAddress(cfg config.Config, grpcPort int) string {
	ip := netutil.OutboundIPv4()
	addr := netutil.RegisterAddress{IP: ip, GRPC: grpcPort}
	for _, t := range cfg.Transports {
		hostPort := netutil.JoinIPPort(ip, t.Port)
		switch t.Type {
		case "websocket":
			addr.WebSocket = hostPort
		default:
			if addr.TCP == "" {
				addr.TCP = hostPort
			}
		}
	}
	data, _ := json.Marshal(addr)
	return string(data)
}

func (c *ClusterComponent) Start() error {
	// etcd 组件仅服务网关自身注册（供 loginserver 发现连接地址）；
	// 逻辑服务发现与 loginserver 发现已由消费方按需自建（cluster 不再代持）。
	if !c.cfg.Discovery.RegisterSelf {
		tlog.Info(c.ctx, "etcd 自注册未启用（registerSelf=false），跳过 etcd 注册")
		return nil
	}

	etcdCfg := c.cfg.Uetcd()
	advertiseAddr := buildRegisterAddress(c.cfg, c.grpcPort)
	// ServiceID 格式: {belong}/{serverType}:{zone}，etcd key: /services/{belong}/{serverType}:{zone}/{instanceId}
	serviceID := udiscovery.JoinServiceID(c.cfg.Belong, enums.ServerType_SERVER_TYPE_SGATE, c.cfg.Zone)
	compCfg := uetcd.ComponentConfig{
		Etcd: etcdCfg,
		Registration: uetcd.RegistrationConfig{
			ServiceID:  serviceID,
			InstanceID: c.cfg.ServerID,
			Address:    advertiseAddr,
			LeaseTTL:   c.cfg.Etcd.LeaseTTL,
		},
	}
	c.discovery = uetcd.New(compCfg)
	if err := c.discovery.Start(); err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}
	tlog.Info(c.ctx, "etcd 注册成功 serviceID=%s instanceID=%s address=%s",
		serviceID,
		c.cfg.ServerID,
		advertiseAddr)

	tlog.Info(c.ctx, "cluster component started serverType=%s serverID=%s zone=%s",
		c.cfg.ServerType,
		c.cfg.ServerID,
		c.cfg.Zone)
	return nil
}

func (c *ClusterComponent) Destroy() {
	if c.discovery != nil {
		c.discovery.Destroy()
	}
	if c.configCenter != nil {
		c.configCenter.Stop()
	}
}
