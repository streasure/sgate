package backend

import (
	"context"
	"fmt"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type GatewayClient struct {
	client     protoGw.GatewayClient // gRPC 客户端
	conn       *grpc.ClientConn      // gRPC 连接
	address    string                // 目标网关地址
	serverID   string                // 目标网关标识
	mu         sync.RWMutex          // 读写锁
	closing    bool                  // 是否正在关闭
	connecting bool                  // 是否正在连接中（Connect 期间）
}

// NewGatewayClient 创建网关客户端实例
func NewGatewayClient(serverID, address string) *GatewayClient {
	return &GatewayClient{
		address:  address,
		serverID: serverID,
	}
}

// Connect 建立到目标网关的 gRPC 连接
func (gc *GatewayClient) Connect() error {
	// 标记正在连接中，阻止 Close() 在连接过程中直接关闭
	gc.mu.Lock()
	gc.connecting = true
	gc.mu.Unlock()

	defer func() {
		gc.mu.Lock()
		gc.connecting = false
		wasClosing := gc.closing
		gc.mu.Unlock()
		// 如果 Connect 期间有 Close() 被调用，这里完成实际关闭
		if wasClosing && gc.conn != nil {
			gc.conn.Close()
		}
	}()

	conn, err := grpc.NewClient(gc.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("dial gateway %s (%s): %w", gc.serverID, gc.address, err)
	}

	gc.mu.Lock()
	if gc.closing {
		gc.mu.Unlock()
		conn.Close()
		// 连接期间被关闭属于正常生命周期事件，不返回错误
		return nil
	}
	gc.conn = conn
	gc.client = protoGw.NewGatewayClient(conn)
	gc.mu.Unlock()

	tlog.Info(context.TODO(), "网关客户端已创建 serverID=%s address=%s", gc.serverID, gc.address)
	return nil
}

// IsConnected 检查网关客户端连接状态
func (gc *GatewayClient) IsConnected() bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.conn != nil && !gc.closing && gc.conn.GetState() != connectivity.Shutdown
}

// Close 关闭网关客户端连接
func (gc *GatewayClient) Close() {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.closing {
		return
	}
	gc.closing = true
	// 如果正在连接中，由 Connect() 的 defer 完成实际关闭
	if gc.connecting {
		return
	}
	if gc.conn != nil {
		gc.conn.Close()
	}
}

// GatewayClientPool 网关客户端池，管理通过 etcd 发现的其他网关实例的连接
// 结构类似于 LogicClientPool，但更简单，因为网关间通信使用 Unary RPC（无流）
type GatewayClientPool struct {
	clients    map[string]*GatewayClient // 网关客户端映射
	gens       map[string]uint64         // 每个 serverID 的注册代次，防止 deregister 误关新 client
	mu         sync.RWMutex              // 读写锁
	discovery  *uetcd.Component          // 服务发现组件
	addressMap map[string]string         // 地址映射（serverID → address）
	selfID     string                    // 本实例 ID（排除自身）
	nextGen    uint64                    // 全局递增代次计数器
}

func NewGatewayClientPool(gateway GatewayInterface) *GatewayClientPool {
	return &GatewayClientPool{
		clients:    make(map[string]*GatewayClient),
		gens:       make(map[string]uint64),
		addressMap: make(map[string]string),
		selfID:     gateway.GetServerID(),
	}
}

func (pool *GatewayClientPool) LoadEvents(events []uetcd.ServiceEvent) {
	for _, event := range events {
		pool.handleServiceChange(event)
	}
}

func (pool *GatewayClientPool) GetClient(serverID string) GatewayClientProvider {
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	return client
}

func (pool *GatewayClientPool) SetDiscovery(discovery *uetcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *GatewayClientPool) handleServiceChange(event uetcd.ServiceEvent) {
	// 专用发现组件监听 Gateway:{zone}，忽略当前网关实例自身。
	if event.InstanceID == pool.selfID {
		return
	}
	switch event.Type {
	case uetcd.EventRegister:
		pool.handleRegister(event)
	case uetcd.EventDeregister:
		pool.handleDeregister(event)
	}
}

func (pool *GatewayClientPool) handleRegister(event uetcd.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && existing != nil && existing.IsConnected() && existing.address == event.Address {
		return
	}

	// 清理已经失效的旧条目。
	if exists && existing != nil && !existing.IsConnected() {
		pool.mu.Lock()
		delete(pool.clients, event.InstanceID)
		delete(pool.addressMap, event.InstanceID)
		delete(pool.gens, event.InstanceID)
		pool.mu.Unlock()
		go existing.Close()
	}

	// 递增代次，后续 deregister 事件只能关闭此代次之前的 client
	pool.mu.Lock()
	pool.nextGen++
	gen := pool.nextGen
	pool.gens[event.InstanceID] = gen
	pool.mu.Unlock()

	client := NewGatewayClient(event.InstanceID, event.Address)
	go func() {
		tlog.Info(context.TODO(), "正在连接已发现的网关 serverID=%s address=%s", event.InstanceID, event.Address)
		if err := client.Connect(); err != nil {
			tlog.Error(context.TODO(), "连接已发现的网关失败 serverID=%s address=%s error=%v",
				event.InstanceID, event.Address, err)
			return
		}
		tlog.Info(context.TODO(), "已发现的网关连接就绪 serverID=%s address=%s", event.InstanceID, event.Address)
	}()

	pool.mu.Lock()
	// 如果代次已被更新（新的 register 已到来），不再覆盖
	if pool.gens[event.InstanceID] == gen {
		pool.clients[event.InstanceID] = client
		pool.addressMap[event.InstanceID] = event.Address
	}
	pool.mu.Unlock()

	tlog.Info(context.TODO(), "网关客户端已加入池 serverID=%s address=%s gen=%d totalClients=%d",
		event.InstanceID, event.Address, gen, pool.ClientCount())
}

func (pool *GatewayClientPool) handleDeregister(event uetcd.ServiceEvent) {
	// 取出当前注册的 client 和代次
	pool.mu.Lock()
	client, exists := pool.clients[event.InstanceID]
	gen := pool.gens[event.InstanceID]
	if exists {
		delete(pool.clients, event.InstanceID)
		delete(pool.addressMap, event.InstanceID)
		delete(pool.gens, event.InstanceID)
	}
	// 记录当前全局代次，用于判断是否有新的 register 已到来
	currentGen := pool.nextGen
	pool.mu.Unlock()

	if client != nil {
		if !client.IsConnected() {
			go client.Close()
			tlog.Warn(context.TODO(), "网关下线且连接已断开，安全清理 serverID=%s address=%s",
				event.InstanceID, event.Address)
		} else if gen < currentGen {
			// 此 client 对应的代次已过时（有新的 register 已到来），关闭旧 client
			go client.Close()
			tlog.Warn(context.TODO(), "网关代次已更新，关闭旧连接 serverID=%s address=%s gen=%d currentGen=%d",
				event.InstanceID, event.Address, gen, currentGen)
		} else {
			tlog.Warn(context.TODO(), "网关已从 etcd 注销，但 gRPC 连接仍存活，保留连接 serverID=%s address=%s",
				event.InstanceID, event.Address)
		}
	}

	tlog.Warn(context.TODO(), "网关注销事件处理完成 serverID=%s address=%s totalClients=%d",
		event.InstanceID, event.Address, pool.ClientCount())
}

func (pool *GatewayClientPool) ClientCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return len(pool.clients)
}

func (pool *GatewayClientPool) IsConnected() bool {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, c := range pool.clients {
		if c.IsConnected() {
			return true
		}
	}
	return false
}

func (pool *GatewayClientPool) Close() {
	pool.mu.Lock()
	clients := make([]*GatewayClient, 0, len(pool.clients))
	for _, c := range pool.clients {
		clients = append(clients, c)
	}
	pool.clients = make(map[string]*GatewayClient)
	pool.addressMap = make(map[string]string)
	pool.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
}
