package backend

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/streasure/sgate/internal/connection"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
)

type LogicClientPool struct {
	clients    map[string]*LogicClient     // 逻辑服客户端映射（serverID -> client）
	ordered    []string                    // 有序的服务 ID 列表，用于确定性轮询
	mu         sync.RWMutex                // 读写锁
	gateway    GatewayInterface            // 网关接口引用
	balancer   *cluster.Balancer           // 负载均衡器
	stopCh     chan struct{}               // 停止信号
	wg         sync.WaitGroup              // 等待协程退出
	rrIndex    atomic.Uint64               // 轮询索引（原子操作）
	fastClient atomic.Pointer[LogicClient] // 快速路径：单客户端时的原子指针
	zoneMap    map[string]string           // zone 映射（serverID -> zone，来自 logic 拨入握手）
	closed     bool                        // 池已关闭（Close 后拒绝新接入）
}

// RegisterClient 注册逻辑服客户端到池中
func (pool *LogicClientPool) RegisterClient(serverID string, client *LogicClient) {
	if serverID == "" || client == nil {
		return
	}
	client.SetServerID(serverID)
	pool.mu.Lock()
	pool.clients[serverID] = client
	if !slices.Contains(pool.ordered, serverID) {
		pool.ordered = append(pool.ordered, serverID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()
}

// GetClient 根据 serverID 获取已连接的逻辑服客户端
func (pool *LogicClientPool) GetClient(serverID string) connection.LogicClientProvider {
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	return client
}

// NewLogicClientPool 创建逻辑服客户端池
func NewLogicClientPool(gateway GatewayInterface) *LogicClientPool {
	return &LogicClientPool{
		clients: make(map[string]*LogicClient),
		zoneMap: make(map[string]string),
		gateway: gateway,
		stopCh:  make(chan struct{}),
	}
}

// updateFastClient 更新快速路径指针，必须在持有 pool.mu 时调用。
// 当且仅当有 1 个客户端连接时设置 fastClient，否则置 nil。
func (pool *LogicClientPool) updateFastClient() {
	if len(pool.clients) == 1 {
		for _, c := range pool.clients {
			pool.fastClient.Store(c)
			return
		}
	}
	pool.fastClient.Store(nil)
}

// ZoneOf 返回指定 serverID 所属的 zone（来自 logic 拨入握手元数据）；未知返回空串。
func (pool *LogicClientPool) ZoneOf(serverID string) string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.zoneMap[serverID]
}

// IsClientConnected 检查指定 serverID 的客户端是否已连接。
func (pool *LogicClientPool) IsClientConnected(serverID string) bool {
	pool.mu.RLock()
	client, ok := pool.clients[serverID]
	pool.mu.RUnlock()
	return ok && client != nil && client.IsConnected()
}

// Attach 接受 logic 主动拨入的一个流分片（flip 模式）。
// 首次接入时创建 LogicClient 与分片管理器；logic 重启导致分片总数变化时重建客户端。
// 返回附着后的客户端与分片，调用方随后驱动接收循环，并在流结束时 defer 分片解绑。
func (pool *LogicClientPool) Attach(meta routes.LogicStreamMeta, stream dataStream) (*LogicClient, *StreamShard, error) {
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return nil, nil, ErrConnectionClosing
	}
	client := pool.clients[meta.LogicID]
	var stale *LogicClient
	if client == nil || client.totalShards != meta.ShardCount {
		if client != nil {
			// 分片总数变化（logic 以不同配置重启）：旧客户端整体废弃。
			stale = client
			delete(pool.clients, meta.LogicID)
		}
		client = NewLogicClient(pool.gateway)
		client.SetServerID(meta.LogicID)
		client.address = meta.Addr
		client.initAccepted(meta.ShardCount)
		pool.clients[meta.LogicID] = client
		if meta.Zone != "" {
			pool.zoneMap[meta.LogicID] = meta.Zone
		}
		if !slices.Contains(pool.ordered, meta.LogicID) {
			pool.ordered = append(pool.ordered, meta.LogicID)
		}
		pool.updateFastClient()
		tlog.Info(context.TODO(), "logic client attached to pool serviceID=%s zone=%s address=%s shards=%d totalClients=%d",
			meta.LogicID, meta.Zone, meta.Addr, meta.ShardCount, len(pool.clients))
	}
	pool.mu.Unlock()

	if stale != nil {
		go stale.Close()
	}

	if err := client.attachShard(meta.ShardIdx, stream); err != nil {
		return nil, nil, err
	}
	sm := client.streamManager.Load()
	if sm == nil || meta.ShardIdx >= len(sm.shards) {
		return nil, nil, ErrNotConnected
	}
	return client, sm.shards[meta.ShardIdx], nil
}

// SetBalancer 设置负载均衡器
func (pool *LogicClientPool) SetBalancer(balancer *cluster.Balancer) {
	pool.balancer = balancer
}

// SendMessage 发送消息到逻辑服，优先使用快速路径
func (pool *LogicClientPool) SendMessage(msg *protoGw.StreamData) error {
	// 快速路径：只有一个客户端，无需加锁。
	// 快速路径：单客户端时无需加锁
	if c := pool.fastClient.Load(); c != nil {
		return c.SendMessage(msg)
	}
	return pool.RoundRobinSendMessage(msg)
}

// SendMessageTo 向指定逻辑服发送会话绑定消息
// 不会回退到轮询，因为那样可能跨服务器分片
func (pool *LogicClientPool) SendMessageTo(serverID string, msg *protoGw.StreamData) error {
	if serverID == "" {
		return ErrNotConnected
	}
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return ErrNotConnected
	}
	return client.SendMessage(msg)
}

// RoundRobinSendMessage 使用轮询方式发送消息到逻辑服
func (pool *LogicClientPool) RoundRobinSendMessage(msg *protoGw.StreamData) error {
	pool.mu.RLock()
	n := len(pool.ordered)
	if n == 0 {
		pool.mu.RUnlock()
		return ErrNotConnected
	}

	idx := pool.rrIndex.Add(1) % uint64(n)
	serviceID := pool.ordered[idx]
	client := pool.clients[serviceID]
	pool.mu.RUnlock()

	if client == nil || !client.IsConnected() {
		return ErrNotConnected
	}
	return client.SendMessage(msg)
}

// Close 关闭客户端池中所有连接；关闭后拒绝新的 logic 接入。
func (pool *LogicClientPool) Close() {
	close(pool.stopCh)
	pool.wg.Wait()

	pool.mu.Lock()
	defer pool.mu.Unlock()

	pool.closed = true
	for id, client := range pool.clients {
		client.Close()
		delete(pool.clients, id)
	}
	pool.ordered = pool.ordered[:0]
	clear(pool.zoneMap)
}

// ClientCount 获取客户端池中的客户端数量
func (pool *LogicClientPool) ClientCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return len(pool.clients)
}

// IsConnected 检查池中是否有已连接的客户端
func (pool *LogicClientPool) IsConnected() bool {
	// 快速路径：只有一个客户端，无需加锁。
	// 快速路径：单客户端时无需加锁
	if c := pool.fastClient.Load(); c != nil {
		return c.IsConnected()
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	for _, client := range pool.clients {
		if client.IsConnected() {
			return true
		}
	}
	return false
}
