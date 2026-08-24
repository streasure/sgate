package logic

import "time"

type ServiceConfig struct {
	ListenAddr         string
	ListenPort         string
	AdvertiseAddr      string
	ServiceID          string
	ServiceName        string
	Zone               string
	RedisAddr          string
	RedisPassword      string
	RedisDB            int
	HeartbeatInterval  time.Duration
	HeartbeatTTL       time.Duration
	GRPCWindowSize     int
	GRPCMaxMessageSize int
	DispatchWorkers    int
	DispatchChSize     int
	StreamSendChSize   int
}

func defaultConfig() ServiceConfig {
	return ServiceConfig{
		ListenAddr:         "0.0.0.0",
		ListenPort:         "50052",
		AdvertiseAddr:      "",
		ServiceID:          "",
		ServiceName:        "logic",
		Zone:               "default",
		RedisAddr:          "127.0.0.1:6379",
		RedisPassword:      "",
		RedisDB:            10,
		HeartbeatInterval:  3 * time.Second,
		HeartbeatTTL:       10 * time.Second,
		GRPCWindowSize:     524288,
		GRPCMaxMessageSize: 4 * 1024 * 1024,
		DispatchWorkers:    0,
		DispatchChSize:     0,
		StreamSendChSize:   0,
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

func WithRedisAddr(addr string) ServiceOption {
	return func(c *ServiceConfig) { c.RedisAddr = addr }
}

func WithRedisPassword(password string) ServiceOption {
	return func(c *ServiceConfig) { c.RedisPassword = password }
}

func WithRedisDB(db int) ServiceOption {
	return func(c *ServiceConfig) { c.RedisDB = db }
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
