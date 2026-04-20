package gateway

import (
	"sync"
	"sync/atomic"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

type HeartbeatConfig struct {
	Interval    time.Duration // 心跳间隔
	Timeout     time.Duration // 心跳超时时间
	MaxMisses   int          // 最大 missed 心跳次数
}

type connectionHeartbeat struct {
	connectionID   string
	lastHeartbeat  int64 // Unix timestamp in milliseconds
	missedCount    int32
	channel        chan struct{}
}

type HeartbeatManager struct {
	config             HeartbeatConfig
	connections        sync.Map
	stopChan           chan struct{}
	wg                 sync.WaitGroup
	mu                 sync.RWMutex
}

var heartbeatManager *HeartbeatManager
var heartbeatOnce sync.Once

func NewHeartbeatManager(config HeartbeatConfig) *HeartbeatManager {
	heartbeatOnce.Do(func() {
		heartbeatManager = &HeartbeatManager{
			config:    config,
			stopChan:  make(chan struct{}),
		}
	})
	return heartbeatManager
}

func GetHeartbeatManager() *HeartbeatManager {
	return heartbeatManager
}

func (hm *HeartbeatManager) Start() {
	if hm == nil {
		return
	}

	// 启动心跳检查协程
	hm.wg.Add(1)
	go hm.checkHeartbeats()

	// 启动连接清理协程
	hm.wg.Add(1)
	go hm.cleanupDeadConnections()

	tlog.Info("心跳管理器已启动",
		"interval", hm.config.Interval,
		"timeout", hm.config.Timeout,
		"maxMisses", hm.config.MaxMisses)
}

func (hm *HeartbeatManager) Stop() {
	if hm == nil {
		return
	}

	close(hm.stopChan)
	hm.wg.Wait()

	// 清理所有连接的心跳
	hm.connections.Range(func(key, value interface{}) bool {
		hb := value.(*connectionHeartbeat)
		close(hb.channel)
		hm.connections.Delete(key)
		return true
	})

	tlog.Info("心跳管理器已停止")
}

func (hm *HeartbeatManager) RegisterConnection(connectionID string) {
	if hm == nil {
		return
	}

	hb := &connectionHeartbeat{
		connectionID:  connectionID,
		lastHeartbeat: time.Now().UnixMilli(),
		missedCount:   0,
		channel:       make(chan struct{}),
	}

	hm.connections.Store(connectionID, hb)

	tlog.Debug("连接已注册心跳", "connectionID", connectionID)
}

func (hm *HeartbeatManager) UnregisterConnection(connectionID string) {
	if hm == nil {
		return
	}

	if hb, ok := hm.connections.Load(connectionID); ok {
		close(hb.(*connectionHeartbeat).channel)
		hm.connections.Delete(connectionID)
		tlog.Debug("连接已取消注册心跳", "connectionID", connectionID)
	}
}

func (hm *HeartbeatManager) RecordHeartbeat(connectionID string) {
	if hm == nil {
		return
	}

	if hb, ok := hm.connections.Load(connectionID); ok {
		heartbeat := hb.(*connectionHeartbeat)
		atomic.StoreInt64(&heartbeat.lastHeartbeat, time.Now().UnixMilli())
		atomic.StoreInt32(&heartbeat.missedCount, 0)
		tlog.Debug("心跳已记录", "connectionID", connectionID)
	}
}

func (hm *HeartbeatManager) GetConnectionStatus(connectionID string) (bool, int) {
	if hm == nil {
		return false, 0
	}

	if hb, ok := hm.connections.Load(connectionID); ok {
		heartbeat := hb.(*connectionHeartbeat)
		return true, int(atomic.LoadInt32(&heartbeat.missedCount))
	}
	return false, 0
}

func (hm *HeartbeatManager) checkHeartbeats() {
	defer hm.wg.Done()

	ticker := time.NewTicker(hm.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-hm.stopChan:
			return
		case <-ticker.C:
			hm.verifyHeartbeats()
		}
	}
}

func (hm *HeartbeatManager) verifyHeartbeats() {
	now := time.Now().UnixMilli()
	timeoutMs := hm.config.Timeout.Milliseconds()

	hm.connections.Range(func(key, value interface{}) bool {
		hb := value.(*connectionHeartbeat)
		lastHeartbeat := atomic.LoadInt64(&hb.lastHeartbeat)

		// 检查是否超时
		if now-lastHeartbeat > timeoutMs {
			// 增加 missed 计数
			newMissedCount := atomic.AddInt32(&hb.missedCount, 1)

			tlog.Warn("心跳超时",
				"connectionID", hb.connectionID,
				"lastHeartbeat", lastHeartbeat,
				"now", now,
				"missedCount", newMissedCount)

			// 如果超过最大 missed 次数，标记为死亡
			if newMissedCount >= int32(hm.config.MaxMisses) {
				tlog.Error("连接被判定为死亡（心跳超时）",
					"connectionID", hb.connectionID,
					"missedCount", newMissedCount)

				// 通知连接管理器关闭连接
				NotifyConnectionDead(hb.connectionID, "heartbeat_timeout")
			}
		}

		return true
	})
}

func (hm *HeartbeatManager) cleanupDeadConnections() {
	defer hm.wg.Done()

	// 每分钟清理一次已注册的死亡连接
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-hm.stopChan:
			return
		case <-ticker.C:
			hm.cleanup()
		}
	}
}

func (hm *HeartbeatManager) cleanup() {
	hm.connections.Range(func(key, value interface{}) bool {
		hb := value.(*connectionHeartbeat)

		select {
		case <-hb.channel:
			// Channel 已关闭，连接已移除
			hm.connections.Delete(key)
		default:
			// Channel 仍然开放，连接存活
		}

		return true
	})
}

func (hm *HeartbeatManager) GetStats() map[string]interface{} {
	if hm == nil {
		return nil
	}

	totalConnections := 0
	deadConnections := 0

	hm.connections.Range(func(key, value interface{}) bool {
		totalConnections++
		hb := value.(*connectionHeartbeat)
		if atomic.LoadInt32(&hb.missedCount) >= int32(hm.config.MaxMisses) {
			deadConnections++
		}
		return true
	})

	return map[string]interface{}{
		"totalConnections": totalConnections,
		"deadConnections":  deadConnections,
		"aliveConnections": totalConnections - deadConnections,
		"interval":         hm.config.Interval.String(),
		"timeout":         hm.config.Timeout.String(),
		"maxMisses":        hm.config.MaxMisses,
	}
}

// NotifyConnectionDead 通知连接死亡
var notifyConnectionDeadFunc func(connectionID, reason string)

func SetConnectionDeadNotifier(notifier func(connectionID, reason string)) {
	notifyConnectionDeadFunc = notifier
}

func NotifyConnectionDead(connectionID, reason string) {
	if notifyConnectionDeadFunc != nil {
		notifyConnectionDeadFunc(connectionID, reason)
	}
}

// DefaultHeartbeatConfig 默认心跳配置
func DefaultHeartbeatConfig() HeartbeatConfig {
	return HeartbeatConfig{
		Interval:  30 * time.Second, // 每 30 秒检查一次
		Timeout:   10 * time.Second,  // 10 秒内没有心跳则认为超时
		MaxMisses: 3,                 // 最多 missed 3 次
	}
}
