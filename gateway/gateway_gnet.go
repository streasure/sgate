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
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/panjf2000/gnet/v2/pkg/logging"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cast"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

type prebuiltResponse struct {
	data []byte
}

type fastPathCache struct {
	testFrame       []byte
	pongFrame       []byte
	testResponseLen uint32
	pongResponseLen uint32
	testFrameLen    uint32
	testPayloadLen  uint32
	batchFrames     map[int][]byte
}

var testRoutePattern = [6]byte{0x1a, 0x04, 0x74, 0x65, 0x73, 0x74}

func buildFastPathCache() *fastPathCache {
	cache := &fastPathCache{}

	testMsg := &protobuf.Message{
		Route:           "testResult",
		Payload:         map[string]string{"success": "true", "message": "Test route works"},
		Timestamp:       0,
		ProtocolVersion: "1.0.0",
	}
	testData, _ := proto.Marshal(testMsg)
	cache.testResponseLen = uint32(len(testData))
	cache.testFrame = make([]byte, 4+len(testData))
	binary.BigEndian.PutUint32(cache.testFrame[:4], cache.testResponseLen)
	copy(cache.testFrame[4:], testData)

	pongMsg := &protobuf.Message{
		Route:           "pong",
		Payload:         map[string]string{"timestamp": "0"},
		Timestamp:       0,
		ProtocolVersion: "1.0.0",
	}
	pongData, _ := proto.Marshal(pongMsg)
	cache.pongResponseLen = uint32(len(pongData))
	cache.pongFrame = make([]byte, 4+len(pongData))
	binary.BigEndian.PutUint32(cache.pongFrame[:4], cache.pongResponseLen)
	copy(cache.pongFrame[4:], pongData)

	cache.testFrameLen = uint32(len(cache.testFrame))

	testReqMsg := &protobuf.Message{
		Route:   "test",
		Payload: map[string]string{"data": "1"},
	}
	testReqData, _ := proto.Marshal(testReqMsg)
	cache.testPayloadLen = uint32(len(testReqData))

	cache.batchFrames = make(map[int][]byte)
	frameLen := len(cache.testFrame)
	for _, count := range []int{1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 16, 20, 24, 32, 48, 64, 96, 128} {
		buf := make([]byte, frameLen*count)
		for i := 0; i < count; i++ {
			copy(buf[i*frameLen:], cache.testFrame)
		}
		cache.batchFrames[count] = buf
	}

	return cache
}

func (f *fastPathCache) getBatchFrame(count int) []byte {
	if bf, ok := f.batchFrames[count]; ok {
		return bf
	}
	return nil
}

var (
	fastPathTotal int64
	fastPathLast  int64
)

func writeFrameFast(c gnet.Conn, data []byte) {
	headerPtr := frameHeaderPool.Get().(*[]byte)
	binary.BigEndian.PutUint32(*headerPtr, uint32(len(data)))
	c.Writev([][]byte{*headerPtr, data})
	frameHeaderPool.Put(headerPtr)
}

func writePrebuiltFrame(c gnet.Conn, prebuilt []byte, frameLen uint32) {
	headerPtr := frameHeaderPool.Get().(*[]byte)
	binary.BigEndian.PutUint32(*headerPtr, frameLen)
	c.Writev([][]byte{*headerPtr, prebuilt})
	frameHeaderPool.Put(headerPtr)
}

var batchBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 8192)
		return &b
	},
}

func extractRouteFast(data []byte) string {
	offset := 0
	for offset < len(data) {
		b := data[offset]
		if b < 0x80 {
			offset++
			fieldNum := int(b >> 3)
			wireType := int(b & 0x7)

			switch wireType {
			case 0:
				for offset < len(data) && data[offset] >= 0x80 {
					offset++
				}
				if offset < len(data) {
					offset++
				}
			case 1:
				offset += 8
			case 2:
				if offset >= len(data) {
					return ""
				}
				l := int(data[offset])
				offset++
				if l >= 0x80 {
					if offset >= len(data) {
						return ""
					}
					l2 := int(data[offset])
					offset++
					l = (l & 0x7F) | (l2 << 7)
				}
				if fieldNum == 3 && l > 0 && l < 256 && offset+l <= len(data) {
					return string(data[offset : offset+l])
				}
				offset += l
			case 5:
				offset += 4
			default:
				return ""
			}
		} else {
			offset++
		}
	}
	return ""
}

func decodeVarint(buf []byte) (uint64, int) {
	var x uint64
	var s uint
	for i := 0; i < len(buf) && i < 10; i++ {
		b := buf[i]
		if b < 0x80 {
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0
}

// GatewayGnet 基于 gnet 的网关实现
type GatewayGnet struct {
	connectionManager      *ConnectionManager         // 连接管理器
	routeManager           *RouteManager              // 路由管理器
	messagePool            chan *Message              // 消息队列
	workerPool             sync.WaitGroup             // 工作池
	stopChan               chan struct{}              // 停止信号通道
	workerStopChan         chan struct{}              // 工作线程停止信号通道
	metrics                *metrics.Metrics           // 指标收集器
	transportType          map[string]string          // 端口到传输类型的映射
	rateLimiter            *RateLimiter               // 速率限制器
	authSecret             atomic.Value               // 认证密钥，使用atomic.Value存储
	authRoutes             atomic.Value               // 需要认证的路由，使用atomic.Value存储
	ctx                    context.Context            // 上下文
	tlsConfig              *tls.Config                // TLS配置
	clusterID              string                     // 集群ID
	isLeader               bool                       // 是否是领导者
	bufferPool             sync.Pool                  // 缓冲区池
	whitelistBlacklist     *WhitelistBlacklist        // 白名单和黑名单管理器
	workerMutex            sync.Mutex                 // 工作池互斥锁
	workerCount            int32                      // 当前工作线程数，使用int32以支持原子操作
	minWorkers             atomic.Int32               // 最小工作线程数，使用atomic.Int32存储
	maxWorkers             atomic.Int32               // 最大工作线程数，使用atomic.Int32存储
	workerQueueSize        atomic.Int32               // 工作队列大小阈值，使用atomic.Int32存储
	cfg                    atomic.Value               // 配置实例，使用atomic.Value存储
	wsConnections          sync.Map                   // 活跃的WebSocket连接
	configPath             string                     // 配置文件路径
	configUpdateChan       chan *config.Config        // 配置更新通道
	cache                  *Cache                     // 缓存管理器
	loadBalancer           *LoadBalancer              // 负载均衡器
	messageIntegrity       *MessageIntegrity          // 消息完整性管理器
	messageACK             *MessageACK                // 消息确认管理器
	compressor             *Compressor                // 压缩管理器
	versionNegotiation     *VersionNegotiation        // 版本协商管理器
	circuitBreakerManager  *CircuitBreakerManager     // 熔断器管理器
	messageQueue           *MessageQueue              // 消息队列管理器
	tracer                 *Tracer                    // 链路追踪器
	logicClient            *LogicClient               // 逻辑服 gRPC 客户端
	logicClientPool        *LogicClientPool           // 逻辑服客户端池
	serviceDiscovery       *ServiceDiscovery          // 服务发现
	redisClient            *redis.Client              // Redis客户端
	resourceCircuitBreaker *CircuitBreaker            // 资源熔断器
	resourceCheckInterval  time.Duration              // 资源检查间隔
	userRateLimitConfig    config.UserRateLimitConfig // 用户维度限流配置
	fastPath               *fastPathCache             // 快速路径缓存
}

// monitorResources 监控系统资源使用情况
func (g *GatewayGnet) monitorResources() {
	ticker := time.NewTicker(g.resourceCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 获取当前配置（安全的类型断言）
			cfgVal := g.cfg.Load()
			if cfgVal == nil {
				continue
			}
			cfg, ok := cfgVal.(*config.Config)
			if !ok {
				continue
			}

			// 检查是否启用资源熔断器
			if !cfg.Resources.EnableResourceCircuitBreaker {
				continue
			}

			// 检查内存使用情况
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)

			// 计算内存占用百分比
			totalMemory := float64(runtime.MemStats{}.Sys) // 系统总内存
			if totalMemory == 0 {
				totalMemory = 1 // 避免除以零
			}
			memoryUsage := float64(memStats.Alloc) / totalMemory * 100

			// 检查 CPU 使用情况
			// 这里简化处理，实际应该使用更准确的 CPU 使用率计算方法
			// 暂时使用固定值模拟，实际项目中应该使用第三方库或系统 API 获取真实 CPU 使用率
			cpuUsage := 0.0 // 实际项目中需要实现真实的 CPU 使用率计算

			// 检查是否达到资源阈值
			if memoryUsage >= cfg.Resources.MemoryThreshold || cpuUsage >= cfg.Resources.CPUThreshold {
				// 记录资源使用情况
				tlog.Warn("系统资源使用率过高",
					"memoryUsage", cast.ToString(memoryUsage)+"%",
					"cpuUsage", cast.ToString(cpuUsage)+"%",
					"memoryThreshold", cfg.Resources.MemoryThreshold,
					"cpuThreshold", cfg.Resources.CPUThreshold)

				// 触发资源熔断器
				g.resourceCircuitBreaker.RecordFailure()
			} else {
				// 资源使用正常，重置资源熔断器
				g.resourceCircuitBreaker.RecordSuccess()
			}

		case <-g.stopChan:
			return
		}
	}
}

// NewGatewayGnet 创建基于 gnet 的网关实例
func NewGatewayGnet() *GatewayGnet {
	ctx := context.Background()

	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("加载配置失败，使用默认配置", "error", err)
	}

	// 从配置中读取工作池参数
	minWorkers := cfg.WorkerPool.MinWorkers
	if minWorkers <= 0 {
		minWorkers = runtime.GOMAXPROCS(0) * 8 // 最小工作线程数为CPU核心数的8倍，提高并发处理能力
	}
	maxWorkers := cfg.WorkerPool.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = runtime.GOMAXPROCS(0) * 32 // 最大工作线程数为CPU核心数的32倍，提高极限性能
	}
	queueSize := cfg.WorkerPool.QueueSize
	if queueSize <= 0 {
		queueSize = 5000000 // 进一步增大队列大小，提高消息处理容量
	}
	workerQueueSize := cfg.WorkerPool.QueueSizeThreshold
	if workerQueueSize <= 0 {
		workerQueueSize = 10000 // 增大队列大小阈值
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

	gw := &GatewayGnet{
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
		clusterID: "sgate-cluster", // 集群ID
		isLeader:  false,           // 默认不是领导者
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 8192) // 增大缓冲区大小到8KB
			},
		},
		whitelistBlacklist:     NewWhitelistBlacklist(),                             // 白名单和黑名单管理器
		workerCount:            0,                                                   // 当前工作线程数
		configPath:             "config/config.yaml",                                // 配置文件路径
		configUpdateChan:       make(chan *config.Config),                           // 配置更新通道
		cache:                  NewCache(),                                          // 缓存管理器
		loadBalancer:           NewLoadBalancer(),                                   // 负载均衡器
		resourceCircuitBreaker: NewCircuitBreaker("resource", 1, 1, 30*time.Second), // 资源熔断器
		resourceCheckInterval:  5 * time.Second,                                     // 每5秒检查一次资源使用情况
		userRateLimitConfig:    cfg.RateLimiter.UserRateLimit,                       // 用户维度限流配置
		logicClient:            NewLogicClient(GatewayInterface(nil)),               // 逻辑服 gRPC 客户端
	}

	// 使用atomic.Value存储配置
	gw.authSecret.Store(authSecret)
	gw.authRoutes.Store(authRoutes)
	gw.minWorkers.Store(int32(minWorkers))
	gw.maxWorkers.Store(int32(maxWorkers))
	gw.workerQueueSize.Store(int32(workerQueueSize))
	gw.cfg.Store(cfg)

	// 启动资源监控协程
	go gw.monitorResources()

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

	gw.fastPath = buildFastPathCache()

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
			tlog.Info("connecting to logic server...")
			time.Sleep(2 * time.Second)
			tlog.Info("attempting to connect to logic server: localhost:50052")
			if err := gw.logicClient.Connect("localhost:50052"); err != nil {
				tlog.Error("failed to connect to logic server", "error", err)
			} else {
				tlog.Info("successfully connected to logic server")
			}
		}()
	}

	go func() {
		tlog.Info("starting gRPC server", "port", ":50051")
		if err := StartGRPCServer(gw, ":50051"); err != nil {
			tlog.Error("failed to start gRPC server", "error", err)
		} else {
			tlog.Info("gRPC server started", "port", ":50051")
		}
	}()

	gw.routeManager.RegisterLogicRoute("ping")
	gw.routeManager.RegisterLogicRoute("test")

	return gw
}

// addWorker 添加工作线程
func (g *GatewayGnet) addWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount >= g.maxWorkers.Load() {
		return
	}

	// 使用原子操作增加工作线程数
	atomic.AddInt32(&g.workerCount, 1)
	g.workerPool.Add(1)
	go g.messageWorker()
}

// removeWorker 移除工作线程
func (g *GatewayGnet) removeWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount <= g.minWorkers.Load() {
		return
	}

	// 使用原子操作减少工作线程数
	atomic.AddInt32(&g.workerCount, -1)
	// 向工作线程发送停止信号
	select {
	case g.workerStopChan <- struct{}{}:
		// 成功发送停止信号
	default:
		// 通道已满，忽略
	}
}

// workerPoolManager 工作池管理器
func (g *GatewayGnet) workerPoolManager() {
	ticker := time.NewTicker(500 * time.Millisecond) // 缩短检查间隔，提高响应速度
	defer ticker.Stop()

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

			// 计算负载指标
			loadFactor := float64(queueLength) / float64(g.workerQueueSize.Load())
			connectionFactor := float64(activeConnections) / float64(500) // 每500个连接增加一个线程
			timeFactor := averageProcessingTime / float64(5)              // 处理时间超过5ms增加线程

			// 综合负载指标
			totalLoad := loadFactor + connectionFactor + timeFactor

			// 根据综合负载动态调整工作线程数
			if totalLoad > 0.8 && g.workerCount < g.maxWorkers.Load() {
				// 负载较高，添加工作线程
				// 一次添加多个线程，根据负载程度
				addCount := int(totalLoad * 2) // 增加更多线程以快速响应负载
				if addCount > 20 {
					addCount = 20 // 最多一次添加20个线程
				}
				for i := 0; i < addCount && g.workerCount < g.maxWorkers.Load(); i++ {
					g.addWorker()
				}
				// 更新工作线程数指标
				g.metrics.SetWorkerCount(int64(g.workerCount))
			} else if totalLoad < 0.2 && g.workerCount > g.minWorkers.Load() {
				// 负载较低，移除工作线程
				// 一次移除多个线程，快速减少空闲线程
				removeCount := int(g.workerCount - g.minWorkers.Load())
				if removeCount > 5 {
					removeCount = 5 // 最多一次移除5个线程
				}
				for i := 0; i < removeCount && g.workerCount > g.minWorkers.Load(); i++ {
					g.removeWorker()
				}
				// 更新工作线程数指标
				g.metrics.SetWorkerCount(int64(g.workerCount))
			}
		}
	}
}

// wsHeartbeatChecker WebSocket心跳检查器
func (g *GatewayGnet) wsHeartbeatChecker() {
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
func (g *GatewayGnet) checkWebSocketConnections() {
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
func (g *GatewayGnet) configWatcher() {
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
func (g *GatewayGnet) handleConfigUpdate(newCfg *config.Config) {
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

	// 记录资源限制配置更新
	tlog.Info("配置更新完成",
		"memoryThreshold", newCfg.Resources.MemoryThreshold,
		"cpuThreshold", newCfg.Resources.CPUThreshold,
		"enableResourceCircuitBreaker", newCfg.Resources.EnableResourceCircuitBreaker)
}

// messageWorker 消息处理工作线程
func (g *GatewayGnet) messageWorker() {
	defer func() {
		g.workerPool.Done()
		// 使用原子操作减少工作线程数
		atomic.AddInt32(&g.workerCount, -1)
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
func (g *GatewayGnet) registerDefaultRoutes() {
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
	// 注册ping路由
	g.registerPingRoute()

	// 注册默认的测试路由，减少路由不存在的情况
	g.routeManager.RegisterRoute("test", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("testResult", map[string]string{
			"success": "true",
			"message": "Test route works",
		}))
	})

	// 注册默认的错误处理路由
	g.routeManager.RegisterRoute("error", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			if errorMsg, ok := payloadMap["message"]; ok {
				callback(NewResponseMessage("error", map[string]string{
					"message": errorMsg,
				}))
			} else {
				callback(NewResponseMessage("error", map[string]string{
					"message": "Unknown error",
				}))
			}
		} else {
			callback(NewResponseMessage("error", map[string]string{
				"message": "Invalid error format",
			}))
		}
	})

	// 注册默认的消息队列测试路由
	g.routeManager.RegisterRoute("queueTest", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		if payloadMap, ok := payload.(map[string]string); ok {
			if message, ok := payloadMap["message"]; ok {
				// 创建测试消息
				protoMsg := &protobuf.Message{
					ConnectionId: connectionID,
					UserUuid:     "test_user",
					Route:        "test",
					Payload:      map[string]string{"data": message},
					Sequence:     1,
				}
				// 入队消息
				messageID := "test_msg_" + generateConnectionID()
				err := g.messageQueue.Enqueue(protoMsg, messageID)
				if err != nil {
					callback(NewResponseMessage("queueResult", map[string]string{
						"success": "false",
						"error":   err.Error(),
					}))
				} else {
					callback(NewResponseMessage("queueResult", map[string]string{
						"success":   "true",
						"messageID": messageID,
					}))
				}
			} else {
				callback(NewResponseMessage("queueResult", map[string]string{
					"success": "false",
					"error":   "No message provided",
				}))
			}
		} else {
			callback(NewResponseMessage("queueResult", map[string]string{
				"success": "false",
				"error":   "Invalid payload format",
			}))
		}
	})

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

// GnetConnContext 连接上下文结构
type GnetConnContext struct {
	ConnectionID string
	FrameBuf     []byte
	FrameOff     int
	FrameCap     int
}

// OnOpen 连接打开时的回调
func (g *GatewayGnet) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	// 处理新连接
	// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
	tempUserUUID := "temp_" + generateConnectionID()
	connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
	// 分配缓冲区并设置连接上下文
	connCtx := &GnetConnContext{
		ConnectionID: connectionID,
		FrameBuf:     nil,
	}
	c.SetContext(connCtx)

	// 收集连接指标
	g.metrics.IncConnectionsTotal()
	g.metrics.IncConnectionsActive()

	// 输出调试日志
	tlog.Debug("新的连接", "connectionID", connectionID, "userUUID", tempUserUUID)

	return
}

// OnClose 连接关闭时的回调
func (g *GatewayGnet) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnClose panic recovered", "error", cast.ToString(r))
			action = gnet.Close
		}
	}()

	var connectionID string
	connCtx := c.Context()

	if connCtx != nil {
		if ctx, ok := connCtx.(*GnetConnContext); ok {
			connectionID = ctx.ConnectionID
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
func (g *GatewayGnet) OnTraffic(c gnet.Conn) (action gnet.Action) {
	if g.fastPath != nil {
		data, err := c.Peek(-1)
		if err == nil && len(data) >= 10 {
			connCtx := c.Context()
			if connCtx != nil {
				if ctx, ok := connCtx.(*GnetConnContext); ok && len(ctx.FrameBuf) <= ctx.FrameOff {
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

func (g *GatewayGnet) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnTraffic panic recovered", "error", cast.ToString(r))
			action = gnet.Close
		}
	}()

	data, err := c.Next(-1)
	if err != nil {
		return gnet.Close
	}
	if len(data) == 0 {
		return
	}

	atomic.AddInt64(&fastPathTotal, 1)

	g.metrics.AddBytesReceived(int64(len(data)))

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
		writeErrorFrame(c, NewErrorMessage("error", "IP address is blacklisted", "", ""))
		return gnet.Close
	}

	if !g.rateLimiter.Allow("ip", clientIP) {
		tlog.Warn("请求被限流（IP维度）", "clientIP", clientIP)
		writeErrorFrame(c, NewErrorMessage("error", "Rate limit exceeded (IP dimension)", "", ""))
		return gnet.Close
	}

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
	ctx, ok := connCtx.(*GnetConnContext)
	if !ok {
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf[ctx.FrameOff:], data...)
	ctx.FrameOff = 0

	for len(ctx.FrameBuf)-ctx.FrameOff >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[ctx.FrameOff : ctx.FrameOff+4])
		if frameLen == 0 || frameLen > 4*1024*1024 {
			tlog.Error("无效帧长度", "frameLen", frameLen)
			ctx.FrameBuf = nil
			ctx.FrameOff = 0
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if len(ctx.FrameBuf)-ctx.FrameOff < totalLen {
			return
		}

		frameData := ctx.FrameBuf[ctx.FrameOff+4 : ctx.FrameOff+totalLen]

		isTestRoute := len(frameData) >= 6 && frameData[0] == testRoutePattern[0] && frameData[1] == testRoutePattern[1] &&
			frameData[2] == testRoutePattern[2] && frameData[3] == testRoutePattern[3] &&
			frameData[4] == testRoutePattern[4] && frameData[5] == testRoutePattern[5]

		if isTestRoute && g.fastPath != nil {
			testCount := 1
			ctx.FrameOff += totalLen
			atomic.AddInt64(&fastPathTotal, 1)

			for len(ctx.FrameBuf)-ctx.FrameOff >= 4 {
				nextFrameLen := binary.BigEndian.Uint32(ctx.FrameBuf[ctx.FrameOff : ctx.FrameOff+4])
				if nextFrameLen == 0 || nextFrameLen > 4*1024*1024 {
					break
				}
				nextTotalLen := 4 + int(nextFrameLen)
				if len(ctx.FrameBuf)-ctx.FrameOff < nextTotalLen {
					break
				}
				nextFrameData := ctx.FrameBuf[ctx.FrameOff+4 : ctx.FrameOff+nextTotalLen]
				if len(nextFrameData) >= 6 && nextFrameData[0] == testRoutePattern[0] && nextFrameData[1] == testRoutePattern[1] &&
					nextFrameData[2] == testRoutePattern[2] && nextFrameData[3] == testRoutePattern[3] &&
					nextFrameData[4] == testRoutePattern[4] && nextFrameData[5] == testRoutePattern[5] {
					testCount++
					ctx.FrameOff += nextTotalLen
					continue
				}
				break
			}

			atomic.AddInt64(&fastPathTotal, int64(testCount-1))

			if testCount == 1 {
				c.Write(g.fastPath.testFrame)
			} else if bf := g.fastPath.getBatchFrame(testCount); bf != nil {
				c.Write(bf)
			} else {
				bp := batchBufPool.Get().(*[]byte)
				b := (*bp)[:0]
				for i := 0; i < testCount; i++ {
					b = append(b, g.fastPath.testFrame...)
				}
				c.Write(b)
				*bp = b
				batchBufPool.Put(bp)
			}
			continue
		}

		route := extractRouteFast(frameData)
		isFastPath := route == "ping" && !g.isLogicConnected() && g.fastPath != nil

		if isFastPath {
			c.Write(g.fastPath.pongFrame)
			ctx.FrameOff += totalLen
			continue
		}

		frameCopy := make([]byte, len(frameData))
		copy(frameCopy, frameData)

		ctx.FrameOff += totalLen

		if ret := g.handleTCPRequest(c, frameCopy); ret == gnet.Close {
			return gnet.Close
		}
	}

	if ctx.FrameOff > 0 {
		remaining := len(ctx.FrameBuf) - ctx.FrameOff
		if remaining == 0 {
			ctx.FrameBuf = nil
		} else {
			copy(ctx.FrameBuf, ctx.FrameBuf[ctx.FrameOff:])
			ctx.FrameBuf = ctx.FrameBuf[:remaining]
		}
		ctx.FrameOff = 0
	}

	return
}

// handleTCPRequest 处理TCP请求
func (g *GatewayGnet) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Empty data", "", "")))
		return
	}

	route := extractRouteFast(data)

	if route == "test" && g.fastPath != nil {
		c.Write(g.fastPath.testFrame)
		return
	}

	if route == "ping" && g.fastPath != nil && !g.isLogicConnected() {
		c.Write(g.fastPath.pongFrame)
		return
	}

	message := &protobuf.Message{}
	if err := proto.Unmarshal(data, message); err != nil {
		writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Invalid message format", err.Error(), string(data))))
		return
	}

	logicRoutesIntegrity := map[string]bool{"test": true, "ping": true, "handshake": true}
	if !logicRoutesIntegrity[message.Route] && !g.isLogicRoute(message.Route) {
		if err := g.messageIntegrity.ProcessMessage(message); err != nil {
			writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Message integrity error", err.Error(), "")))
			return
		}
	}

	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*GnetConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		c.SetContext(&GnetConnContext{
			ConnectionID: connectionID,
			FrameBuf:     nil,
		})
		tlog.Info("生成新的连接ID", "connectionID", connectionID, "userUUID", tempUserUUID)
	}

	if message.Route == "handshake" {
		return g.handleHandshake(c, connectionID, message)
	}

	logicRoutes := map[string]bool{"test": true, "ping": true}
	if !logicRoutes[message.Route] && !g.isLogicRoute(message.Route) {
		if _, exists := g.versionNegotiation.GetClientVersion(connectionID); !exists {
			writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Handshake required", "Protocol version negotiation is required", "")))
			return
		}
	}

	userUUID := message.UserUuid

	if userUUID != "" {
		g.connectionManager.UpdateUserConnection(connectionID, "", userUUID)
		tlog.Debug("收到用户UUID", "connectionID", connectionID, "userUUID", userUUID)
	}

	route2 := message.Route
	if route2 == "" {
		tlog.Warn("消息格式错误，缺少route字段", "message", message)
		writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Invalid message format: missing route", "", "")))
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

	if route2 == "ping" && !g.isLogicConnected() {
		writeMsgFrame(c, NewResponseMessage("pong", map[string]string{
			"timestamp": cast.ToString(time.Now().UnixMilli()),
		}))
		return
	}

	if !g.rateLimiter.Allow("route", route2) {
		writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Rate limit exceeded (route dimension)", "", "")))
		return
	}

	if userUUID != "" && g.userRateLimitConfig.Enabled {
		if !g.rateLimiter.Allow("user", userUUID) {
			writeFrameFast(c, mustMarshalError(NewErrorMessage("error", "Rate limit exceeded (user dimension)", "", "")))
			if g.userRateLimitConfig.Action == "close" {
				return gnet.Close
			}
			return
		}
	}

	traceID := GenerateTraceID()
	if traceIDValue, ok := payload["trace_id"]; ok {
		traceID = traceIDValue
	}

	msg := GetMessage()
	msg.ConnectionID = connectionID
	msg.Route = route2
	msg.Payload = payload
	msg.Conn = c
	msg.TraceID = traceID

	select {
	case g.messagePool <- msg:
	default:
		g.handleMessage(msg)
	}

	return
}

// handleHandshake 处理握手消息
func (g *GatewayGnet) handleHandshake(c gnet.Conn, connectionID string, message *protobuf.Message) gnet.Action {
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

// handleMessage 处理消息
func (g *GatewayGnet) handleMessage(msg *Message) {
	if !g.resourceCircuitBreaker.Allow() {
		writeErrorFrame(msg.Conn, NewErrorMessage("error", "Service temporarily unavailable", "Resource circuit breaker is open", ""))
		PutMessage(msg)
		return
	}

	breaker := g.circuitBreakerManager.GetCircuitBreaker(msg.Route, 5, 3, 30*time.Second)

	if !breaker.Allow() {
		writeErrorFrame(msg.Conn, NewErrorMessage("error", "Service temporarily unavailable", "Circuit breaker is open", ""))
		PutMessage(msg)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Internal server error", cast.ToString(r), ""))
		}
		PutMessage(msg)
	}()

	if !g.routeManager.HasRoute(msg.Route) {
		if g.isLogicConnected() {
			g.forwardToLogic(msg)
			return
		}
		writeErrorFrame(msg.Conn, NewErrorMessage("error", "Route not found", "The requested route does not exist", ""))
		return
	}

	if g.requiresAuth(msg.Route) {
		token, ok := getTokenFromPayloadMap(msg.Payload)
		if !ok {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Missing token", "Authentication token is required", ""))
			return
		}

		authSecretVal := g.authSecret.Load()
		if authSecretVal == nil {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Server configuration error", "", ""))
			return
		}
		authSecret, ok := authSecretVal.(string)
		if !ok {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Server configuration error", "", ""))
			return
		}

		claims, err := ValidateToken(token, authSecret)
		if err != nil {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Invalid token", err.Error(), ""))
			return
		}

		msg.Payload = addUserInfoToPayloadMap(msg.Payload, claims.UserID, claims.Role)
	}

	ctx := map[string]interface{}{
		"connection_id": msg.ConnectionID,
		"route":         msg.Route,
		"gateway":       g,
	}

	g.routeManager.HandleRoute(msg.ConnectionID, msg.Route, msg.Payload, func(response interface{}) {
		if response == nil {
			return
		}

		skipIntegrity := msg.Route == "test" || msg.Route == "ping"

		if responseMsg, ok := response.(*protobuf.Message); ok {
			if !skipIntegrity {
				g.messageIntegrity.PrepareMessage(responseMsg)
			} else {
				responseMsg.Timestamp = time.Now().UnixMilli()
				if responseMsg.ProtocolVersion == "" {
					responseMsg.ProtocolVersion = "1.0.0"
				}
			}
			writeMsgFrame(msg.Conn, responseMsg)
		} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
			if !skipIntegrity {
				g.messageIntegrity.PrepareErrorResponse(errorMsg)
			}
			writeErrorFrame(msg.Conn, errorMsg)
		} else {
			writeErrorFrame(msg.Conn, NewErrorMessage("error", "Invalid response format", "", ""))
		}
	}, ctx)
}

// OnBoot 服务器启动时的回调
func (g *GatewayGnet) OnBoot(engine gnet.Engine) (action gnet.Action) {
	return
}

// SetTLSConfig 设置TLS配置
func (g *GatewayGnet) SetTLSConfig(config *tls.Config) {
	g.tlsConfig = config
}

// GetTLSConfig 获取TLS配置
func (g *GatewayGnet) GetTLSConfig() *tls.Config {
	return g.tlsConfig
}

// GetVersion 获取网关版本
func (g *GatewayGnet) GetVersion() string {
	return "1.0.0"
}

func (g *GatewayGnet) GetRouteManager() *RouteManager {
	return g.routeManager
}

func (g *GatewayGnet) GetConnectionManager() *ConnectionManager {
	return g.connectionManager
}

func (g *GatewayGnet) isLogicConnected() bool {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return true
	}
	return g.logicClient.IsConnected()
}

func (g *GatewayGnet) isLogicRoute(route string) bool {
	if g.isLogicConnected() {
		return true
	}
	return route == "test" || route == "ping"
}

func (g *GatewayGnet) forwardToLogic(msg *Message) {
	var logicClient LogicClientProvider
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		logicClient = g.logicClientPool
	} else {
		logicClient = g.logicClient
	}

	if logicClient == nil || !logicClient.IsConnected() {
		errorMsg := NewErrorMessage("error", "Logic server not connected", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		msg.Conn.Writev([][]byte{responseData})
		return
	}

	protoMsg := &protobuf.Message{
		ConnectionId: msg.ConnectionID,
		Route:        msg.Route,
		Payload:      msg.Payload,
		Timestamp:    time.Now().UnixMilli(),
	}

	if err := logicClient.SendMessage(protoMsg); err != nil {
		tlog.Error("failed to forward message to logic server", "error", err, "route", msg.Route)
		errorMsg := NewErrorMessage("error", "Failed to forward to logic server", err.Error(), "")
		responseData, _ := proto.Marshal(errorMsg)
		msg.Conn.Writev([][]byte{responseData})
	}
}

// registerVersionRoute 注册版本路由
func (g *GatewayGnet) registerVersionRoute() {
	g.routeManager.RegisterRoute("version", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("version", map[string]string{
			"version":   g.GetVersion(),
			"clusterID": g.clusterID,
			"isLeader":  cast.ToString(g.isLeader),
		}))
	})
}

// registerPingRoute 注册ping路由
func (g *GatewayGnet) registerPingRoute() {
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("ping", map[string]interface{}{
			"message":   "Pong",
			"timestamp": time.Now().Unix(),
		}))
	})
}

// OnTick 定时回调
func (g *GatewayGnet) OnTick() (delay time.Duration, action gnet.Action) {
	// 定期记录指标
	g.metrics.LogMetrics()

	// 更新消息队列长度指标
	g.metrics.SetQueueLength(int64(len(g.messagePool)))

	return 1 * time.Second, gnet.None
}

// OnShutdown 服务器关闭时的回调
func (g *GatewayGnet) OnShutdown(engine gnet.Engine) {
	// 服务器关闭时的清理工作
	g.Close()
}

// Close 关闭网关
func (g *GatewayGnet) Close() {
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

	if g.messageACK != nil {
		g.messageACK.Stop()
	}

	tlog.Info("网关已关闭")
}

// Broadcast 广播消息
func (g *GatewayGnet) Broadcast(message string) {
	// 遍历所有连接，发送广播消息
	g.connectionManager.Broadcast(message)
}

// SetTransportType 设置端口到传输类型的映射
func (g *GatewayGnet) SetTransportType(port string, transportType string) {
	g.transportType[port] = transportType
}

// HandleWebSocket 处理WebSocket连接
func (g *GatewayGnet) HandleWebSocket(c gnet.Conn, data []byte) (action gnet.Action) {
	// 暂时返回 gnet.None，后续可以实现完整的WebSocket处理逻辑
	return gnet.None
}

// requiresAuth 检查路由是否需要认证
func (g *GatewayGnet) requiresAuth(route string) bool {
	// 从 atomic.Value 中获取认证路由
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

// Start 启动网关
func (g *GatewayGnet) Start(addr string) error {
	tcpKeepaliveTime := 30 * time.Second
	numLoop := runtime.GOMAXPROCS(0) * 2
	if numLoop < 16 {
		numLoop = 16
	}
	options := []gnet.Option{
		gnet.WithMulticore(true),
		gnet.WithReusePort(true),
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithTCPKeepAlive(tcpKeepaliveTime),
		gnet.WithNumEventLoop(numLoop),
		gnet.WithReadBufferCap(32768),
		gnet.WithWriteBufferCap(32768),
		gnet.WithSocketRecvBuffer(524288),
		gnet.WithSocketSendBuffer(524288),
		gnet.WithLogLevel(logging.WarnLevel),
		gnet.WithLogger(g),
	}

	return gnet.Run(g, addr, options...)
}

// Debugf 实现gnet.Logger接口
func (g *GatewayGnet) Debugf(format string, args ...interface{}) {
	tlog.Debug(fmt.Sprintf(format, args...))
}

// Infof 实现gnet.Logger接口
func (g *GatewayGnet) Infof(format string, args ...interface{}) {
	tlog.Info(fmt.Sprintf(format, args...))
}

// Warnf 实现gnet.Logger接口
func (g *GatewayGnet) Warnf(format string, args ...interface{}) {
	tlog.Warn(fmt.Sprintf(format, args...))
}

// Errorf 实现gnet.Logger接口
func (g *GatewayGnet) Errorf(format string, args ...interface{}) {
	tlog.Error(fmt.Sprintf(format, args...))
}

// Fatalf 实现gnet.Logger接口
func (g *GatewayGnet) Fatalf(format string, args ...interface{}) {
	tlog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
