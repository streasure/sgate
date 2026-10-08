package logic

import (
	"time"
)

// ServiceConfig 逻辑层服务配置，包含身份标识、传输、拨入网关和 etcd 设置
type ServiceConfig struct {
	ListenAddr         string        `yaml:"listenAddr"`         // 监听地址
	ListenPort         string        `yaml:"listenPort"`         // 监听端口
	AdvertiseAddr      string        `yaml:"advertiseAddr"`      // 对外暴露地址
	Belong             string        `yaml:"belong"`             // 所属应用/团队标识
	ServiceID          string        `yaml:"serverId"`           // 服务实例 ID
	ServerType         string        `yaml:"serverType"`         // 服务类型
	ServiceName        string        `yaml:"serviceName"`        // 服务名称
	Zone               string        `yaml:"zone"`               // 可用区
	EtcdEndpoints      []string      `yaml:"etcdEndpoints"`      // etcd 集群端点列表
	EtcdEndpoint       string        `yaml:"etcdEndpoint"`       // etcd 单节点端点
	EtcdUsername       string        `yaml:"etcdUsername"`       // etcd 用户名
	EtcdPassword       string        `yaml:"etcdPassword"`       // etcd 密码
	EtcdServicePrefix  string        `yaml:"etcdServicePrefix"`  // etcd 服务路径前缀
	EtcdLeaseTTL       string        `yaml:"etcdLeaseTTL"`       // etcd 租约 TTL
	HeartbeatInterval  time.Duration `yaml:"heartbeatInterval"`  // 心跳间隔
	HeartbeatTTL       time.Duration `yaml:"heartbeatTTL"`       // 心跳 TTL
	GRPCWindowSize     int           `yaml:"grpcWindowSize"`     // gRPC 流控窗口大小
	GRPCMaxMessageSize int           `yaml:"grpcMaxMessageSize"` // gRPC 单条消息最大长度
	DispatchWorkers    int           `yaml:"dispatchWorkers"`    // 分发工作协程数
	DispatchChSize     int           `yaml:"dispatchChSize"`     // 分发通道大小
	StreamSendChSize   int           `yaml:"streamSendChSize"`   // 流发送通道大小
	Passthrough        bool          `yaml:"passthrough"`        // 是否直通模式
	ShardCount         int           `yaml:"shardCount"`         // 拨入每个网关的流分片数（<=0 时 NumCPU*8）
	ConnGroupCount     int           `yaml:"connGroupCount"`     // 拨入每个网关的独立 TCP 连接组数（<=0 时 4）
	Gateways           []string      `yaml:"gateways"`           // 静态网关 gRPC 地址列表（ip:port），与 etcd 发现互补
}

// defaultConfig 返回逻辑层服务的默认配置
func defaultConfig() ServiceConfig {
	return ServiceConfig{
		ListenAddr: "0.0.0.0", ListenPort: "50052", Belong: "default", ServiceID: "", ServerType: "Logic",
		ServiceName: "logic", Zone: "default", EtcdEndpoint: "http://127.0.0.1:2379",
		EtcdServicePrefix: "/services", EtcdLeaseTTL: "10s", HeartbeatInterval: 3 * time.Second,
		HeartbeatTTL: 10 * time.Second, GRPCWindowSize: 524288, GRPCMaxMessageSize: 4 * 1024 * 1024,
	}
}

// ServiceOption 服务配置选项函数
type ServiceOption func(*ServiceConfig)

// WithListenPort 设置监听端口
func WithListenPort(port string) ServiceOption { return func(c *ServiceConfig) { c.ListenPort = port } }

// WithServiceID 设置服务实例 ID
func WithServiceID(id string) ServiceOption { return func(c *ServiceConfig) { c.ServiceID = id } }

// WithServerType 设置服务类型
func WithServerType(t string) ServiceOption { return func(c *ServiceConfig) { c.ServerType = t } }

// WithServiceName 设置服务名称
func WithServiceName(name string) ServiceOption {
	return func(c *ServiceConfig) { c.ServiceName = name }
}

// WithZone 设置可用区（空值不生效）
func WithZone(zone string) ServiceOption {
	return func(c *ServiceConfig) {
		if zone != "" {
			c.Zone = zone
		}
	}
}

// WithEtcd 设置 etcd 端点
func WithEtcd(endpoint string) ServiceOption {
	return func(c *ServiceConfig) { c.EtcdEndpoint = endpoint }
}

// WithGateways 设置静态网关 gRPC 地址列表（ip:port），
// 与 etcd 服务发现互补（etcd 关闭或网关未注册时的兜底）。
func WithGateways(addrs ...string) ServiceOption {
	return func(c *ServiceConfig) { c.Gateways = append(c.Gateways, addrs...) }
}

// WithShardCount 设置拨入每个网关的流分片数。
func WithShardCount(n int) ServiceOption {
	return func(c *ServiceConfig) { c.ShardCount = n }
}
