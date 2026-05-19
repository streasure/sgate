package logic

import "time"

type ServiceConfig struct {
	ListenAddr        string
	ListenPort        string
	AdvertiseAddr     string
	ServiceID         string
	ServiceName       string
	RedisAddr         string
	RedisPassword     string
	RedisDB           int
	HeartbeatInterval time.Duration
	HeartbeatTTL      time.Duration
}

func defaultConfig() ServiceConfig {
	return ServiceConfig{
		ListenAddr:        "0.0.0.0",
		ListenPort:        "50052",
		AdvertiseAddr:     "",
		ServiceID:         "",
		ServiceName:       "logic",
		RedisAddr:         "127.0.0.1:6379",
		RedisPassword:     "",
		RedisDB:           10,
		HeartbeatInterval: 3 * time.Second,
		HeartbeatTTL:      10 * time.Second,
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
