package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/streasure/sgate/discovery"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

type ServiceChangeCallback func(event discovery.ServiceEvent)

type ServiceDiscovery struct {
	rdb         *redis.Client
	cfg         config.DiscoveryConfig
	services    map[string]*discovery.ServiceInfo
	mu          sync.RWMutex
	callbacks   []ServiceChangeCallback
	stopCh      chan struct{}
	wg          sync.WaitGroup
	sub         *redis.PubSub
	keyEventSub *redis.PubSub
}

func NewServiceDiscovery(rdb *redis.Client, cfg config.DiscoveryConfig) *ServiceDiscovery {
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = discovery.DefaultScan
	}
	if cfg.DeregisterDelay <= 0 {
		cfg.DeregisterDelay = discovery.DefaultDeregister
	}
	return &ServiceDiscovery{
		rdb:       rdb,
		cfg:       cfg,
		services:  make(map[string]*discovery.ServiceInfo),
		callbacks: make([]ServiceChangeCallback, 0),
		stopCh:    make(chan struct{}),
	}
}

func (sd *ServiceDiscovery) Start() error {
	ctx := context.Background()

	sd.rdb.ConfigSet(ctx, "notify-keyspace-events", "Ex")

	if err := sd.scanServices(ctx); err != nil {
		tlog.Warn("initial service scan failed", "error", err)
	}

	sd.sub = sd.rdb.Subscribe(ctx, discovery.ServiceChannel)

	keyEventChannel := fmt.Sprintf("__keyevent@%d__:expired", sd.rdb.Options().DB)
	sd.keyEventSub = sd.rdb.Subscribe(ctx, keyEventChannel)

	sd.wg.Add(3)
	go sd.subscribeLoop()
	go sd.keyEventLoop()
	go sd.scanLoop()

	tlog.Info("service discovery started",
		"serviceName", sd.cfg.ServiceName,
		"scanInterval", sd.cfg.ScanInterval,
		"deregisterDelay", sd.cfg.DeregisterDelay,
		"keyEventChannel", keyEventChannel,
	)
	return nil
}

func (sd *ServiceDiscovery) Stop() {
	close(sd.stopCh)
	if sd.sub != nil {
		sd.sub.Close()
	}
	if sd.keyEventSub != nil {
		sd.keyEventSub.Close()
	}
	sd.wg.Wait()
	tlog.Info("service discovery stopped")
}

func (sd *ServiceDiscovery) OnServiceChange(callback ServiceChangeCallback) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.callbacks = append(sd.callbacks, callback)
}

func (sd *ServiceDiscovery) GetServices() []*discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()

	result := make([]*discovery.ServiceInfo, 0, len(sd.services))
	for _, svc := range sd.services {
		result = append(result, svc)
	}
	return result
}

func (sd *ServiceDiscovery) GetService(serviceID string) *discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	return sd.services[serviceID]
}

func (sd *ServiceDiscovery) GetServiceByAddress(address string) *discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, svc := range sd.services {
		if svc.Address == address {
			return svc
		}
	}
	return nil
}

// shouldAcceptZone 判断服务是否属于本网关 zone。
// C3 修复: 无 zone 元数据的服务视为 "default"；网关未配置 zone 时放行所有。
// 网关配置了 zone 时，仅接受 zone 相同的服务（含 "default" 匹配）。
func (sd *ServiceDiscovery) shouldAcceptZone(meta map[string]string) bool {
	if sd.cfg.Zone == "" {
		return true
	}
	svcZone := meta["zone"]
	if svcZone == "" {
		svcZone = "default"
	}
	return svcZone == sd.cfg.Zone
}

func (sd *ServiceDiscovery) subscribeLoop() {
	defer sd.wg.Done()

	for {
		ch := sd.sub.Channel()

		for {
			select {
			case <-sd.stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					tlog.Warn("subscribe channel closed, attempting reconnect...")
					if sd.reconnectSubscription() {
						ch = sd.sub.Channel()
						continue
					}
					return
				}
				sd.handleMessage(msg)
			}
		}
	}
}

func (sd *ServiceDiscovery) keyEventLoop() {
	defer sd.wg.Done()
	prefix := fmt.Sprintf("%s%s:", discovery.ServiceKeyPrefix, sd.cfg.ServiceName)

	for {
		ch := sd.keyEventSub.Channel()

		for {
			select {
			case <-sd.stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					tlog.Warn("keyEvent channel closed, attempting reconnect...")
					if sd.reconnectKeyEvent() {
						ch = sd.keyEventSub.Channel()
						continue
					}
					return
				}
				if msg.Channel != "" {
					key := msg.Payload
					if len(key) > len(prefix) && key[:len(prefix)] == prefix {
						sd.handleKeyExpired(key)
					}
				}
			}
		}
	}
}

func (sd *ServiceDiscovery) reconnectSubscription() bool {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		select {
		case <-sd.stopCh:
			return false
		default:
		}
		time.Sleep(time.Duration(i+1) * 2 * time.Second)
		sd.sub = sd.rdb.Subscribe(ctx, discovery.ServiceChannel)
		if err := sd.sub.Ping(ctx); err != nil {
			tlog.Warn("subscription reconnect failed", "attempt", i+1, "error", err)
			continue
		}
		tlog.Info("subscription reconnected")
		return true
	}
	tlog.Error("subscription reconnect exhausted")
	return false
}

func (sd *ServiceDiscovery) reconnectKeyEvent() bool {
	ctx := context.Background()
	keyEventChannel := fmt.Sprintf("__keyevent@%d__:expired", sd.rdb.Options().DB)
	for i := 0; i < 5; i++ {
		select {
		case <-sd.stopCh:
			return false
		default:
		}
		time.Sleep(time.Duration(i+1) * 2 * time.Second)
		sd.keyEventSub = sd.rdb.Subscribe(ctx, keyEventChannel)
		if err := sd.keyEventSub.Ping(ctx); err != nil {
			tlog.Warn("keyEvent reconnect failed", "attempt", i+1, "error", err)
			continue
		}
		tlog.Info("keyEvent reconnected")
		return true
	}
	tlog.Error("keyEvent reconnect exhausted")
	return false
}

func (sd *ServiceDiscovery) handleKeyExpired(key string) {
	prefix := fmt.Sprintf("%s%s:", discovery.ServiceKeyPrefix, sd.cfg.ServiceName)
	serviceID := key[len(prefix):]

	sd.mu.Lock()
	svc, exists := sd.services[serviceID]
	if exists {
		delete(sd.services, serviceID)
	}
	sd.mu.Unlock()

	if exists {
		tlog.Warn("service key expired, immediate offline detection",
			"serviceID", svc.ServiceID,
			"address", svc.Address,
			"key", key,
		)
		sd.notifyCallbacks(discovery.ServiceEvent{
			Type:      discovery.EventDeregister,
			Service:   *svc,
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

func (sd *ServiceDiscovery) handleMessage(msg *redis.Message) {
	var event discovery.ServiceEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		tlog.Error("discovery event unmarshal failed", "error", err)
		return
	}

	switch event.Type {
	case discovery.EventRegister:
		sd.handleRegister(event)
	case discovery.EventDeregister:
		sd.handleDeregister(event)
	case discovery.EventHeartbeat:
		sd.handleHeartbeat(event)
	}
}

func (sd *ServiceDiscovery) handleRegister(event discovery.ServiceEvent) {
	if !sd.shouldAcceptZone(event.Service.Metadata) {
		tlog.Debug("skipping service from different zone",
			"serviceID", event.Service.ServiceID,
			"serviceZone", event.Service.Metadata["zone"],
			"localZone", sd.cfg.Zone,
		)
		return
	}

	sd.mu.Lock()
	_, existed := sd.services[event.Service.ServiceID]
	sd.services[event.Service.ServiceID] = &event.Service
	sd.mu.Unlock()

	if !existed {
		tlog.Info("service registered",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
			"serviceName", event.Service.ServiceName,
		)
		sd.notifyCallbacks(event)
	}
}

func (sd *ServiceDiscovery) handleDeregister(event discovery.ServiceEvent) {
	sd.mu.Lock()
	_, existed := sd.services[event.Service.ServiceID]
	if existed {
		delete(sd.services, event.Service.ServiceID)
	}
	sd.mu.Unlock()

	if existed {
		tlog.Info("service deregistered",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
		)
		sd.notifyCallbacks(event)
	}
}

func (sd *ServiceDiscovery) handleHeartbeat(event discovery.ServiceEvent) {
	if !sd.shouldAcceptZone(event.Service.Metadata) {
		return
	}

	sd.mu.Lock()
	_, existed := sd.services[event.Service.ServiceID]
	sd.services[event.Service.ServiceID] = &event.Service
	sd.mu.Unlock()

	if !existed {
		tlog.Info("service discovered via heartbeat",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
		)
		sd.notifyCallbacks(discovery.ServiceEvent{
			Type:      discovery.EventRegister,
			Service:   event.Service,
			Timestamp: event.Timestamp,
		})
	}
}

func (sd *ServiceDiscovery) scanLoop() {
	defer sd.wg.Done()
	ticker := time.NewTicker(sd.cfg.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sd.stopCh:
			return
		case <-ticker.C:
			ctx := context.Background()
			if err := sd.scanServices(ctx); err != nil {
				tlog.Error("service scan failed", "error", err)
			}
		}
	}
}

func (sd *ServiceDiscovery) scanServices(ctx context.Context) error {
	pattern := fmt.Sprintf("%s%s:*", discovery.ServiceKeyPrefix, sd.cfg.ServiceName)
	keys, err := sd.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return fmt.Errorf("scan service keys: %w", err)
	}

	tlog.Info("scanServices", "pattern", pattern, "keysFound", len(keys))

	activeServices := make(map[string]*discovery.ServiceInfo)
	for _, key := range keys {
		data, err := sd.rdb.Get(ctx, key).Result()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			tlog.Warn("get service key failed", "key", key, "error", err)
			continue
		}

		var svc discovery.ServiceInfo
		if err := json.Unmarshal([]byte(data), &svc); err != nil {
			tlog.Warn("unmarshal service info failed", "key", key, "error", err)
			continue
		}

		if !sd.shouldAcceptZone(svc.Metadata) {
			continue
		}

		activeServices[svc.ServiceID] = &svc
	}

	sd.mu.Lock()
	oldServices := make(map[string]*discovery.ServiceInfo)
	for k, v := range sd.services {
		oldServices[k] = v
	}

	for id, svc := range activeServices {
		sd.services[id] = svc
	}

	var deregistered []*discovery.ServiceInfo
	for id, svc := range oldServices {
		if _, ok := activeServices[id]; !ok {
			delete(sd.services, id)
			deregistered = append(deregistered, svc)
		}
	}
	sd.mu.Unlock()

	for _, svc := range deregistered {
		tlog.Warn("service expired (heartbeat TTL)",
			"serviceID", svc.ServiceID,
			"address", svc.Address,
		)
		sd.notifyCallbacks(discovery.ServiceEvent{
			Type:      discovery.EventDeregister,
			Service:   *svc,
			Timestamp: time.Now().UnixMilli(),
		})
	}

	for id := range activeServices {
		if _, ok := oldServices[id]; !ok {
			tlog.Info("service discovered by scan",
				"serviceID", activeServices[id].ServiceID,
				"address", activeServices[id].Address,
			)
			sd.notifyCallbacks(discovery.ServiceEvent{
				Type:      discovery.EventRegister,
				Service:   *activeServices[id],
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}

	return nil
}

func (sd *ServiceDiscovery) notifyCallbacks(event discovery.ServiceEvent) {
	sd.mu.RLock()
	callbacks := make([]ServiceChangeCallback, len(sd.callbacks))
	copy(callbacks, sd.callbacks)
	sd.mu.RUnlock()

	for _, cb := range callbacks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					tlog.Error("notifyCallback panic recovered", "error", r)
				}
			}()
			cb(event)
		}()
	}
}
