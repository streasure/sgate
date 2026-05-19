package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	tlog "github.com/streasure/treasure-slog"
)

type ServiceRegistry struct {
	rdb               *redis.Client
	serviceInfo       *ServiceInfo
	heartbeatTTL      time.Duration
	heartbeatInterval time.Duration
	stopCh            chan struct{}
	wg                sync.WaitGroup
}

func NewServiceRegistry(rdb *redis.Client, serviceInfo *ServiceInfo, heartbeatInterval, heartbeatTTL time.Duration) *ServiceRegistry {
	if heartbeatInterval <= 0 {
		heartbeatInterval = DefaultHeartbeat
	}
	if heartbeatTTL <= 0 {
		heartbeatTTL = DefaultKeyTTL
	}
	return &ServiceRegistry{
		rdb:               rdb,
		serviceInfo:       serviceInfo,
		heartbeatInterval: heartbeatInterval,
		heartbeatTTL:      heartbeatTTL,
		stopCh:            make(chan struct{}),
	}
}

func (sr *ServiceRegistry) Start() error {
	ctx := context.Background()
	key := sr.serviceKey()

	data, err := json.Marshal(sr.serviceInfo)
	if err != nil {
		return fmt.Errorf("marshal service info: %w", err)
	}

	if err := sr.rdb.Set(ctx, key, data, sr.heartbeatTTL).Err(); err != nil {
		return fmt.Errorf("register service in redis: %w", err)
	}

	sr.publishEvent(ctx, ServiceEvent{
		Type:      EventRegister,
		Service:   *sr.serviceInfo,
		Timestamp: time.Now().UnixMilli(),
	})

	sr.wg.Add(1)
	go sr.heartbeatLoop()

	tlog.Info("service registry started",
		"serviceID", sr.serviceInfo.ServiceID,
		"address", sr.serviceInfo.Address,
		"heartbeatInterval", sr.heartbeatInterval,
		"heartbeatTTL", sr.heartbeatTTL,
	)
	return nil
}

func (sr *ServiceRegistry) Stop() {
	close(sr.stopCh)
	sr.wg.Wait()

	ctx := context.Background()
	key := sr.serviceKey()
	sr.rdb.Del(ctx, key)

	sr.publishEvent(ctx, ServiceEvent{
		Type:      EventDeregister,
		Service:   *sr.serviceInfo,
		Timestamp: time.Now().UnixMilli(),
	})

	tlog.Info("service deregistered",
		"serviceID", sr.serviceInfo.ServiceID,
		"address", sr.serviceInfo.Address,
	)
}

func (sr *ServiceRegistry) serviceKey() string {
	return fmt.Sprintf("%s%s:%s", ServiceKeyPrefix, sr.serviceInfo.ServiceName, sr.serviceInfo.ServiceID)
}

func (sr *ServiceRegistry) heartbeatLoop() {
	defer sr.wg.Done()
	ticker := time.NewTicker(sr.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sr.stopCh:
			return
		case <-ticker.C:
			sr.doHeartbeat()
		}
	}
}

func (sr *ServiceRegistry) doHeartbeat() {
	ctx := context.Background()
	key := sr.serviceKey()

	data, err := json.Marshal(sr.serviceInfo)
	if err != nil {
		tlog.Error("heartbeat marshal failed", "error", err)
		return
	}

	if err := sr.rdb.Set(ctx, key, data, sr.heartbeatTTL).Err(); err != nil {
		tlog.Error("heartbeat redis set failed", "error", err, "serviceID", sr.serviceInfo.ServiceID)
		return
	}

	sr.publishEvent(ctx, ServiceEvent{
		Type:      EventHeartbeat,
		Service:   *sr.serviceInfo,
		Timestamp: time.Now().UnixMilli(),
	})
}

func (sr *ServiceRegistry) publishEvent(ctx context.Context, event ServiceEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		tlog.Error("publish event marshal failed", "error", err)
		return
	}
	if err := sr.rdb.Publish(ctx, ServiceChannel, data).Err(); err != nil {
		tlog.Error("publish event failed", "error", err, "type", event.Type)
	}
}
