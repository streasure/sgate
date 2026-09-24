package gateway

import (
	"fmt"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
)

type MessageIntegrity struct {
	timeWindow  int64
	replayCache map[string]int64
	cacheMutex  sync.RWMutex
	stopOnce    sync.Once
	stopCh      chan struct{}
}

func NewMessageIntegrity(timeWindow int64) *MessageIntegrity {
	mi := &MessageIntegrity{
		timeWindow:  timeWindow,
		replayCache: make(map[string]int64),
		stopCh:      make(chan struct{}),
	}
	go mi.cleanupCache()
	return mi
}

func (mi *MessageIntegrity) cleanupCache() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-mi.stopCh:
			return
		case <-ticker.C:
			mi.cacheMutex.Lock()
			now := time.Now().UnixMilli()
			for key, ts := range mi.replayCache {
				if now-ts > mi.timeWindow {
					delete(mi.replayCache, key)
				}
			}
			mi.cacheMutex.Unlock()
		}
	}
}

func (mi *MessageIntegrity) Stop() {
	mi.stopOnce.Do(func() { close(mi.stopCh) })
}

func (mi *MessageIntegrity) ProcessMessage(msg *protoGw.StreamData) error {
	msgID := fmt.Sprintf("%s-%s-%d-%d", msg.SessionId, msg.UserKey, msg.Cmd, msg.SeqId)
	mi.cacheMutex.Lock()
	if _, exists := mi.replayCache[msgID]; exists {
		mi.cacheMutex.Unlock()
		return fmt.Errorf("replay attack detected")
	}
	mi.replayCache[msgID] = time.Now().UnixMilli()
	mi.cacheMutex.Unlock()
	return nil
}
