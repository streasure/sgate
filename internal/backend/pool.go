package backend

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/connection"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
)

type LogicClientPool struct {
	clients    map[string]*LogicClient     // 逻辑服客户端映射（serverID -> client）
	ordered    []string                    // 有序的服务 ID 列表，用于确定性轮询
	mu         sync.RWMutex                // 读写锁
	gateway    GatewayInterface            // 网关接口引用
	discovery  *uetcd.Component            // 服务发现组件
	balancer   *cluster.Balancer           // 负载均衡器
	stopCh     chan struct{}               // 停止信号
	wg         sync.WaitGroup              // 等待协程退出
	rrIndex    atomic.Uint64               // 轮询索引（原子操作）
	fastClient atomic.Pointer[LogicClient] // 快速路径：单客户端时的原子指针
	addressMap map[string]string           // 地址映射（serverID -> address，来自 etcd）
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
		clients:    make(map[string]*LogicClient),
		addressMap: make(map[string]string),
		gateway:    gateway,
		stopCh:     make(chan struct{}),
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

// LookupAddress 从 etcd 维护的映射中获取指定 serverID 的地址
func (pool *LogicClientPool) LookupAddress(serverID string) string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.addressMap[serverID]
}

// IsClientConnected 检查指定 serverID 的客户端是否已连接。
func (pool *LogicClientPool) IsClientConnected(serverID string) bool {
	pool.mu.RLock()
	client, ok := pool.clients[serverID]
	pool.mu.RUnlock()
	return ok && client != nil && client.IsConnected()
}

// SetDiscovery 设置服务发现组件并监听服务变更。
// 注册回调后立即重放已知服务，避免因 discovery 启动先于回调注册而丢失初始快照。
func (pool *LogicClientPool) SetDiscovery(discovery *uetcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
	svcs := discovery.ServiceSet()
	tlog.Info(context.TODO(), "SetDiscovery: replaying known services count=%d", len(svcs))
	for fullKey, address := range svcs {
		instanceID := fullKey[strings.LastIndex(fullKey, "/")+1:]
		tlog.Info(context.TODO(), "SetDiscovery: replaying service instanceID=%s address=%s", instanceID, address)
		pool.handleServiceRegister(uetcd.ServiceEvent{
			Type:       uetcd.EventRegister,
			ServiceID:  discovery.ServiceID(),
			InstanceID: instanceID,
			Address:    address,
		})
	}
}

// SetBalancer 设置负载均衡器
func (pool *LogicClientPool) SetBalancer(balancer *cluster.Balancer) {
	pool.balancer = balancer
}

// handleServiceChange 处理服务注册/注销事件
func (pool *LogicClientPool) handleServiceChange(event uetcd.ServiceEvent) {
	switch event.Type {
	case uetcd.EventRegister:
		pool.handleServiceRegister(event)
	case uetcd.EventDeregister:
		pool.handleServiceDeregister(event)
	}
}

// handleServiceRegister 处理服务注册事件，创建新的逻辑服客户端连接
func (pool *LogicClientPool) handleServiceRegister(event uetcd.ServiceEvent) {
	pool.mu.Lock()
	if existing, exists := pool.clients[event.InstanceID]; exists && existing != nil {
		pool.mu.Unlock()
		return
	}

	client := NewLogicClient(pool.gateway)
	client.SetServerID(event.InstanceID)
	client.shardCount = runtime.NumCPU() * 8
	pool.clients[event.InstanceID] = client
	pool.addressMap[event.InstanceID] = event.Address
	if !slices.Contains(pool.ordered, event.InstanceID) {
		pool.ordered = append(pool.ordered, event.InstanceID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()

	go func() {
		tlog.Info(context.TODO(), "connecting to discovered logic service serviceID=%s address=%s",
			event.InstanceID,
			event.Address,
		)
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		const maxAttempts = 60
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if err := client.Connect(event.Address); err == nil {
				tlog.Info(context.TODO(), "connected to discovered logic service serviceID=%s address=%s",
					event.InstanceID,
					event.Address,
				)
				return
			} else {
				client.mu.RLock()
				closing := client.closing
				client.mu.RUnlock()
				if closing {
					return
				}
				if attempt == 1 || attempt%10 == 0 {
					tlog.Warn(context.TODO(), "logic service connection failed, retrying serviceID=%s address=%s attempt=%d error=%v",
						event.InstanceID,
						event.Address,
						attempt,
						err,
					)
				}
				time.Sleep(backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
		tlog.Error(context.TODO(), "logic service connection gave up after %d attempts serviceID=%s address=%s", maxAttempts, event.InstanceID, event.Address)
	}()

	if pool.balancer != nil {
		pool.balancer.AddNode(event.InstanceID, event.Address, 1)
	}

	tlog.Info(context.TODO(), "logic client added to pool serviceID=%s address=%s totalClients=%d",
		event.InstanceID,
		event.Address,
		pool.ClientCount(),
	)
}

// handleServiceDeregister 处理服务注销事件
// 注意：不立即删除和关闭连接，避免 etcd 租约过期但 gRPC 连接仍可用时的误判
// 让健康检查器和 gRPC 流自身错误检测来处理真正的连接断开
func (pool *LogicClientPool) handleServiceDeregister(event uetcd.ServiceEvent) {
	// 服务发现租约可能在现有 gRPC 连接仍可用时过期。
	// 不立即从连接池删除和关闭连接，避免误判导致转发中断。
	// 让 HealthChecker 和 gRPC 流自身错误检测来处理真正的连接断开。
	pool.mu.RLock()
	client, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && client != nil {
		if !client.IsConnected() {
			// gRPC 连接已断开，安全清理
			pool.mu.Lock()
			delete(pool.clients, event.InstanceID)
			delete(pool.addressMap, event.InstanceID)
			pool.ordered = slices.DeleteFunc(pool.ordered, func(v string) bool { return v == event.InstanceID })
			pool.updateFastClient()
			pool.mu.Unlock()
			if pool.balancer != nil {
				pool.balancer.RemoveNode(event.InstanceID)
			}
			go client.Close()
			tlog.Warn(context.TODO(), "logic service offline and connection already disconnected, cleaning up serviceID=%s address=%s",
				event.InstanceID,
				event.Address,
			)
		} else {
			// gRPC 连接仍存活，保留连接，等服务重新注册或 HealthChecker 检测到断开
			tlog.Warn(context.TODO(), "logic service deregistered from etcd, keeping gRPC connection (still connected) serviceID=%s address=%s",
				event.InstanceID,
				event.Address,
			)
		}
	}

	tlog.Warn(context.TODO(), "logic client deregister event processed serviceID=%s address=%s totalClients=%d",
		event.InstanceID,
		event.Address,
		pool.ClientCount(),
	)
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

// Close 关闭客户端池中所有连接
func (pool *LogicClientPool) Close() {
	close(pool.stopCh)
	pool.wg.Wait()

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for id, client := range pool.clients {
		client.Close()
		delete(pool.clients, id)
	}
	pool.ordered = pool.ordered[:0]
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
