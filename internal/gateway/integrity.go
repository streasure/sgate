package gateway

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/uhash"
)

// integrityShardCount 重放缓存分片数（2 的幂）。单 map 全局写锁会让
// verifyInbound=true 时所有连接的每条消息串行化；按 connectionID 哈希
// 分片后热点摊到 N 把锁上（同连接恒同片，顺序语义不变）。
const integrityShardCount = 16

// errIntegrityReplay 预分配错误，避免热路径每次 fmt.Errorf 分配。
var errIntegrityReplay = fmt.Errorf("replay attack detected")

type integrityShard struct {
	mu    sync.Mutex
	cache map[string]int64
}

type MessageIntegrity struct {
	timeWindow int64
	shards     [integrityShardCount]integrityShard
	stopOnce   sync.Once
	stopCh     chan struct{}
}

func NewMessageIntegrity(timeWindow int64) *MessageIntegrity {
	mi := &MessageIntegrity{
		timeWindow: timeWindow,
		stopCh:     make(chan struct{}),
	}
	for i := range mi.shards {
		mi.shards[i].cache = make(map[string]int64)
	}
	panicutil.GoLoop(context.Background(), "gateway.messageIntegrity.cleanup", mi.cleanupCache)
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
			now := time.Now().UnixMilli()
			for i := range mi.shards {
				sh := &mi.shards[i]
				sh.mu.Lock()
				maps.DeleteFunc(sh.cache, func(_ string, ts int64) bool {
					return now-ts > mi.timeWindow
				})
				sh.mu.Unlock()
			}
		}
	}
}

func (mi *MessageIntegrity) Stop() {
	mi.stopOnce.Do(func() { close(mi.stopCh) })
}

// shardFor 按 connectionID 定位分片（FNV-1a，零分配）。
func (mi *MessageIntegrity) shardFor(connectionID string) *integrityShard {
	return &mi.shards[uhash.FNV1a32(connectionID)%integrityShardCount]
}

// ProcessMessage 按连接维度做重放检测。
// key 必须包含 connectionID：消息帧内 SessionId/UserKey 解码后恒为空，
// 用原始字段会让所有连接共享同一 key（互丢消息）。
// 要求 SeqId 非 0：重放检测依赖每消息唯一序号；SeqId==0（未提供序号）
// 会退化成「首条放行、同 cmd 后续全被误判重放」，故直接拒绝。
func (mi *MessageIntegrity) ProcessMessage(connectionID string, msg *protoGw.StreamData) error {
	if msg.SeqId == 0 {
		return fmt.Errorf("seq_id required when verifyInbound enabled cmd=%d", msg.Cmd)
	}
	// 拼接 key 替代 fmt.Sprintf（少一次反射式格式化与额外分配）
	var b strings.Builder
	b.Grow(len(connectionID) + len(msg.UserKey) + 32)
	b.WriteString(connectionID)
	b.WriteByte(0)
	b.WriteString(msg.UserKey)
	b.WriteByte(0)
	b.WriteString(strconv.FormatInt(int64(msg.Cmd), 10))
	b.WriteByte(0)
	b.WriteString(strconv.FormatInt(msg.SeqId, 10))
	key := b.String()

	sh := mi.shardFor(connectionID)
	sh.mu.Lock()
	if _, exists := sh.cache[key]; exists {
		sh.mu.Unlock()
		return errIntegrityReplay
	}
	sh.cache[key] = time.Now().UnixMilli()
	sh.mu.Unlock()
	return nil
}
