package logic

import "time"

// ServiceConfig logic server 配置
// 服务注册基于 Nacos naming API
type ServiceConfig struct {
	ListenAddr    string
	ListenPort    string
	AdvertiseAddr string
	ServiceID     string
	ServiceName   string
	Zone          string
	// Nacos 命名服务配置（服务注册）
	NacosEndpoint       string // Nacos 控制台地址（用于登录认证）
	NacosNamingEndpoint string // Nacos 主端口地址（用于实例注册/查询），为空则回退到 NacosEndpoint
	NacosNamespace      string // 命名空间 ID
	NacosGroup          string // 分组名
	NacosUsername       string // 认证用户名
	NacosPassword       string // 认证密码
	NacosAPIVersion     string // API 版本：v3（默认）或 v1
	HeartbeatInterval   time.Duration
	HeartbeatTTL        time.Duration
	GRPCWindowSize      int
	GRPCMaxMessageSize  int
	DispatchWorkers     int
	DispatchChSize      int
	StreamSendChSize    int
	Passthrough         bool
}

func defaultConfig() ServiceConfig {
	return ServiceConfig{
		ListenAddr:          "0.0.0.0",
		ListenPort:          "50052",
		AdvertiseAddr:       "",
		ServiceID:           "",
		ServiceName:         "logic",
		Zone:                "default",
		NacosEndpoint:       "http://127.0.0.1:8080",
		NacosNamingEndpoint: "http://127.0.0.1:8848",
		NacosNamespace:      "public",
		NacosGroup:          "DEFAULT_GROUP",
		NacosUsername:       "nacos",
		NacosPassword:       "nacos",
		NacosAPIVersion:     "v3",
		HeartbeatInterval:   3 * time.Second,
		HeartbeatTTL:        10 * time.Second,
		GRPCWindowSize:      524288,
		GRPCMaxMessageSize:  4 * 1024 * 1024,
		DispatchWorkers:     0,
		DispatchChSize:      0,
		StreamSendChSize:    0,
	}
}

type ServiceOption func(*ServiceConfig)

func WithListenPort(port string) ServiceOption {
	return func(c *ServiceConfig) { c.ListenPort = port }
}

func WithAdvertiseAddr(addr string) ServiceOption {
	return func(c *ServiceConfig) { c.AdvertiseAddr = addr }
}

func WithServiceID(id string) ServiceOption {
	return func(c *ServiceConfig) { c.ServiceID = id }
}

func WithServiceName(name string) ServiceOption {
	return func(c *ServiceConfig) { c.ServiceName = name }
}

func WithZone(zone string) ServiceOption {
	return func(c *ServiceConfig) {
		if zone != "" {
			c.Zone = zone
		}
	}
}

// WithNacosEndpoint 设置 Nacos 控制台地址（用于登录认证，默认 http://127.0.0.1:8080）
func WithNacosEndpoint(endpoint string) ServiceOption {
	return func(c *ServiceConfig) { c.NacosEndpoint = endpoint }
}

// WithNacosNamingEndpoint 设置 Nacos Server API 端口地址（用于实例注册/查询，默认 http://127.0.0.1:8848）
// Nacos 3.x 拆分了控制台端口与主端口；不设置则回退到 NacosEndpoint
func WithNacosNamingEndpoint(endpoint string) ServiceOption {
	return func(c *ServiceConfig) { c.NacosNamingEndpoint = endpoint }
}

// WithNacosNamespace 设置 Nacos 命名空间 ID
func WithNacosNamespace(ns string) ServiceOption {
	return func(c *ServiceConfig) { c.NacosNamespace = ns }
}

// WithNacosGroup 设置 Nacos 分组名
func WithNacosGroup(group string) ServiceOption {
	return func(c *ServiceConfig) { c.NacosGroup = group }
}

// WithNacosAuth 设置 Nacos 认证用户名和密码
func WithNacosAuth(username, password string) ServiceOption {
	return func(c *ServiceConfig) {
		c.NacosUsername = username
		c.NacosPassword = password
	}
}

// WithNacosAPIVersion 设置 Nacos API 版本（v3 或 v1）
func WithNacosAPIVersion(version string) ServiceOption {
	return func(c *ServiceConfig) { c.NacosAPIVersion = version }
}

func WithHeartbeat(interval, ttl time.Duration) ServiceOption {
	return func(c *ServiceConfig) {
		c.HeartbeatInterval = interval
		c.HeartbeatTTL = ttl
	}
}

func WithGRPCWindowSize(size int) ServiceOption {
	return func(c *ServiceConfig) { c.GRPCWindowSize = size }
}

func WithGRPCMaxMessageSize(size int) ServiceOption {
	return func(c *ServiceConfig) { c.GRPCMaxMessageSize = size }
}

func WithStreamSendChSize(n int) ServiceOption {
	return func(c *ServiceConfig) { c.StreamSendChSize = n }
}

// WithPassthrough enables raw receive-and-send handling for throughput tests.
func WithPassthrough(enabled bool) ServiceOption {
	return func(c *ServiceConfig) { c.Passthrough = enabled }
}

// WithDispatchWorkerCount 设置分发 worker 数（0=默认 NumCPU*128）。
// 注意与 server.go 中面向 Server 的 WithDispatchWorkers(ServerOption) 区分。
func WithDispatchWorkerCount(n int) ServiceOption {
	return func(c *ServiceConfig) { c.DispatchWorkers = n }
}
