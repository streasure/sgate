package component

import (
	"context"
	"fmt"

	json "github.com/bytedance/sonic"

	"github.com/streasure/protocol/enums"
	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/netutil"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
)

// serverTypeName 返回 protocol enums 中的 ServerType 字符串名。
func serverTypeName(t enums.ServerType) string {
	return enums.ServerType_name[int32(t)]
}

type ClusterComponent struct {
	component.BaseComponent
	cfg              config.Config
	grpcPort         int
	Discovery        *uetcd.Component
	GatewayDiscovery *uetcd.Component // 发现同一可用区内的其他网关。
	LoginDiscovery   *uetcd.Component // 发现指定 zone 的 loginserver。
	Balancer         *clusterPkg.Balancer
	ConfigCenter     clusterPkg.ConfigCenter
	Cluster          *clusterPkg.Cluster
	AlertWebhook     *clusterPkg.AlertWebhook
}

// NewClusterComponent 创建集群组件，配置从 config.Get() 读取。
func NewClusterComponent() *ClusterComponent {
	cfg := config.Get()
	return &ClusterComponent{
		cfg:      *cfg,
		grpcPort: cfg.GRPC.Port,
	}
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
	setClusterInitResources(c.Balancer, c.ConfigCenter, c.AlertWebhook)
	return nil
}

// registerAddress 是 etcd 注册地址的 JSON 结构，包含网关所有连接地址。
type registerAddress struct {
	IP        string `json:"ip"`
	GRPC      int    `json:"grpc"`
	TCP       string `json:"tcp,omitempty"`
	WebSocket string `json:"websocket,omitempty"`
}

// buildRegisterAddress 根据配置构建 JSON 格式的注册地址。
func buildRegisterAddress(cfg config.Config, grpcPort int) string {
	ip := netutil.GetOutboundIPv4()
	addr := registerAddress{IP: ip, GRPC: grpcPort}
	for _, t := range cfg.Transports {
		hostPort := fmt.Sprintf("%s:%d", ip, t.Port)
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
	clusterMode := c.cfg.Cluster.Mode
	if clusterMode == "" {
		clusterMode = "standalone"
	}

	etcdCfg := uetcd.Config{Endpoints: c.cfg.Etcd.Endpoints, Endpoint: c.cfg.Etcd.Endpoint, Username: c.cfg.Etcd.Username, Password: c.cfg.Etcd.Password, ServicePrefix: c.cfg.Etcd.ServicePrefix}
	compCfg := uetcd.ComponentConfig{Etcd: etcdCfg}

	// 逻辑服务发现（单体和集群模式均需要）
	if c.cfg.Discovery.Enabled {
		logicType := serverTypeName(enums.ServerType_SERVER_TYPE_LOGICSERVER)
		compCfg.Discovery = uetcd.DiscoveryConfig{ServiceID: c.cfg.Belong + "/" + logicType + ":" + c.cfg.Zone}
	}

	// 注册网关自身，供 loginserver 等服务发现连接地址
	// cluster 模式始终注册；standalone 模式由 RegisterSelf 控制
	// ServiceID 使用 protocol enums 的 SERVER_TYPE_SGATE 名称
	sgateType := serverTypeName(enums.ServerType_SERVER_TYPE_SGATE)
	if clusterMode == "cluster" || c.cfg.Discovery.RegisterSelf {
		advertiseAddr := buildRegisterAddress(c.cfg, c.grpcPort)
		// ServiceID 格式: {belong}/{serverType}:{zone}，etcd key: /services/{belong}/{serverType}:{zone}/{instanceId}
		serviceID := c.cfg.Belong + "/" + sgateType + ":" + c.cfg.Zone
		compCfg.Registration = uetcd.RegistrationConfig{
			ServiceID:  serviceID,
			InstanceID: c.cfg.ServerID,
			Address:    advertiseAddr,
			LeaseTTL:   c.cfg.Etcd.LeaseTTL,
		}
	}

	c.Discovery = uetcd.New(compCfg)
	if err := c.Discovery.Start(); err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}

	if compCfg.Registration.ServiceID != "" {
		advertiseAddr := buildRegisterAddress(c.cfg, c.grpcPort)
		tlog.Info(context.TODO(), "etcd 注册成功 serviceID=%s instanceID=%s address=%s mode=%s",
			compCfg.Registration.ServiceID,
			c.cfg.ServerID,
			advertiseAddr,
			clusterMode)
	} else {
		tlog.Info(context.TODO(), "etcd 逻辑服务发现已启动（网关自身不注册） serviceID=%s", compCfg.Discovery.ServiceID)
	}

	// 网关间发现仅在集群模式下启用（与自身注册同一 ServerType 枚举名）
	if clusterMode == "cluster" && c.cfg.Discovery.GatewayDiscovery {
		gwServiceID := c.cfg.Belong + "/" + sgateType + ":" + c.cfg.Zone
		gwCompCfg := uetcd.ComponentConfig{
			Etcd: etcdCfg,
			Discovery: uetcd.DiscoveryConfig{
				ServiceID: gwServiceID,
			},
		}
		c.GatewayDiscovery = uetcd.New(gwCompCfg)
		c.GatewayDiscovery.OnServiceChange(appendGatewayEvent)
		if err := c.GatewayDiscovery.Start(); err != nil {
			tlog.Warn(context.TODO(), "网关间发现启动失败，网关间协作已禁用 error=%v", err)
			c.GatewayDiscovery = nil
		} else {
			tlog.Info(context.TODO(), "网关间发现已启动 serviceID=%s", gwServiceID)
		}
	}

	// loginserver 发现固定使用 protocol enums 的 SERVER_TYPE_LOGINSERVER，zone 与网关一致
	loginType := serverTypeName(enums.ServerType_SERVER_TYPE_LOGINSERVER)
	loginServiceID := c.cfg.Belong + "/" + loginType + ":" + c.cfg.Zone
	loginCompCfg := uetcd.ComponentConfig{
		Etcd: etcdCfg,
		Discovery: uetcd.DiscoveryConfig{
			ServiceID: loginServiceID,
		},
	}
	c.LoginDiscovery = uetcd.New(loginCompCfg)
	if err := c.LoginDiscovery.Start(); err != nil {
		return fmt.Errorf("start loginserver discovery: %w", err)
	}
	tlog.Info(context.TODO(), "loginserver discovery started serviceID=%s", loginServiceID)

	// Leader 选举仅在集群模式下启用
	if clusterMode == "cluster" {
		c.Cluster = clusterPkg.NewCluster(c.cfg.Cluster, c.cfg.ServerID, c.cfg.ServerType, c.cfg.Zone)
		c.Cluster.Start()
	}

	setClusterStartResources(c.Discovery, c.GatewayDiscovery, c.LoginDiscovery, c.Cluster)

	tlog.Info(context.TODO(), "集群组件已启动 mode=%s serverType=%s serverID=%s zone=%s",
		clusterMode,
		c.cfg.ServerType,
		c.cfg.ServerID,
		c.cfg.Zone)
	return nil
}

func (c *ClusterComponent) Destroy() {
	if c.Discovery != nil {
		c.Discovery.Destroy()
	}
	if c.GatewayDiscovery != nil {
		c.GatewayDiscovery.Destroy()
	}
	if c.LoginDiscovery != nil {
		c.LoginDiscovery.Destroy()
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
