package discovery

import "time"

const (
	ServiceKeyPrefix  = "sgate:service:"
	ServiceChannel    = "sgate:service:events"
	DefaultKeyTTL     = 15 * time.Second
	DefaultHeartbeat  = 3 * time.Second
	DefaultDeregister = 5 * time.Second
	DefaultScan       = 10 * time.Second
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
