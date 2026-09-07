package gateway

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

type MessageIntegrity struct {
	timeWindow  int64
	replayCache map[string]int64
	cacheMutex  sync.RWMutex
}

func NewMessageIntegrity(timeWindow int64) *MessageIntegrity {
	mi := &MessageIntegrity{
		timeWindow:  timeWindow,
		replayCache: make(map[string]int64),
	}
	go mi.cleanupCache()
	return mi
}

func (mi *MessageIntegrity) cleanupCache() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
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

func (mi *MessageIntegrity) GenerateChecksum(msg proto.Message) string {
	data, err := proto.Marshal(msg)
	if err != nil {
		return ""
	}
	hash := md5.Sum(data)
	return hex.EncodeToString(hash[:])
}

func (mi *MessageIntegrity) CheckReplay(msgID string) bool {
	mi.cacheMutex.RLock()
	defer mi.cacheMutex.RUnlock()
	_, exists := mi.replayCache[msgID]
	return exists
}

func (mi *MessageIntegrity) MarkProcessed(msgID string) {
	mi.cacheMutex.Lock()
	defer mi.cacheMutex.Unlock()
	mi.replayCache[msgID] = time.Now().UnixMilli()
}

func (mi *MessageIntegrity) ProcessMessage(msg *protoGw.StreamData) error {
	msgID := fmt.Sprintf("%s-%s-%d-%d", msg.SessionId, msg.UserKey, msg.Cmd, msg.SeqId)
	if mi.CheckReplay(msgID) {
		return fmt.Errorf("replay attack detected")
	}
	mi.MarkProcessed(msgID)
	return nil
}
