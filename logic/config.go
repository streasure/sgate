package logic

import (
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ServiceConfig contains the logic server identity, transport and etcd settings.
type ServiceConfig struct {
	ListenAddr         string        `yaml:"listenAddr"`
	ListenPort         string        `yaml:"listenPort"`
	AdvertiseAddr      string        `yaml:"advertiseAddr"`
	ServiceID          string        `yaml:"serverId"`
	ServerType         string        `yaml:"serverType"`
	ServiceName        string        `yaml:"serviceName"`
	Zone               string        `yaml:"zone"`
	EtcdEndpoints      []string      `yaml:"etcdEndpoints"`
	EtcdEndpoint       string        `yaml:"etcdEndpoint"`
	EtcdUsername       string        `yaml:"etcdUsername"`
	EtcdPassword       string        `yaml:"etcdPassword"`
	EtcdServicePrefix  string        `yaml:"etcdServicePrefix"`
	EtcdLeaseTTL       string        `yaml:"etcdLeaseTTL"`
	HeartbeatInterval  time.Duration `yaml:"heartbeatInterval"`
	HeartbeatTTL       time.Duration `yaml:"heartbeatTTL"`
	GRPCWindowSize     int           `yaml:"grpcWindowSize"`
	GRPCMaxMessageSize int           `yaml:"grpcMaxMessageSize"`
	DispatchWorkers    int           `yaml:"dispatchWorkers"`
	DispatchChSize     int           `yaml:"dispatchChSize"`
	StreamSendChSize   int           `yaml:"streamSendChSize"`
	Passthrough        bool          `yaml:"passthrough"`
}

func LoadConfig(name string) (ServiceConfig, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func defaultConfig() ServiceConfig {
	return ServiceConfig{
		ListenAddr: "0.0.0.0", ListenPort: "50052", ServiceID: "", ServerType: "Logic",
		ServiceName: "logic", Zone: "default", EtcdEndpoint: "http://127.0.0.1:2379",
		EtcdServicePrefix: "/services", EtcdLeaseTTL: "10s", HeartbeatInterval: 3 * time.Second,
		HeartbeatTTL: 10 * time.Second, GRPCWindowSize: 524288, GRPCMaxMessageSize: 4 * 1024 * 1024,
	}
}

type ServiceOption func(*ServiceConfig)

func WithConfig(cfg ServiceConfig) ServiceOption { return func(c *ServiceConfig) { *c = cfg } }
func WithListenPort(port string) ServiceOption   { return func(c *ServiceConfig) { c.ListenPort = port } }
func WithAdvertiseAddr(addr string) ServiceOption {
	return func(c *ServiceConfig) { c.AdvertiseAddr = addr }
}
func WithServiceID(id string) ServiceOption { return func(c *ServiceConfig) { c.ServiceID = id } }
func WithServerType(t string) ServiceOption { return func(c *ServiceConfig) { c.ServerType = t } }
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
func WithEtcd(endpoint string) ServiceOption {
	return func(c *ServiceConfig) { c.EtcdEndpoint = endpoint }
}
func WithHeartbeat(interval, ttl time.Duration) ServiceOption {
	return func(c *ServiceConfig) { c.HeartbeatInterval, c.HeartbeatTTL = interval, ttl }
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
func WithPassthrough(enabled bool) ServiceOption {
	return func(c *ServiceConfig) { c.Passthrough = enabled }
}
func WithDispatchWorkerCount(n int) ServiceOption {
	return func(c *ServiceConfig) { c.DispatchWorkers = n }
}
