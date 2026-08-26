package discovery

import "time"

const (
	// DefaultKeyTTL Nacos 临时实例心跳 TTL（实例失联后在此时间内被摘除）
	DefaultKeyTTL = 15 * time.Second
	// DefaultHeartbeat 向 Nacos 重新注册实例的心跳间隔
	DefaultHeartbeat = 3 * time.Second
	// DefaultDeregister 服务下线后等待回调的宽限期
	DefaultDeregister = 5 * time.Second
	// DefaultScan 从 Nacos 拉取实例列表的轮询间隔
	DefaultScan = 10 * time.Second
)

type ServiceEventType string

const (
	EventRegister   ServiceEventType = "register"
	EventDeregister ServiceEventType = "deregister"
	EventHeartbeat  ServiceEventType = "heartbeat"
)

type ServiceInfo struct {
	ServiceID   string            `json:"service_id"`
	ServiceName string            `json:"service_name"`
	Address     string            `json:"address"`
	Weight      int               `json:"weight"`
	Metadata    map[string]string `json:"metadata"`
	StartTime   int64             `json:"start_time"`
}

type ServiceEvent struct {
	Type      ServiceEventType `json:"type"`
	Service   ServiceInfo      `json:"service"`
	Timestamp int64            `json:"timestamp"`
}
