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
	"github.com/streasure/util/uetcd"
)

// serverTypeName 返回 protocol enums 中的 ServerType 字符串名。
func serverTypeName(t enums.ServerType) string {
	return enums.ServerType_name[int32(t)]
}

type ClusterComponent struct {
	component.BaseComponent
	ctx            context.Context
	cfg            config.Config
	grpcPort       int
	discovery      *uetcd.Component
	loginDiscovery *uetcd.Component // 发现指定 zone 的 loginserver
	configCenter   clusterPkg.ConfigCenter
	alertWebhook   *clusterPkg.AlertWebhook
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

// registerAddress 是 etcd 注册地址的 JSON 结构，包含网关所有连接地址。
type registerAddress struct {
	IP        string `json:"ip"`
	GRPC      int    `json:"grpc"`
	TCP       string `json:"tcp,omitempty"`
	WebSocket string `json:"websocket,omitempty"`
}

// buildRegisterAddress 根据配置构建 JSON 格式的注册地址。
func buildRegisterAddress(cfg config.Config, grpcPort int) string {
	ip := netutil.OutboundIPv4()
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
	etcdCfg := uetcd.Config{Endpoints: c.cfg.Etcd.Endpoints, Endpoint: c.cfg.Etcd.Endpoint, Username: c.cfg.Etcd.Username, Password: c.cfg.Etcd.Password, ServicePrefix: c.cfg.Etcd.ServicePrefix}
	compCfg := uetcd.ComponentConfig{Etcd: etcdCfg}

	// 逻辑服务发现
	if c.cfg.Discovery.Enabled {
		logicType := serverTypeName(enums.ServerType_SERVER_TYPE_LOGICSERVER)
		compCfg.Discovery = uetcd.DiscoveryConfig{ServiceID: c.cfg.Belong + "/" + logicType + ":" + c.cfg.Zone}
	}

	// 注册网关自身，供 loginserver 发现连接地址（registerSelf 控制）
	// ServiceID 使用 protocol enums 的 SERVER_TYPE_SGATE 名称
	sgateType := serverTypeName(enums.ServerType_SERVER_TYPE_SGATE)
	if c.cfg.Discovery.RegisterSelf {
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

	c.discovery = uetcd.New(compCfg)
	if err := c.discovery.Start(); err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}

	if compCfg.Registration.ServiceID != "" {
		advertiseAddr := buildRegisterAddress(c.cfg, c.grpcPort)
		tlog.Info(c.ctx, "etcd 注册成功 serviceID=%s instanceID=%s address=%s",
			compCfg.Registration.ServiceID,
			c.cfg.ServerID,
			advertiseAddr)
	} else {
		tlog.Info(c.ctx, "etcd 逻辑服务发现已启动（网关自身不注册） serviceID=%s", compCfg.Discovery.ServiceID)
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
	c.loginDiscovery = uetcd.New(loginCompCfg)
	if err := c.loginDiscovery.Start(); err != nil {
		return fmt.Errorf("start loginserver discovery: %w", err)
	}
	tlog.Info(c.ctx, "loginserver discovery started serviceID=%s", loginServiceID)

	setClusterStartResources(c.discovery, c.loginDiscovery)

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
	if c.loginDiscovery != nil {
		c.loginDiscovery.Destroy()
	}
	if c.configCenter != nil {
		c.configCenter.Stop()
	}
}
