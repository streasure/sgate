package gateway

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/panjf2000/gnet/v2"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cast"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

var (
	frameHeaderPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 4)
			return &buf
		},
	}
)

// MemoryMonitor 内存监控器
// 功能: 监控系统内存使用情况，包括堆内存、缓冲区使用等
type MemoryMonitor struct {
	stopChan chan struct{}
	ticker   *time.Ticker
}

// NewMemoryMonitor 创建内存监控器
func NewMemoryMonitor() *MemoryMonitor {
	return &MemoryMonitor{
		stopChan: make(chan struct{}),
		ticker:   time.NewTicker(5 * time.Second),
	}
}

// Start 启动内存监控
func (mm *MemoryMonitor) Start(g *Gateway) {
	go func() {
		for {
			select {
			case <-mm.stopChan:
				return
			case <-mm.ticker.C:
				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)

				// 输出内存使用日志
				tlog.Debug("内存使用情况",
					"heapAlloc", cast.ToString(float64(memStats.HeapAlloc)/1024/1024)+" MB",
					"heapObjects", memStats.HeapObjects,
					"bufferUsage", cast.ToString(float64(g.bufferUsage.Load())/1024/1024)+" MB",
					"bufferCount", g.bufferCount.Load(),
					"objectPoolUsage", cast.ToString(float64(g.objectPoolUsage.Load())/1024/1024)+" MB",
				)
			}
		}
	}()
}

// Stop 停止内存监控
func (mm *MemoryMonitor) Stop() {
	mm.ticker.Stop()
	close(mm.stopChan)
}

// Gateway 网关结构
// 功能: 整个网关服务的核心结构，管理所有连接、路由、消息处理等
// 字段:
//   connectionManager: 连接管理器，管理所有网络连接
//   routeManager: 路由管理器，管理所有路由
//   messagePool: 消息队列，存储待处理的消息
//   workerPool: 工作池，用于管理工作线程
//   stopChan: 停止信号通道
//   workerStopChan: 工作线程停止信号通道
//   metrics: 指标收集器，用于收集系统指标
//   transportType: 端口到传输类型的映射
//   rateLimiter: 速率限制器，用于限制请求速率
//   authSecret: 认证密钥
//   authRoutes: 需要认证的路由
//   ctx: 上下文
//   tlsConfig: TLS配置
//   clusterID: 集群ID
//   isLeader: 是否是领导者
//   bufferPool: 缓冲区池，用于复用缓冲区
//   whitelistBlacklist: 白名单和黑名单管理器
//   workerMutex: 工作池互斥锁
//   workerCount: 当前工作线程数
//   minWorkers: 最小工作线程数
//   maxWorkers: 最大工作线程数
//   workerQueueSize: 工作队列大小阈值
//   cfg: 配置实例
//   wsConnections: 活跃的WebSocket连接
//   configPath: 配置文件路径
//   configUpdateChan: 配置更新通道
//   cache: 缓存管理器
//   loadBalancer: 负载均衡器

// SetTransportType 设置端口到传输类型的映射
// 功能: 设置端口与传输类型的对应关系，用于区分不同端口的传输协议
// 参数:
//
//	port: 端口号
//	transportType: 传输类型，如"websocket"
func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType[port] = transportType
}

// GetBuffer 从缓冲区池获取缓冲区
// 功能: 从缓冲区池获取一个缓冲区，用于网络IO操作
// 参数:
//
//	size: 预期缓冲区大小
//
// 返回值:
//
//	[]byte: 缓冲区
func (g *Gateway) GetBuffer(size int) []byte {
	buf := g.bufferPool.Get().([]byte)
	// 如果缓冲区大小不够，扩容
	if cap(buf) < size {
		// 计算新的缓冲区大小，确保不超过最大缓冲区大小
		newSize := cap(buf) * 2
		if newSize < size {
			newSize = size
		}
		if newSize > g.maxBufferSize {
			newSize = g.maxBufferSize
		}
		// 重新分配缓冲区
		newBuf := make([]byte, newSize)
		g.bufferPool.Put(buf) // 归还旧缓冲区
		// 更新内存使用统计
		g.bufferCount.Add(1)
		g.bufferUsage.Add(int64(cap(newBuf)))
		return newBuf
	}
	// 重置缓冲区长度
	// 更新内存使用统计
	g.bufferCount.Add(1)
	g.bufferUsage.Add(int64(cap(buf)))
	return buf[:cap(buf)]
}

// PutBuffer 将缓冲区归还到缓冲区池
// 功能: 将缓冲区归还到缓冲区池，以便复用
// 参数:
//
//	buf: 缓冲区
func (g *Gateway) PutBuffer(buf []byte) {
	// 只有当缓冲区大小在合理范围内时才归还
	if cap(buf) >= g.minBufferSize && cap(buf) <= g.maxBufferSize {
		// 重置缓冲区长度
		buf = buf[:cap(buf)]
		g.bufferPool.Put(buf)
		// 更新内存使用统计
		g.bufferCount.Add(-1)
		g.bufferUsage.Add(-int64(cap(buf)))
	} else {
		// 否则让垃圾回收器处理
		g.bufferCount.Add(-1)
		g.bufferUsage.Add(-int64(cap(buf)))
	}
}

// Message 消息结构
// 功能: 定义网关内部传递的消息结构
// 字段:
//   ConnectionID: 连接ID，用于标识消息来源
//   Route: 路由名称，用于确定消息的处理逻辑
//   Payload: 消息负载，包含消息的具体内容
//   Conn: 网络连接，用于回复消息

type Message struct {
	ConnectionID string            `json:"connection_id"` // 连接ID
	Route        string            `json:"route"`         // 路由名称
	Payload      map[string]string `json:"payload"`       // 消息负载
	Conn         gnet.Conn         `json:"conn"`          // 网络连接
	TraceID      string            `json:"trace_id"`      // 追踪ID
}

// protobufMessagePool Protocol Buffers 消息对象池
// 功能: 复用 Protocol Buffers 消息对象，减少内存分配
var protobufMessagePool = sync.Pool{
	New: func() interface{} {
		return &protobuf.Message{
			Payload: make(map[string]string, 32), // 预分配更大的空间，减少扩容
		}
	},
}

// 预分配的Protocol Buffers消息对象数量
const preallocatedProtobufMessages = 64

// 初始化Protocol Buffers消息对象池
func init() {
	// 预分配消息对象，减少运行时分配
	for i := 0; i < preallocatedProtobufMessages; i++ {
		protobufMessagePool.Put(&protobuf.Message{
			Payload: make(map[string]string, 32),
		})
	}
}

// GetProtobufMessage 从对象池获取 Protocol Buffers 消息对象
// 功能: 从消息对象池获取一个消息对象，并清空其内容
// 返回值:
//
//	*protobuf.Message: 消息对象
func GetProtobufMessage() *protobuf.Message {
	msg := protobufMessagePool.Get().(*protobuf.Message)
	// 快速清空消息对象
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	// 清空payload，保持容量不变
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	} else {
		msg.Payload = make(map[string]string, 32)
	}
	// 注意：由于对象池是全局的，这里无法直接更新Gateway的对象池使用统计
	// 可以考虑将对象池移到Gateway结构中，或者使用全局计数器
	return msg
}

// PutProtobufMessage 将 Protocol Buffers 消息对象归还到对象池
// 功能: 将消息对象归还到对象池，清空其内容以便复用
// 参数:
//
//	msg: 消息对象
func PutProtobufMessage(msg *protobuf.Message) {
	if msg == nil {
		return
	}
	// 快速清空消息对象
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	// 清空payload，保持容量不变
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
	protobufMessagePool.Put(msg)
}

// messagePool 消息对象池
// 功能: 复用消息对象，减少内存分配
var messagePool = sync.Pool{
	New: func() interface{} {
		return &Message{
			Payload: make(map[string]string, 32), // 预分配更大的空间，减少扩容
		}
	},
}

// 预分配的消息对象数量
const preallocatedMessages = 64

// 初始化消息对象池
func init() {
	// 预分配消息对象，减少运行时分配
	for i := 0; i < preallocatedMessages; i++ {
		messagePool.Put(&Message{
			Payload: make(map[string]string, 32),
		})
	}
}

// GetMessage 从对象池获取消息对象
// 功能: 从消息对象池获取一个消息对象，并清空其内容
// 返回值:
//
//	*Message: 消息对象
func GetMessage() *Message {
	msg := messagePool.Get().(*Message)
	// 快速清空消息对象
	msg.ConnectionID = ""
	msg.Route = ""
	// 清空payload，保持容量不变
	for k := range msg.Payload {
		delete(msg.Payload, k)
	}
	msg.Conn = nil
	return msg
}

// PutMessage 将消息对象归还到对象池
// 功能: 将消息对象归还到对象池，清空其内容以便复用
// 参数:
//
//	msg: 消息对象
func PutMessage(msg *Message) {
	if msg == nil {
		return
	}
	// 快速清空消息对象
	msg.ConnectionID = ""
	msg.Route = ""
	// 清空payload，保持容量不变
	for k := range msg.Payload {
		delete(msg.Payload, k)
	}
	msg.Conn = nil
	messagePool.Put(msg)
}

// Gateway 网关结构
// 字段:
//   connectionManager: 连接管理器
//   routeManager: 路由管理器
//   messagePool: 消息队列
//   workerPool: 工作池
//   stopChan: 停止信号通道
//   metrics: 指标收集器
//   transportType: 端口到传输类型的映射
//   rateLimiter: 速率限制器
//   authSecret: 认证密钥

type Gateway struct {
	connectionManager     *ConnectionManager         // 连接管理器
	routeManager          *RouteManager              // 路由管理器
	messagePool           chan *Message              // 消息队列
	workerPool            sync.WaitGroup             // 工作池
	stopChan              chan struct{}              // 停止信号通道
	workerStopChan        chan struct{}              // 工作线程停止信号通道
	metrics               *metrics.Metrics           // 指标收集器
	transportType         map[string]string          // 端口到传输类型的映射
	rateLimiter           *RateLimiter               // 速率限制器
	authSecret            atomic.Value               // 认证密钥，使用atomic.Value存储
	authRoutes            atomic.Value               // 需要认证的路由，使用atomic.Value存储
	ctx                   context.Context            // 上下文
	tlsConfig             *tls.Config                // TLS配置
	clusterID             string                     // 集群ID
	isLeader              bool                       // 是否是领导者
	bufferPool            *sync.Pool                 // 缓冲区池
	minBufferSize         int                        // 最小缓冲区大小
	maxBufferSize         int                        // 最大缓冲区大小
	defaultBufferSize     int                        // 默认缓冲区大小
	whitelistBlacklist    *WhitelistBlacklist        // 白名单和黑名单管理器
	workerCount           atomic.Int32               // 当前工作线程数，使用atomic.Int32存储
	minWorkers            atomic.Int32               // 最小工作线程数，使用atomic.Int32存储
	maxWorkers            atomic.Int32               // 最大工作线程数，使用atomic.Int32存储
	workerQueueSize       atomic.Int32               // 工作队列大小阈值，使用atomic.Int32存储
	cfg                   atomic.Value               // 配置实例，使用atomic.Value存储
	wsConnections         sync.Map                   // 活跃的WebSocket连接
	configPath            string                     // 配置文件路径
	configUpdateChan      chan *config.Config        // 配置更新通道
	cache                 *Cache                     // 缓存管理器
	loadBalancer          *LoadBalancer              // 负载均衡器
	messageIntegrity      *MessageIntegrity          // 消息完整性管理器
	messageACK            *MessageACK                // 消息确认管理器
	database              *Database                  // 数据库管理器
	redis                 *DistributedManager        // 缓存管理器(已替换为内存)
	compressor            *Compressor                // 压缩管理器
	versionNegotiation    *VersionNegotiation        // 版本协商管理器
	circuitBreakerManager *CircuitBreakerManager     // 熔断器管理器
	messageQueue          *MessageQueue              // 消息队列管理器
	tracer                *Tracer                    // 链路追踪器
	logicClient           *LogicClient               // 逻辑服 gRPC 客户端
	logicClientPool       *LogicClientPool           // 逻辑服客户端池
	serviceDiscovery      *ServiceDiscovery          // 服务发现
	redisClient           *redis.Client              // Redis客户端
	userRateLimitConfig   config.UserRateLimitConfig // 用户维度限流配置
	// 内存管理相关
	memoryMonitor   *MemoryMonitor // 内存监控器
	bufferUsage     atomic.Int64   // 缓冲区使用量
	bufferCount     atomic.Int64   // 缓冲区数量
	objectPoolUsage atomic.Int64   // 对象池使用量
	fastPath        *fastPathCache // 超级快速路径缓存
}

// NewGateway 创建网关实例
// 返回值:
//
//	*Gateway: 网关实例
func NewGateway() *Gateway {
	ctx := context.Background()

	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("加载配置失败，使用默认配置", "error", err)
	}

	switch cfg.LogLevel {
	case "debug":
		tlog.SetLevel("debug")
	case "info":
		tlog.SetLevel("info")
	case "warn":
		tlog.SetLevel("warn")
	case "error":
		tlog.SetLevel("error")
	default:
		// tlog.SetLevel("info")
	}

	// 初始化白名单和黑名单管理器
	whitelistBlacklist := NewWhitelistBlacklist()

	// 从配置中读取工作池参数
	minWorkers := cfg.WorkerPool.MinWorkers
	if minWorkers <= 0 {
		minWorkers = runtime.GOMAXPROCS(0) * 4 // 最小工作线程数为CPU核心数的4倍
	}
	maxWorkers := cfg.WorkerPool.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = runtime.GOMAXPROCS(0) * 16 // 最大工作线程数为CPU核心数的16倍
	}
	queueSize := cfg.WorkerPool.QueueSize
	if queueSize <= 0 {
		queueSize = 2000000 // 增大队列大小
	}
	workerQueueSize := cfg.WorkerPool.QueueSizeThreshold
	if workerQueueSize <= 0 {
		workerQueueSize = 5000 // 增大队列大小阈值
	}

	// 从配置中读取速率限制参数
	rateLimitRate := cfg.RateLimiter.Rate
	if rateLimitRate <= 0 {
		rateLimitRate = 1000000 // 增大速率限制
	}
	rateLimitWindow := cfg.RateLimiter.Window
	if rateLimitWindow <= 0 {
		rateLimitWindow = time.Second // 默认时间窗口
	}

	// 从配置中读取安全参数
	authSecret := cfg.Security.AuthSecret
	if authSecret == "" {
		authSecret = "default_secret" // 默认认证密钥
	}

	// 初始化需要认证的路由
	authRoutes := make(map[string]bool)
	for _, route := range cfg.Security.AuthRoutes {
		authRoutes[route] = true
	}

	gw := &Gateway{
		connectionManager: NewConnectionManager(),                         // 创建连接管理器
		routeManager:      NewRouteManager(),                              // 创建路由管理器
		messagePool:       make(chan *Message, queueSize),                 // 创建消息队列
		stopChan:          make(chan struct{}),                            // 创建停止信号通道
		workerStopChan:    make(chan struct{}),                            // 创建工作线程停止信号通道
		metrics:           metrics.NewMetrics(),                           // 创建指标收集器
		transportType:     make(map[string]string),                        // 创建端口到传输类型的映射
		rateLimiter:       NewRateLimiter(rateLimitRate, rateLimitWindow), // 创建速率限制器
		ctx:               ctx,
		tlsConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS13,
		},
		clusterID:         "sgate-cluster", // 集群ID
		isLeader:          false,           // 默认不是领导者
		minBufferSize:     4096,            // 4KB最小缓冲区
		maxBufferSize:     65536,           // 64KB最大缓冲区
		defaultBufferSize: 16384,           // 16KB默认缓冲区
		bufferPool: &sync.Pool{
			New: func() interface{} {
				// 预分配16KB缓冲区，减少内存分配和拷贝
				return make([]byte, 16384)
			},
		},
		whitelistBlacklist:  whitelistBlacklist,                    // 白名单和黑名单管理器
		workerCount:         atomic.Int32{},                        // 当前工作线程数，使用atomic.Int32存储
		configPath:          "config/config.yaml",                  // 配置文件路径
		configUpdateChan:    make(chan *config.Config),             // 配置更新通道
		cache:               NewCache(),                            // 缓存管理器
		loadBalancer:        NewLoadBalancer(),                     // 负载均衡器
		memoryMonitor:       NewMemoryMonitor(),                    // 内存监控器
		userRateLimitConfig: cfg.RateLimiter.UserRateLimit,         // 用户维度限流配置
		logicClient:         NewLogicClient(GatewayInterface(nil)), // 逻辑服 gRPC 客户端
	}

	// 使用atomic.Value存储配置
	gw.authSecret.Store(authSecret)
	gw.authRoutes.Store(authRoutes)
	gw.minWorkers.Store(int32(minWorkers))
	gw.maxWorkers.Store(int32(maxWorkers))
	gw.workerQueueSize.Store(int32(workerQueueSize))
	gw.cfg.Store(cfg)

	// 启动内存监控器
	gw.memoryMonitor.Start(gw)

	// 注册默认路由
	gw.registerDefaultRoutes()

	// 启动消息处理工作池
	for i := 0; i < minWorkers; i++ {
		gw.addWorker()
	}

	// 启动工作池管理器
	go gw.workerPoolManager()

	// 启动WebSocket心跳检查
	go gw.wsHeartbeatChecker()

	// 初始化消息完整性管理器
	gw.messageIntegrity = NewMessageIntegrity(30000) // 30秒时间窗口

	// 初始化消息确认管理器
	gw.messageACK = NewMessageACK(3, 2*time.Second, 30*time.Second) // 最大重试3次，重试间隔2秒，超时30秒
	gw.messageACK.Start()

	// 初始化数据库
	dbConfig := DatabaseConfig{
		Host:          "localhost",
		Port:          5432,
		User:          "sgate",
		Password:      "sgate_password",
		Database:      "sgate",
		MaxIdleConns:  10,
		MaxOpenConns:  100,
		ConnTimeout:   5 * time.Second,
		RetryInterval: 2 * time.Second,
		MaxRetries:    5,
	}
	gw.database = NewDatabase(dbConfig)

	// 初始化内存缓存管理器
	gw.redis = newDistributedManager()

	// 初始化压缩管理器
	gw.compressor = NewCompressor()

	// 初始化版本协商管理器
	supportedVersions := []string{"1.0.0", "1.1.0", "2.0.0"} // 支持的协议版本
	gw.versionNegotiation = NewVersionNegotiation(supportedVersions, 10*time.Second)

	// 初始化熔断器管理器
	gw.circuitBreakerManager = NewCircuitBreakerManager()

	// 初始化消息队列
	gw.messageQueue = NewMessageQueue(5*time.Second, 3)

	// 初始化链路追踪器
	gw.tracer = NewTracer(5 * time.Minute)

	// 启动连接检查器，清理不活跃连接
	gw.connectionManager.StartConnectionChecker(5*time.Minute, 30*time.Second)

	// 启动配置热加载监听器
	go gw.configWatcher()

	// 启动配置更新处理
	go func() {
		for {
			select {
			case <-gw.stopChan:
				return
			case newCfg := <-gw.configUpdateChan:
				gw.handleConfigUpdate(newCfg)
			}
		}
	}()

	gw.logicClient.gateway = gw

	gw.logicClientPool = NewLogicClientPool(gw)

	if cfg.Discovery.Enabled && cfg.Redis.Addr != "" {
		gw.redisClient = redis.NewClient(&redis.Options{
			Addr:         cfg.Redis.Addr,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			PoolSize:     cfg.Redis.PoolSize,
			MinIdleConns: cfg.Redis.MinIdleConns,
		})

		ctx := context.Background()
		if err := gw.redisClient.Ping(ctx).Err(); err != nil {
			tlog.Warn("Redis connection failed, service discovery disabled", "error", err)
			gw.redisClient = nil
		} else {
			tlog.Info("Redis connected, starting service discovery", "addr", cfg.Redis.Addr)

			gw.serviceDiscovery = NewServiceDiscovery(gw.redisClient, cfg.Discovery)
			gw.logicClientPool.SetDiscovery(gw.serviceDiscovery)

			if err := gw.serviceDiscovery.Start(); err != nil {
				tlog.Error("service discovery start failed", "error", err)
			}
		}
	}

	if gw.serviceDiscovery == nil {
		tlog.Info("service discovery disabled, using static logic server connection")
		go func() {
			tlog.Info("waiting before connecting to logic server", "delay", "2s")
			time.Sleep(2 * time.Second)
			tlog.Info("connecting to logic server", "address", "localhost:50052")
			if err := gw.logicClient.Connect("localhost:50052"); err != nil {
				tlog.Error("failed to connect to logic server", "error", err)
			} else {
				tlog.Info("successfully connected to logic server")
			}
		}()
	}

	tlog.Info("about to start gRPC server", "port", ":50051")
	go func() {
		tlog.Info("starting gRPC server", "port", ":50051")
		if err := StartGRPCServer(gw, ":50051"); err != nil {
			tlog.Error("failed to start gRPC server", "error", err)
		} else {
			tlog.Info("gRPC server started", "port", ":50051")
		}
	}()

	gw.fastPath = buildFastPathCache()

	return gw
}

// addWorker 添加工作线程
func (g *Gateway) addWorker() {
	if g.workerCount.Load() >= g.maxWorkers.Load() {
		return
	}

	// 使用原子操作增加工作线程数
	g.workerCount.Add(1)
	g.workerPool.Add(1)
	go g.messageWorker()
}

// removeWorker 移除工作线程
func (g *Gateway) removeWorker() {
	if g.workerCount.Load() <= g.minWorkers.Load() {
		return
	}

	// 使用原子操作减少工作线程数
	g.workerCount.Add(-1)
	// 向工作线程发送停止信号
	select {
	case g.workerStopChan <- struct{}{}:
		// 成功发送停止信号
	default:
		// 通道已满，忽略
	}
}

// workerPoolManager 工作池管理器
func (g *Gateway) workerPoolManager() {
	ticker := time.NewTicker(50 * time.Millisecond) // 进一步缩短检查间隔，提高响应速度
	defer ticker.Stop()

	// 记录历史负载，用于平滑调整
	historyLoad := make([]float64, 0, 20)           // 增加历史记录长度
	historyQueueLength := make([]int, 0, 20)        // 增加历史记录长度
	historyProcessingTime := make([]float64, 0, 20) // 记录历史处理时间
	historyWorkerCount := make([]int32, 0, 20)      // 记录历史工作线程数

	// 自适应调整参数
	addThreshold := 0.25    // 降低添加线程的阈值
	removeThreshold := 0.03 // 降低移除线程的阈值

	// 线程监控统计
	threadStats := struct {
		totalThreadsCreated int64   // 总创建线程数
		totalThreadsRemoved int64   // 总移除线程数
		peakThreadCount     int32   // 峰值线程数
		avgThreadCount      float64 // 平均线程数
		totalThreadTime     int64   // 总线程运行时间
	}{}

	// 记录上次输出时间
	lastLogTime := time.Now()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			// 检查消息队列长度
			queueLength := len(g.messagePool)
			// 检查活跃连接数
			activeConnections := g.connectionManager.GetConnectionCount()
			// 检查平均处理时间
			averageProcessingTime := g.metrics.GetAverageProcessingTime()
			// 检查当前工作线程数
			currentWorkers := g.workerCount.Load()

			// 计算负载指标
			loadFactor := float64(queueLength) / float64(g.workerQueueSize.Load())
			connectionFactor := float64(activeConnections) / float64(150) // 每150个连接增加一个线程
			timeFactor := averageProcessingTime / float64(1.5)            // 处理时间超过1.5ms增加线程

			// 综合负载指标，使用加权平均
			totalLoad := loadFactor*0.4 + connectionFactor*0.2 + timeFactor*0.4

			// 添加到历史记录
			historyLoad = append(historyLoad, totalLoad)
			historyQueueLength = append(historyQueueLength, queueLength)
			historyProcessingTime = append(historyProcessingTime, float64(averageProcessingTime))
			historyWorkerCount = append(historyWorkerCount, currentWorkers)
			if len(historyLoad) > 20 {
				historyLoad = historyLoad[1:]
				historyQueueLength = historyQueueLength[1:]
				historyProcessingTime = historyProcessingTime[1:]
				historyWorkerCount = historyWorkerCount[1:]
			}

			// 计算指数移动平均值，更准确地反映近期负载
			avgLoad := g.calculateEMA(historyLoad, 0.3)
			// 计算队列长度趋势（更复杂的趋势分析）
			queueTrend := g.calculateTrend(historyQueueLength)
			// 计算处理时间趋势
			processingTimeTrend := g.calculateTrend(historyProcessingTime)

			// 自适应调整阈值
			if queueTrend > 0 && processingTimeTrend > 0 {
				// 负载呈上升趋势，降低添加阈值
				addThreshold = 0.2
			} else if queueTrend < 0 && processingTimeTrend < 0 {
				// 负载呈下降趋势，降低移除阈值
				removeThreshold = 0.02
			} else {
				// 恢复默认阈值
				addThreshold = 0.25
				removeThreshold = 0.03
			}

			maxWorkers := g.maxWorkers.Load()
			minWorkers := g.minWorkers.Load()

			if avgLoad > addThreshold && currentWorkers < maxWorkers {
				targetWorkers := currentWorkers + int32(float64(currentWorkers)*0.3) + 1
				if targetWorkers > maxWorkers {
					targetWorkers = maxWorkers
				}
				addCount := int(targetWorkers - currentWorkers)

				if addCount > 10 {
					addCount = 10
				}

				for i := 0; i < addCount && g.workerCount.Load() < maxWorkers; i++ {
					g.addWorker()
				}

				if addCount > 0 {
					g.metrics.SetWorkerCount(int64(g.workerCount.Load()))
					threadStats.totalThreadsCreated += int64(addCount)
					if g.workerCount.Load() > threadStats.peakThreadCount {
						threadStats.peakThreadCount = g.workerCount.Load()
					}
				}
			} else if avgLoad < removeThreshold && currentWorkers > minWorkers {
				removeCount := int(float64(currentWorkers-minWorkers) * 0.2)
				if removeCount < 1 {
					removeCount = 1
				}
				if removeCount > 5 {
					removeCount = 5
				}

				for i := 0; i < removeCount && g.workerCount.Load() > minWorkers; i++ {
					g.removeWorker()
				}

				g.metrics.SetWorkerCount(int64(g.workerCount.Load()))
				threadStats.totalThreadsRemoved += int64(removeCount)
			}

			// 每10秒输出一次线程池状态
			if time.Since(lastLogTime) >= 10*time.Second {
				// 计算平均线程数
				var totalWorkers int64
				for _, count := range historyWorkerCount {
					totalWorkers += int64(count)
				}
				if len(historyWorkerCount) > 0 {
					threadStats.avgThreadCount = float64(totalWorkers) / float64(len(historyWorkerCount))
				}

				tlog.Info("线程池状态",
					"currentWorkers", currentWorkers,
					"queueLength", queueLength,
					"activeConnections", activeConnections,
					"averageProcessingTime", averageProcessingTime,
					"avgLoad", avgLoad,
					"peakThreadCount", threadStats.peakThreadCount,
					"avgThreadCount", threadStats.avgThreadCount,
					"totalThreadsCreated", threadStats.totalThreadsCreated,
					"totalThreadsRemoved", threadStats.totalThreadsRemoved,
				)

				// 更新上次输出时间
				lastLogTime = time.Now()
			}
		}
	}
}

// calculateEMA 计算指数移动平均值
func (g *Gateway) calculateEMA(values []float64, alpha float64) float64 {
	if len(values) == 0 {
		return 0
	}

	ema := values[0]
	for i := 1; i < len(values); i++ {
		ema = alpha*values[i] + (1-alpha)*ema
	}
	return ema
}

// calculateTrend 计算趋势
func (g *Gateway) calculateTrend(values interface{}) int {
	switch v := values.(type) {
	case []int:
		if len(v) < 3 {
			return 0
		}

		// 计算最近3个值的趋势
		recent := v[len(v)-3:]
		if recent[2] > recent[1] && recent[1] > recent[0] {
			return 1 // 上升趋势
		} else if recent[2] < recent[1] && recent[1] < recent[0] {
			return -1 // 下降趋势
		}
	case []float64:
		if len(v) < 3 {
			return 0
		}

		// 计算最近3个值的趋势
		recent := v[len(v)-3:]
		if recent[2] > recent[1] && recent[1] > recent[0] {
			return 1 // 上升趋势
		} else if recent[2] < recent[1] && recent[1] < recent[0] {
			return -1 // 下降趋势
		}
	}
	return 0 // 无明显趋势
}

// wsHeartbeatChecker WebSocket心跳检查器
func (g *Gateway) wsHeartbeatChecker() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			// 检查所有WebSocket连接
			g.checkWebSocketConnections()
		}
	}
}

// checkWebSocketConnections 检查WebSocket连接
func (g *Gateway) checkWebSocketConnections() {
	// 收集所有WebSocket连接
	var connections []*WebSocketConnection
	g.wsConnections.Range(func(key, value interface{}) bool {
		if conn, ok := key.(*WebSocketConnection); ok {
			connections = append(connections, conn)
		}
		return true
	})

	// 检查每个连接的心跳时间
	for _, conn := range connections {
		if time.Since(conn.LastPingTime) > 60*time.Second {
			// 连接超时，关闭连接
			tlog.Warn("WebSocket连接超时，关闭连接", "connectionID", conn.ConnectionID)
			// 关闭连接
			if conn.Conn != nil {
				conn.Conn.Close()
			}
			// 从连接管理器中移除
			if conn.ConnectionID != "" {
				g.connectionManager.RemoveConnection(conn.ConnectionID)
			}
			// 从WebSocket连接集合中移除
			g.wsConnections.Delete(conn)
			// 归还连接对象到对象池
			wsConnectionPool.Put(conn)
		}
	}
}

// configWatcher 配置热加载监听器
func (g *Gateway) configWatcher() {
	// 检查配置文件是否存在
	if _, err := os.Stat(g.configPath); os.IsNotExist(err) {
		// 配置文件不存在，尝试其他路径
		altPaths := []string{"../config/config.yaml", "../../config/config.yaml"}
		found := false
		for _, path := range altPaths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				g.configPath = path
				found = true
				break
			}
		}
		if !found {
			// 配置文件不存在，使用默认配置
			tlog.Info("配置文件不存在，使用默认配置")
			return
		}
	}

	// 监控配置文件变化
	fileInfo, err := os.Stat(g.configPath)
	if err != nil {
		tlog.Error("获取配置文件信息失败", "error", err)
		return
	}

	lastModTime := fileInfo.ModTime()

	for {
		select {
		case <-g.stopChan:
			return
		default:
			// 检查配置文件是否被修改
			fileInfo, err := os.Stat(g.configPath)
			if err != nil {
				tlog.Error("获取配置文件信息失败", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}

			if fileInfo.ModTime() != lastModTime {
				// 配置文件已修改，重新加载配置
				tlog.Info("配置文件已修改，重新加载配置")
				lastModTime = fileInfo.ModTime()

				// 加载新配置
				newCfg, err := config.LoadConfig()
				if err != nil {
					tlog.Error("加载配置文件失败", "error", err)
					time.Sleep(5 * time.Second)
					continue
				}

				// 发送配置更新通知
				g.configUpdateChan <- newCfg
			}

			time.Sleep(5 * time.Second)
		}
	}
}

// handleConfigUpdate 处理配置更新
func (g *Gateway) handleConfigUpdate(newCfg *config.Config) {
	// 更新认证路由
	authRoutes := make(map[string]bool)
	for _, route := range newCfg.Security.AuthRoutes {
		authRoutes[route] = true
	}

	// 使用atomic.Value更新配置（无锁操作）
	g.cfg.Store(newCfg)
	g.authSecret.Store(newCfg.Security.AuthSecret)
	g.authRoutes.Store(authRoutes)
	g.minWorkers.Store(int32(newCfg.WorkerPool.MinWorkers))
	g.maxWorkers.Store(int32(newCfg.WorkerPool.MaxWorkers))
	g.workerQueueSize.Store(int32(newCfg.WorkerPool.QueueSizeThreshold))

	// 更新速率限制器配置
	g.rateLimiter = NewRateLimiter(newCfg.RateLimiter.Rate, newCfg.RateLimiter.Window)

	// 更新白名单和黑名单
	if g.whitelistBlacklist != nil {
		// 重新初始化白名单和黑名单
		g.whitelistBlacklist = NewWhitelistBlacklist()
	}

	// 更新监控指标阈值
	g.metrics.SetActiveConnectionsThreshold(newCfg.Alerts.ActiveConnectionsThreshold)
	g.metrics.SetFailedMessagesThreshold(newCfg.Alerts.FailedMessagesThreshold)
	g.metrics.SetProcessingTimeThreshold(newCfg.Alerts.ProcessingTimeThreshold)
	g.metrics.SetRedisErrorsThreshold(newCfg.Alerts.RedisErrorsThreshold)
	g.metrics.SetQueueLengthThreshold(newCfg.Alerts.QueueLengthThreshold)

	tlog.Info("配置更新完成")
}

// messageWorker 消息处理工作线程
func (g *Gateway) messageWorker() {
	defer func() {
		g.workerPool.Done()
		// 使用原子操作减少工作线程数
		g.workerCount.Add(-1)
	}()

	for {
		select {
		case <-g.stopChan:
			return
		case <-g.workerStopChan:
			return
		case msg := <-g.messagePool:
			// 处理消息
			g.handleMessage(msg)
		}
	}
}

// registerDefaultRoutes 注册默认路由
func (g *Gateway) registerDefaultRoutes() {
	// 注册ping路由
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("pong", map[string]string{
			"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
		}))
	})

	// 注册getConnections路由
	g.routeManager.RegisterRoute("getConnections", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("connections", map[string]string{
			"count": cast.ToString(g.connectionManager.GetConnectionCount()),
		}))
	})

	// 注册broadcast路由
	g.routeManager.RegisterRoute("broadcast", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			if _, ok := payloadMap["message"]; ok {
				g.Broadcast(payloadMap["message"])
				callback(NewResponseMessage("broadcastResult", map[string]string{
					"success": "true",
				}))
			} else {
				callback(NewResponseMessage("broadcastResult", map[string]string{
					"success": "false",
					"error":   "No message provided",
				}))
			}
		} else {
			callback(NewResponseMessage("broadcastResult", map[string]string{
				"success": "false",
				"error":   "Invalid payload format",
			}))
		}
	})

	// 注册健康检查路由
	g.routeManager.RegisterRoute("health", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("health", map[string]string{
			"status":            "healthy",
			"timestamp":         cast.ToString(time.Now().UnixMilli()),
			"activeConnections": cast.ToString(g.connectionManager.GetConnectionCount()),
			"totalConnections":  cast.ToString(g.metrics.GetConnectionsTotal()),
			"messagesReceived":  cast.ToString(g.metrics.GetMessagesReceived()),
			"messagesProcessed": cast.ToString(g.metrics.GetMessagesProcessed()),
			"messagesFailed":    cast.ToString(g.metrics.GetMessagesFailed()),
		}))
	})

	// 注册API文档路由
	g.routeManager.RegisterRoute("api-docs", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("api-docs", map[string]string{
			"version": "1.0.0",
			"routes":  "ping,getConnections,broadcast,health,api-docs,version",
		}))
	})

	// 注册版本路由
	g.registerVersionRoute()
	g.registerPingRoute()

	g.routeManager.RegisterLogicRoute("ping")
	g.routeManager.RegisterLogicRoute("test")

	// 注册白名单管理路由
	g.routeManager.RegisterRoute("addWhitelist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			ip, ok := payloadMap["ip"]
			if !ok {
				callback(NewResponseMessage("whitelistResult", map[string]string{
					"success": "false",
					"message": "Missing or invalid IP",
				}))
				return
			}

			err := g.whitelistBlacklist.AddToWhitelist(ip)
			if err != nil {
				callback(NewResponseMessage("whitelistResult", map[string]string{
					"success": "false",
					"message": err.Error(),
				}))
				return
			}

			callback(NewResponseMessage("whitelistResult", map[string]string{
				"success": "true",
				"message": "IP added to whitelist",
			}))
		} else {
			callback(NewResponseMessage("whitelistResult", map[string]string{
				"success": "false",
				"message": "Invalid payload format",
			}))
		}
	})

	g.routeManager.RegisterRoute("removeWhitelist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			ip, ok := payloadMap["ip"]
			if !ok {
				callback(NewResponseMessage("whitelistResult", map[string]string{
					"success": "false",
					"message": "Missing or invalid IP",
				}))
				return
			}

			err := g.whitelistBlacklist.RemoveFromWhitelist(ip)
			if err != nil {
				callback(NewResponseMessage("whitelistResult", map[string]string{
					"success": "false",
					"message": err.Error(),
				}))
				return
			}

			callback(NewResponseMessage("whitelistResult", map[string]string{
				"success": "true",
				"message": "IP removed from whitelist",
			}))
		} else {
			callback(NewResponseMessage("whitelistResult", map[string]string{
				"success": "false",
				"message": "Invalid payload format",
			}))
		}
	})

	g.routeManager.RegisterRoute("getWhitelist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		whitelist := g.whitelistBlacklist.GetWhitelist()
		// 将切片转换为逗号分隔的字符串
		whitelistStr := ""
		for i, ip := range whitelist {
			if i > 0 {
				whitelistStr += ","
			}
			whitelistStr += ip
		}
		callback(NewResponseMessage("whitelist", map[string]string{
			"whitelist": whitelistStr,
		}))
	})

	// 注册黑名单管理路由
	g.routeManager.RegisterRoute("addBlacklist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			ip, ok := payloadMap["ip"]
			if !ok {
				callback(NewResponseMessage("blacklistResult", map[string]string{
					"success": "false",
					"message": "Missing or invalid IP",
				}))
				return
			}

			err := g.whitelistBlacklist.AddToBlacklist(ip)
			if err != nil {
				callback(NewResponseMessage("blacklistResult", map[string]string{
					"success": "false",
					"message": err.Error(),
				}))
				return
			}

			callback(NewResponseMessage("blacklistResult", map[string]string{
				"success": "true",
				"message": "IP added to blacklist",
			}))
		} else {
			callback(NewResponseMessage("blacklistResult", map[string]string{
				"success": "false",
				"message": "Invalid payload format",
			}))
		}
	})

	g.routeManager.RegisterRoute("removeBlacklist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			ip, ok := payloadMap["ip"]
			if !ok {
				callback(NewResponseMessage("blacklistResult", map[string]string{
					"success": "false",
					"message": "Missing or invalid IP",
				}))
				return
			}

			err := g.whitelistBlacklist.RemoveFromBlacklist(ip)
			if err != nil {
				callback(NewResponseMessage("blacklistResult", map[string]string{
					"success": "false",
					"message": err.Error(),
				}))
				return
			}

			callback(NewResponseMessage("blacklistResult", map[string]string{
				"success": "true",
				"message": "IP removed from blacklist",
			}))
		} else {
			callback(NewResponseMessage("blacklistResult", map[string]string{
				"success": "false",
				"message": "Invalid payload format",
			}))
		}
	})

	g.routeManager.RegisterRoute("getBlacklist", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		blacklist := g.whitelistBlacklist.GetBlacklist()
		// 将切片转换为逗号分隔的字符串
		blacklistStr := ""
		for i, ip := range blacklist {
			if i > 0 {
				blacklistStr += ","
			}
			blacklistStr += ip
		}
		callback(NewResponseMessage("blacklist", map[string]string{
			"blacklist": blacklistStr,
		}))
	})
}

// connContextPool ConnContext 对象池
// 功能: 复用 ConnContext 对象，减少内存分配
var connContextPool = sync.Pool{
	New: func() interface{} {
		return &ConnContext{
			ConnectionID: "",
			FrameBuf:     nil,
		}
	},
}

// GetConnContext 从对象池获取 ConnContext 对象
// 功能: 从对象池获取一个 ConnContext 对象，并清空其内容
// 返回值:
//
//	*ConnContext: ConnContext 对象
func GetConnContext() *ConnContext {
	ctx := connContextPool.Get().(*ConnContext)
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	return ctx
}

func PutConnContext(ctx *ConnContext) {
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	connContextPool.Put(ctx)
}

// ConnContext 连接上下文结构
type ConnContext struct {
	ConnectionID string
	FrameBuf     []byte
	FrameOff     int
}

// OnOpen 连接打开时的回调
// 参数:
//
//	c: 网络连接
//
// 返回值:
//
//	[]byte: 输出数据
//	gnet.Action: 操作类型
func (g *Gateway) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	// 处理新连接
	// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
	tempUserUUID := "temp_" + generateConnectionID()
	connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
	// 从对象池获取连接上下文
	connCtx := GetConnContext()
	connCtx.ConnectionID = connectionID
	// 设置连接上下文
	c.SetContext(connCtx)

	// 收集连接指标
	g.metrics.IncConnectionsTotal()
	g.metrics.IncConnectionsActive()

	// 输出调试日志
	tlog.Debug("新的连接", "connectionID", connectionID, "userUUID", tempUserUUID)

	return
}

// OnClose 连接关闭时的回调
// 参数:
//
//	c: 网络连接
//	err: 错误信息
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	// 处理连接关闭
	var connectionID string
	connCtx := c.Context()

	if connCtx != nil {
		if ctx, ok := connCtx.(*ConnContext); ok {
			connectionID = ctx.ConnectionID
			// 归还ConnContext对象到对象池
			PutConnContext(ctx)
		} else if wsConn, ok := connCtx.(*WebSocketConnection); ok {
			connectionID = wsConn.ConnectionID
			// 归还WebSocket连接对象到对象池
			wsConnectionPool.Put(wsConn)
		} else if id, ok := connCtx.(string); ok {
			connectionID = id
		}
	}

	if connectionID != "" {
		g.connectionManager.RemoveConnection(connectionID)
		// 移除客户端版本映射
		g.versionNegotiation.RemoveClientVersion(connectionID)

		// 收集连接指标
		g.metrics.DecConnectionsActive()

		// 输出调试日志
		tlog.Debug("连接关闭", "connectionID", connectionID, "error", err)
	}

	return
}

// OnTraffic 收到数据时的回调
// 参数:
//
//	c: 网络连接
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) OnTraffic(c gnet.Conn) (action gnet.Action) {
	if g.fastPath != nil {
		data, err := c.Peek(-1)
		if err == nil && len(data) >= 10 {
			connCtx := c.Context()
			if connCtx != nil {
				if ctx, ok := connCtx.(*ConnContext); ok && len(ctx.FrameBuf) <= ctx.FrameOff {
					testPayloadLen := g.fastPath.testPayloadLen
					testFrame := g.fastPath.testFrame
					dataLen := len(data)
					testCount := 0
					consumed := 0

					for consumed+4 < dataLen {
						fl := binary.BigEndian.Uint32(data[consumed : consumed+4])
						if fl != testPayloadLen {
							break
						}
						totalLen := 4 + int(fl)
						if consumed+totalLen > dataLen {
							break
						}
						fd := data[consumed+4 : consumed+totalLen]
						if len(fd) >= 6 && fd[0] == testRoutePattern[0] && fd[1] == testRoutePattern[1] &&
							fd[2] == testRoutePattern[2] && fd[3] == testRoutePattern[3] &&
							fd[4] == testRoutePattern[4] && fd[5] == testRoutePattern[5] {
							testCount++
							consumed += totalLen
							continue
						}
						break
					}

					if testCount > 0 {
						atomic.AddInt64(&fastPathTotal, int64(testCount))
						c.Discard(consumed)
						ctx.FrameBuf = nil
						ctx.FrameOff = 0

						if bf := g.fastPath.getBatchFrame(testCount); bf != nil {
							c.Write(bf)
						} else if testCount == 1 {
							c.Write(testFrame)
						} else {
							bp := batchBufPool.Get().(*[]byte)
							b := (*bp)[:0]
							for i := 0; i < testCount; i++ {
								b = append(b, testFrame...)
							}
							c.Write(b)
							*bp = b
							batchBufPool.Put(bp)
						}
						return
					}
				}
			}
		}
	}

	return g.handleNormalTraffic(c)
}

func (g *Gateway) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnTraffic panic recovered", "error", cast.ToString(r))
			action = gnet.Close
		}
	}()

	var clientIP string
	switch addr := c.RemoteAddr().(type) {
	case *net.TCPAddr:
		clientIP = addr.IP.String()
	case *net.UDPAddr:
		clientIP = addr.IP.String()
	default:
		tlog.Warn("未知的地址类型", "addr", c.RemoteAddr())
		return gnet.Close
	}

	if g.whitelistBlacklist != nil && g.whitelistBlacklist.IsInBlacklist(clientIP) {
		tlog.Warn("请求被拒绝，IP在黑名单中", "clientIP", clientIP)
		writeFrame(c, mustMarshalError(NewErrorMessage("error", "IP address is blacklisted", "", "")))
		return gnet.Close
	}

	if !g.rateLimiter.Allow("ip", clientIP) {
		tlog.Warn("请求被限流（IP维度）", "clientIP", clientIP)
		writeFrame(c, mustMarshalError(NewErrorMessage("error", "Rate limit exceeded (IP dimension)", "", "")))
		return gnet.Close
	}

	data, err := c.Next(-1)
	if err != nil {
		tlog.Error("读取数据失败", "error", err)
		return gnet.Close
	}

	atomic.AddInt64(&fastPathTotal, 1)

	g.metrics.AddBytesReceived(int64(len(data)))

	port := c.LocalAddr().String()
	for i := len(port) - 1; i >= 0; i-- {
		if port[i] == ':' {
			port = port[i+1:]
			break
		}
	}
	transportType := g.transportType[port]

	if transportType == "websocket" {
		return g.HandleWebSocket(c, data)
	}

	connCtx := c.Context()
	if connCtx == nil {
		return gnet.Close
	}
	ctx, ok := connCtx.(*ConnContext)
	if !ok {
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	for len(ctx.FrameBuf) >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[:4])
		if frameLen == 0 || frameLen > 4*1024*1024 {
			ctx.FrameBuf = nil
			ctx.FrameOff = 0
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if len(ctx.FrameBuf) < totalLen {
			return
		}

		frameData := ctx.FrameBuf[4:totalLen]
		frameCopy := make([]byte, len(frameData))
		copy(frameCopy, frameData)

		if len(ctx.FrameBuf) > totalLen {
			ctx.FrameBuf = ctx.FrameBuf[totalLen:]
		} else {
			ctx.FrameBuf = nil
		}
		ctx.FrameOff = 0

		if ret := g.handleTCPRequest(c, frameCopy); ret == gnet.Close {
			return gnet.Close
		}
	}

	return
}

// containsBytes 检查字节数组是否包含另一个字节数组
// 参数:
//
//	data: 数据
//	sub: 子字节数组
//
// 返回值:
//
//	bool: 是否包含
func containsBytes(data, sub []byte) bool {
	for i := 0; i <= len(data)-len(sub); i++ {
		if equal(data[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// trimSpace 去除字符串前后空白
// 参数:
//
//	s: 字符串
//
// 返回值:
//
//	string: 处理后的字符串
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

// handleTCPRequest 处理TCP请求
// 参数:
//
//	c: 网络连接
//	data: 数据
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) isLogicConnected() bool {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return true
	}
	return g.logicClient.IsConnected()
}

func (g *Gateway) isLogicRoute(route string) bool {
	if g.isLogicConnected() {
		return true
	}
	return route == "test" || route == "ping"
}

func (g *Gateway) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		writeErrorFrame(c, NewErrorMessage("error", "Empty data", "", ""))
		return
	}

	message := GetProtobufMessage()
	defer PutProtobufMessage(message)

	if err := proto.Unmarshal(data, message); err != nil {
		writeErrorFrame(c, NewErrorMessage("error", "Invalid message format", err.Error(), string(data)))
		return
	}

	logicRoutesIntegrity := map[string]bool{"test": true, "ping": true, "handshake": true}
	if !logicRoutesIntegrity[message.Route] && !g.isLogicRoute(message.Route) {
		if err := g.messageIntegrity.ProcessMessage(message); err != nil {
			writeErrorFrame(c, NewErrorMessage("error", "Message integrity error", err.Error(), ""))
			return
		}
	}

	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*ConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		c.SetContext(&ConnContext{
			ConnectionID: connectionID,
			FrameBuf:     nil,
		})
		tlog.Info("生成新的连接ID", "connectionID", connectionID, "userUUID", tempUserUUID)
	}

	if message.Route == "handshake" {
		return g.handleHandshake(c, connectionID, message)
	}

	if _, exists := g.versionNegotiation.GetClientVersion(connectionID); !exists && message.Route != "test" {
		writeErrorFrame(c, NewErrorMessage("error", "Handshake required", "Protocol version negotiation is required", ""))
		return
	}

	userUUID := message.UserUuid

	if userUUID != "" {
		g.connectionManager.UpdateUserConnection(connectionID, "", userUUID)
		tlog.Debug("收到用户UUID", "connectionID", connectionID, "userUUID", userUUID)
	}

	route := message.Route
	if route == "" {
		tlog.Warn("消息格式错误，缺少route字段", "message", message)
		writeErrorFrame(c, NewErrorMessage("error", "Invalid message format: missing route", "", ""))
		return
	}

	payload := message.Payload
	if payload == nil {
		payload = make(map[string]string)
	} else {
		copied := make(map[string]string, len(payload))
		for k, v := range payload {
			copied[k] = v
		}
		payload = copied
	}

	msg := GetMessage()
	msg.ConnectionID = connectionID
	msg.Route = route
	msg.Payload = payload
	msg.Conn = c

	select {
	case g.messagePool <- msg:
		// 消息已加入队列
		tlog.Debug("消息已加入队列", "connectionID", connectionID, "route", route)
	default:
		// 消息队列已满，直接处理
		tlog.Warn("消息队列已满，直接处理", "connectionID", connectionID, "route", route)
		g.handleMessage(msg)
	}

	return
}

// handleHandshake 处理握手消息
// 参数:
//
//	c: 网络连接
//	connectionID: 连接ID
//	message: 消息
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) handleHandshake(c gnet.Conn, connectionID string, message *protobuf.Message) gnet.Action {
	handshakeDataStr := message.Payload["handshake_data"]
	var handshakeBytes []byte

	decoded, err := base64.StdEncoding.DecodeString(handshakeDataStr)
	if err == nil {
		handshakeBytes = decoded
	} else {
		handshakeBytes = []byte(handshakeDataStr)
	}

	handshake := &protobuf.Handshake{}
	if err := proto.Unmarshal(handshakeBytes, handshake); err != nil {
		writeErrorFrame(c, NewErrorMessage("error", "Invalid handshake data", err.Error(), ""))
		return gnet.None
	}

	negotiatedVersion, err := g.versionNegotiation.ProcessHandshake(connectionID, handshake)
	if err != nil {
		writeErrorFrame(c, NewErrorMessage("error", "Handshake failed", err.Error(), ""))
		return gnet.None
	}

	response := g.versionNegotiation.GenerateHandshakeResponse(negotiatedVersion)
	g.messageIntegrity.PrepareMessage(response)
	writeMsgFrame(c, response)

	if serverID := message.Payload["serverId"]; serverID != "" {
		g.connectionManager.SetConnectionServerID(connectionID, serverID)
		g.connectionManager.AddUserToGroup("server:"+serverID, connectionID)
		tlog.Info("connection auto-joined server group", "connectionID", connectionID, "serverID", serverID, "groupID", "server:"+serverID)
	}

	return gnet.None
}

// splitBytes 分割字节数组
// 参数:
//
//	data: 数据
//	sep: 分隔符
//
// 返回值:
//
//	[][]byte: 分割后的字节数组
func writeFrame(c gnet.Conn, data []byte) {
	headerPtr := frameHeaderPool.Get().(*[]byte)
	binary.BigEndian.PutUint32(*headerPtr, uint32(len(data)))
	c.Writev([][]byte{*headerPtr, data})
	frameHeaderPool.Put(headerPtr)
}

func mustMarshalError(errMsg *protobuf.ErrorResponse) []byte {
	data, _ := proto.Marshal(errMsg)
	return data
}

func writeErrorFrame(c gnet.Conn, errMsg *protobuf.ErrorResponse) {
	writeFrame(c, mustMarshalError(errMsg))
}

func writeMsgFrame(c gnet.Conn, msg *protobuf.Message) {
	data, _ := proto.Marshal(msg)
	writeFrame(c, data)
}

func splitBytes(data []byte, sep []byte) [][]byte {
	var result [][]byte
	start := 0
	for i := 0; i <= len(data)-len(sep); i++ {
		if equal(data[i:i+len(sep)], sep) {
			result = append(result, data[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	if start < len(data) {
		result = append(result, data[start:])
	}
	return result
}

// equal 比较两个字节数组是否相等
// 参数:
//
//	a: 字节数组a
//	b: 字节数组b
//
// 返回值:
//
//	bool: 是否相等
func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// indexOf 查找字符在字符串中的位置
// 参数:
//
//	s: 字符串
//	c: 字符
//
// 返回值:
//
//	int: 位置
func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// indexOfBytes 查找字节数组在另一个字节数组中的位置
// 参数:
//
//	data: 数据
//	sep: 分隔符
//
// 返回值:
//
//	int: 位置
func indexOfBytes(data, sep []byte) int {
	for i := 0; i <= len(data)-len(sep); i++ {
		if equal(data[i:i+len(sep)], sep) {
			return i
		}
	}
	return -1
}

// trimPrefix 移除字符串开头的指定字符
// 参数:
//
//	s: 字符串
//	c: 字符
//
// 返回值:
//
//	string: 处理后的字符串
func trimPrefix(s string, c byte) string {
	for len(s) > 0 && s[0] == c {
		s = s[1:]
	}
	return s
}

// replaceAll 替换字符串中的所有指定字符
// 参数:
//
//	s: 字符串
//	old: 旧字符
//	new: 新字符
//
// 返回值:
//
//	string: 处理后的字符串
func replaceAll(s string, old, new byte) string {
	// 首先检查是否需要替换
	needReplace := false
	for i := 0; i < len(s); i++ {
		if s[i] == old {
			needReplace = true
			break
		}
	}
	if !needReplace {
		return s
	}

	// 需要替换时再分配内存
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == old {
			result[i] = new
		} else {
			result[i] = s[i]
		}
	}
	return string(result)
}

// OnBoot 服务器启动时的回调
// 参数:
//
//	engine: gnet引擎
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	qpsFile, _ := os.OpenFile("qps_counter.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	go func() {
		for {
			time.Sleep(5 * time.Second)
			cur := atomic.LoadInt64(&fastPathTotal)
			last := atomic.LoadInt64(&fastPathLast)
			atomic.StoreInt64(&fastPathLast, cur)
			qps := int64(0)
			if last > 0 {
				qps = (cur - last) / 5
			}
			msg := fmt.Sprintf("[QPS] total=%d last=%d qps=%d\n", cur, last, qps)
			if qpsFile != nil {
				qpsFile.WriteString(msg)
				qpsFile.Sync()
			}
		}
	}()
	return
}

// SetTLSConfig 设置TLS配置
// 参数:
//
//	config: TLS配置
func (g *Gateway) SetTLSConfig(config *tls.Config) {
	g.tlsConfig = config
}

// GetTLSConfig 获取TLS配置
// 返回值:
//
//	*tls.Config: TLS配置
func (g *Gateway) GetTLSConfig() *tls.Config {
	return g.tlsConfig
}

// GetVersion 获取网关版本
// 返回值:
//
//	string: 版本号
func (g *Gateway) GetVersion() string {
	return "1.0.0"
}

func (g *Gateway) GetRouteManager() *RouteManager {
	return g.routeManager
}

func (g *Gateway) GetConnectionManager() *ConnectionManager {
	return g.connectionManager
}

// registerVersionRoute 注册版本路由
func (g *Gateway) registerVersionRoute() {
	g.routeManager.RegisterRoute("version", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("version", map[string]string{
			"version":   g.GetVersion(),
			"clusterID": g.clusterID,
			"isLeader":  fmt.Sprintf("%t", g.isLeader),
		}))
	})
}

// registerPingRoute 注册ping路由
func (g *Gateway) registerPingRoute() {
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("ping", map[string]interface{}{
			"message":   "Pong",
			"timestamp": time.Now().Unix(),
		}))
	})
}

// OnTick 定时回调
// 返回值:
//
//	time.Duration: 延迟时间
//	gnet.Action: 操作类型
func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	// 定期记录指标
	g.metrics.LogMetrics()

	// 更新消息队列长度指标
	g.metrics.SetQueueLength(int64(len(g.messagePool)))

	return 1 * time.Second, gnet.None
}

// OnShutdown 服务器关闭时的回调
// 参数:
//
//	engine: gnet引擎
func (g *Gateway) OnShutdown(engine gnet.Engine) {
	// 服务器关闭时的清理工作
	g.Close()
}

// handleMessage 处理消息
// 参数:
//
//	msg: 消息
func (g *Gateway) handleMessage(msg *Message) {
	// 收集消息指标
	g.metrics.IncMessagesReceived()

	// 记录处理开始时间
	start := time.Now()

	// 生成或获取追踪ID
	traceID := GenerateTraceID()
	if traceIDValue, ok := msg.Payload["trace_id"]; ok {
		traceID = traceIDValue
	}

	// 开始追踪 span
	span := g.tracer.StartSpan(traceID, "handle_message", "")
	g.tracer.AddAttribute(span, "connection_id", msg.ConnectionID)
	g.tracer.AddAttribute(span, "route", msg.Route)

	// 快速路径：处理 ping 等简单路由
	if msg.Route == "ping" && !g.logicClient.IsConnected() {
		// 直接处理 ping 路由，避免复杂的处理流程
		response := NewResponseMessage("pong", map[string]string{
			"timestamp": cast.ToString(time.Now().UnixMilli()),
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		g.tracer.EndSpan(span)

		PutMessage(msg)
		return
	} else if msg.Route == "version" {
		response := NewResponseMessage("version", map[string]string{
			"version":   g.GetVersion(),
			"clusterID": g.clusterID,
			"isLeader":  fmt.Sprintf("%t", g.isLeader),
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		g.tracer.EndSpan(span)

		PutMessage(msg)
		return
	} else if msg.Route == "getConnections" {
		response := NewResponseMessage("connections", map[string]string{
			"count": cast.ToString(g.connectionManager.GetConnectionCount()),
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		g.tracer.EndSpan(span)

		PutMessage(msg)
		return
	} else if msg.Route == "broadcast" {
		response := NewResponseMessage("broadcastResult", map[string]string{
			"success": "true",
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		g.tracer.EndSpan(span)

		PutMessage(msg)
		return
	} else if msg.Route == "health" {
		response := NewResponseMessage("health", map[string]string{
			"status":            "healthy",
			"timestamp":         cast.ToString(time.Now().UnixMilli()),
			"activeConnections": cast.ToString(g.connectionManager.GetConnectionCount()),
			"totalConnections":  cast.ToString(g.metrics.GetConnectionsTotal()),
			"messagesReceived":  cast.ToString(g.metrics.GetMessagesReceived()),
			"messagesProcessed": cast.ToString(g.metrics.GetMessagesProcessed()),
			"messagesFailed":    cast.ToString(g.metrics.GetMessagesFailed()),
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		g.tracer.EndSpan(span)

		PutMessage(msg)
		return
	} else if msg.Route == "api-docs" {
		response := NewResponseMessage("api-docs", map[string]string{
			"version": "1.0.0",
			"routes":  "ping,getConnections,broadcast,health,api-docs,version",
		})
		responseData, _ := proto.Marshal(response)
		writeFrame(msg.Conn, responseData)

		// 结束 span
		g.tracer.EndSpan(span)

		// 归还消息对象到对象池
		PutMessage(msg)
		return
	}

	// 获取熔断器
	breaker := g.circuitBreakerManager.GetCircuitBreaker(msg.Route, 5, 3, 30*time.Second)

	// 检查熔断器是否允许请求通过
	if !breaker.Allow() {
		// 记录事件
		g.tracer.AddEvent(span, "circuit_breaker_open", map[string]string{
			"route": msg.Route,
		})

		// 熔断器打开，拒绝请求
		errorMsg := NewErrorMessage("error", "Service temporarily unavailable", "Circuit breaker is open", "")
		responseData, _ := proto.Marshal(errorMsg)
		writeFrame(msg.Conn, responseData)

		// 结束 span
		g.tracer.EndSpan(span)

		// 归还消息对象到对象池
		PutMessage(msg)
		return
	}

	// 处理路由
	defer func() {
		if r := recover(); r != nil {
			// 记录事件
			g.tracer.AddEvent(span, "panic", map[string]string{
				"error": fmt.Sprintf("%v", r),
			})

			// 记录失败
			breaker.RecordFailure()
			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("处理消息异常",
				"connectionID", msg.ConnectionID,
				"route", msg.Route,
				"error", r)
		}

		// 结束 span
		g.tracer.EndSpan(span)

		// 归还消息对象到对象池
		PutMessage(msg)
	}()

	// 检查路由是否存在
	hasRoute := g.routeManager.HasRoute(msg.Route)
	if !hasRoute {
		// 记录事件
		g.tracer.AddEvent(span, "route_not_found", map[string]string{
			"route": msg.Route,
		})

		// 记录失败
		breaker.RecordFailure()
		// 路由不存在，发送错误响应
		errorMsg := NewErrorMessage("error", "Route not found", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		writeFrame(msg.Conn, responseData)
		return
	}

	// 安全处理：清理和验证输入
	if msg.Payload != nil {
		// 清理 payload 中的字符串值，防止 XSS 攻击
		msg.Payload = sanitizePayloadMap(msg.Payload)

		// 验证输入，防止 SQL 注入和其他攻击
		if !validatePayloadMap(msg.Payload) {
			// 记录事件
			g.tracer.AddEvent(span, "invalid_input", map[string]string{
				"route": msg.Route,
			})

			// 记录失败
			breaker.RecordFailure()
			// 输入验证失败，发送错误响应
			errorMsg := NewErrorMessage("error", "Invalid input detected", "", "")
			responseData, _ := proto.Marshal(errorMsg)
			writeFrame(msg.Conn, responseData)
			return
		}
	}

	// 检查是否需要认证
	if g.requiresAuth(msg.Route) {
		// 验证JWT令牌
		token, ok := getTokenFromPayloadMap(msg.Payload)
		if !ok {
			// 记录事件
			g.tracer.AddEvent(span, "missing_token", map[string]string{
				"route": msg.Route,
			})

			// 记录失败
			breaker.RecordFailure()
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Missing token", "", "")
			responseData, _ := proto.Marshal(errorMsg)
			writeFrame(msg.Conn, responseData)
			return
		}

		// 安全获取 authSecret
		authSecretVal := g.authSecret.Load()
		if authSecretVal == nil {
			g.tracer.AddEvent(span, "config_error", map[string]string{
				"route": msg.Route,
				"error": "authSecret is nil",
			})
			breaker.RecordFailure()
			errorMsg := NewErrorMessage("error", "Server configuration error", "", "")
			responseData, _ := proto.Marshal(errorMsg)
			writeFrame(msg.Conn, responseData)
			return
		}
		authSecret, ok := authSecretVal.(string)
		if !ok {
			g.tracer.AddEvent(span, "config_error", map[string]string{
				"route": msg.Route,
				"error": "authSecret type error",
			})
			breaker.RecordFailure()
			errorMsg := NewErrorMessage("error", "Server configuration error", "", "")
			responseData, _ := proto.Marshal(errorMsg)
			writeFrame(msg.Conn, responseData)
			return
		}

		// 验证token
		claims, err := ValidateToken(token, authSecret)
		if err != nil {
			// 记录事件
			g.tracer.AddEvent(span, "invalid_token", map[string]string{
				"route": msg.Route,
				"error": err.Error(),
			})

			// 记录失败
			breaker.RecordFailure()
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Invalid token", err.Error(), "")
			responseData, _ := proto.Marshal(errorMsg)
			writeFrame(msg.Conn, responseData)
			return
		}

		// 将用户信息添加到payload中
		msg.Payload = addUserInfoToPayloadMap(msg.Payload, claims.UserID, claims.Role)

		// 记录事件
		g.tracer.AddEvent(span, "auth_success", map[string]string{
			"user_id": claims.UserID,
			"role":    claims.Role,
		})
	}

	// 记录事件
	g.tracer.AddEvent(span, "route_handler_start", map[string]string{
		"route": msg.Route,
	})

	ctx := map[string]interface{}{
		"trace_id":      traceID,
		"connection_id": msg.ConnectionID,
		"route":         msg.Route,
		"timestamp":     time.Now().UnixMilli(),
		"span":          span,
		"gateway":       g,
	}

	g.routeManager.HandleRoute(msg.ConnectionID, msg.Route, msg.Payload, func(response interface{}) {
		g.tracer.AddEvent(span, "route_handler_end", map[string]string{
			"route": msg.Route,
		})

		if response == nil {
			return
		}

		// 生成Protocol Buffers响应
		var responseData []byte
		var err error

		if protoMsg, ok := response.(*protobuf.Message); ok {
			responseData, err = proto.Marshal(protoMsg)
		} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
			responseData, err = proto.Marshal(errorMsg)
		} else {
			// 转换为Protocol Buffers消息
			protoMsg := NewResponseMessage("response", response.(map[string]string))
			responseData, err = proto.Marshal(protoMsg)
		}
		if err != nil {
			// 记录事件
			g.tracer.AddEvent(span, "marshal_error", map[string]string{
				"error": err.Error(),
			})

			// 记录失败
			breaker.RecordFailure()
			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("序列化响应失败", "error", err)
			return
		}

		// 检查是否是WebSocket连接
		// 从连接上下文获取WebSocket连接
		isWebSocket := false
		if wsConn, ok := msg.Conn.Context().(*WebSocketConnection); ok && atomic.LoadInt32(&wsConn.State) == int32(WSStateOpen) {
			isWebSocket = true
		}

		if isWebSocket {
			// 封装为WebSocket消息
			wsConn := msg.Conn.Context().(*WebSocketConnection)
			if err := g.sendWebSocketMessage(wsConn, ws.OpText, responseData); err != nil {
				// 记录事件
				g.tracer.AddEvent(span, "websocket_send_error", map[string]string{
					"error": err.Error(),
				})

				// 记录失败
				breaker.RecordFailure()
				// 收集处理失败的消息指标
				g.metrics.IncMessagesFailed()
				// 输出错误日志
				tlog.Error("发送WebSocket响应失败",
					"connectionID", msg.ConnectionID,
					"error", err)
				return
			}

			// 记录事件
			g.tracer.AddEvent(span, "websocket_send_success", map[string]string{
				"connectionID": msg.ConnectionID,
			})

			// 记录成功
			breaker.RecordSuccess()
			// 收集处理成功的消息指标
			g.metrics.IncMessagesProcessed()

			// 计算处理时间
			duration := time.Since(start)
			g.metrics.AddProcessingTime(duration)
			return
		}

		writeFrame(msg.Conn, responseData)

		g.tracer.AddEvent(span, "send_success", map[string]string{
			"connectionID": msg.ConnectionID,
		})

		// 记录成功
		breaker.RecordSuccess()
		// 收集处理成功的消息指标
		g.metrics.IncMessagesProcessed()

		// 计算处理时间
		duration := time.Since(start)
		g.metrics.AddProcessingTime(duration)

		// 记录事件
		g.tracer.AddEvent(span, "message_processed", map[string]string{
			"duration": fmt.Sprintf("%v", duration),
			"route":    msg.Route,
		})

	}, ctx)
}

// requiresAuth 检查路由是否需要认证
// 参数:
//
//	route: 路由名称
//
// 返回值:
//
//	bool: 是否需要认证
func (g *Gateway) requiresAuth(route string) bool {
	authRoutesVal := g.authRoutes.Load()
	if authRoutesVal == nil {
		return false
	}
	authRoutes, ok := authRoutesVal.(map[string]bool)
	if !ok {
		return false
	}
	return authRoutes[route]
}

// sanitizeString 清理字符串，防止 XSS 攻击
func sanitizeString(s string) string {
	// 替换 HTML 特殊字符
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	s = strings.ReplaceAll(s, "&", "&amp;")
	return s
}

// sanitizeMap 清理 map 中的字符串值，防止 XSS 攻击
func sanitizeMap(m map[string]interface{}) map[string]interface{} {
	for k, v := range m {
		switch val := v.(type) {
		case string:
			m[k] = sanitizeString(val)
		case map[string]interface{}:
			m[k] = sanitizeMap(val)
		case []interface{}:
			for i, item := range val {
				if str, ok := item.(string); ok {
					val[i] = sanitizeString(str)
				} else if subMap, ok := item.(map[string]interface{}); ok {
					val[i] = sanitizeMap(subMap)
				}
			}
			m[k] = val
		}
	}
	return m
}

// sanitizePayload 清理 payload 中的字符串值，防止 XSS 攻击
func sanitizePayload(payload interface{}) interface{} {
	switch val := payload.(type) {
	case map[string]interface{}:
		return sanitizeMap(val)
	case string:
		return sanitizeString(val)
	case []interface{}:
		for i, item := range val {
			val[i] = sanitizePayload(item)
		}
		return val
	default:
		return val
	}
}

// validateInput 验证输入，防止 SQL 注入和其他攻击
func validateInput(input string) bool {
	// 检查 SQL 注入攻击
	sqlInjectionPatterns := []string{
		"' OR '1'='1",
		"' OR 1=1",
		"UNION SELECT",
		"DROP TABLE",
		"DELETE FROM",
		"INSERT INTO",
		"UPDATE.*SET",
	}

	for _, pattern := range sqlInjectionPatterns {
		if strings.Contains(strings.ToUpper(input), strings.ToUpper(pattern)) {
			return false
		}
	}

	// 检查命令注入攻击
	commandInjectionPatterns := []string{
		";",
		"|",
		"&&",
		"||",
		"`",
		"$(",
	}

	for _, pattern := range commandInjectionPatterns {
		if strings.Contains(input, pattern) {
			return false
		}
	}

	return true
}

// validatePayload 验证 payload 中的输入，防止 SQL 注入和其他攻击
func validatePayload(payload interface{}) bool {
	switch val := payload.(type) {
	case map[string]interface{}:
		for _, v := range val {
			if !validatePayload(v) {
				return false
			}
		}
	case string:
		if !validateInput(val) {
			return false
		}
	case []interface{}:
		for _, item := range val {
			if !validatePayload(item) {
				return false
			}
		}
	}
	return true
}

// getTokenFromPayload 从 payload 中获取 token
func getTokenFromPayload(payload interface{}) (string, bool) {
	if payloadMap, ok := payload.(map[string]interface{}); ok {
		if token, ok := payloadMap["token"].(string); ok {
			return token, true
		}
	}
	return "", false
}

// addUserInfoToPayload 将用户信息添加到 payload 中
func addUserInfoToPayload(payload interface{}, userID string, role string) interface{} {
	if payloadMap, ok := payload.(map[string]interface{}); ok {
		payloadMap["user_id"] = userID
		payloadMap["role"] = role
		return payloadMap
	}
	return payload
}

// sanitizePayloadMap 清理 payload 中的字符串值，防止 XSS 攻击
func sanitizePayloadMap(payload map[string]string) map[string]string {
	for k, v := range payload {
		payload[k] = sanitizeString(v)
	}
	return payload
}

// validatePayloadMap 验证 payload 中的输入，防止 SQL 注入和其他攻击
func validatePayloadMap(payload map[string]string) bool {
	for _, v := range payload {
		if !validateInput(v) {
			return false
		}
	}
	return true
}

// getTokenFromPayloadMap 从 payload 中获取 token
func getTokenFromPayloadMap(payload map[string]string) (string, bool) {
	if token, ok := payload["token"]; ok {
		return token, true
	}
	return "", false
}

// addUserInfoToPayloadMap 将用户信息添加到 payload 中
func addUserInfoToPayloadMap(payload map[string]string, userID string, role string) map[string]string {
	payload["user_id"] = userID
	payload["role"] = role
	return payload
}

// Broadcast 广播消息
func (g *Gateway) Broadcast(message interface{}) {
	g.connectionManager.Broadcast(message)
}

// Close 关闭网关
func (g *Gateway) Close() {
	close(g.stopChan)
	g.workerPool.Wait()

	if g.serviceDiscovery != nil {
		g.serviceDiscovery.Stop()
	}

	if g.logicClientPool != nil {
		g.logicClientPool.Close()
	}

	if g.redisClient != nil {
		g.redisClient.Close()
	}

	g.connectionManager.CloseAllConnections()
	tlog.Info("所有连接已关闭")
}
