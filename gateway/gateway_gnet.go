package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/panjf2000/gnet/v2/pkg/logging"
	"github.com/streasure/sgate/gateway/protobuf"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"google.golang.org/protobuf/proto"
	tlog "github.com/streasure/treasure-slog"
)

// GatewayGnet 基于 gnet 的网关实现
type GatewayGnet struct {
	connectionManager  *ConnectionManager  // 连接管理器
	routeManager       *RouteManager       // 路由管理器
	messagePool        chan *Message       // 消息队列
	workerPool         sync.WaitGroup      // 工作池
	stopChan           chan struct{}       // 停止信号通道
	workerStopChan     chan struct{}       // 工作线程停止信号通道
	metrics            *metrics.Metrics    // 指标收集器
	transportType      map[string]string   // 端口到传输类型的映射
	rateLimiter        *RateLimiter        // 速率限制器
	authSecret         atomic.Value        // 认证密钥，使用atomic.Value存储
	authRoutes         atomic.Value        // 需要认证的路由，使用atomic.Value存储
	ctx                context.Context     // 上下文
	tlsConfig          *tls.Config         // TLS配置
	clusterID          string              // 集群ID
	isLeader           bool                // 是否是领导者
	bufferPool         sync.Pool           // 缓冲区池
	whitelistBlacklist *WhitelistBlacklist // 白名单和黑名单管理器
	workerMutex        sync.Mutex          // 工作池互斥锁
	workerCount        int                 // 当前工作线程数
	minWorkers         atomic.Int32        // 最小工作线程数，使用atomic.Int32存储
	maxWorkers         atomic.Int32        // 最大工作线程数，使用atomic.Int32存储
	workerQueueSize    atomic.Int32        // 工作队列大小阈值，使用atomic.Int32存储
	cfg                atomic.Value        // 配置实例，使用atomic.Value存储
	wsConnections      sync.Map            // 活跃的WebSocket连接
	configPath         string              // 配置文件路径
	configUpdateChan   chan *config.Config // 配置更新通道
	cache              *Cache              // 缓存管理器
	loadBalancer       *LoadBalancer       // 负载均衡器
	messageIntegrity   *MessageIntegrity   // 消息完整性管理器
	messageACK        *MessageACK         // 消息确认管理器
	compressor        *Compressor         // 压缩管理器
	versionNegotiation *VersionNegotiation // 版本协商管理器
	circuitBreakerManager *CircuitBreakerManager // 熔断器管理器
	messageQueue      *MessageQueue       // 消息队列管理器
	tracer            *Tracer             // 链路追踪器
	resourceCircuitBreaker *CircuitBreaker // 资源熔断器
	resourceCheckInterval time.Duration    // 资源检查间隔
}

// monitorResources 监控系统资源使用情况
func (g *GatewayGnet) monitorResources() {
	ticker := time.NewTicker(g.resourceCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 获取当前配置
			cfg := g.cfg.Load().(*config.Config)
			
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
					"memoryUsage", fmt.Sprintf("%.2f%%", memoryUsage), 
					"cpuUsage", fmt.Sprintf("%.2f%%", cpuUsage),
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
		minWorkers = runtime.GOMAXPROCS(0) * 4  // 最小工作线程数为CPU核心数的4倍
	}
	maxWorkers := cfg.WorkerPool.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = runtime.GOMAXPROCS(0) * 16  // 最大工作线程数为CPU核心数的16倍
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

	gw := &GatewayGnet{
		connectionManager: NewConnectionManager(), // 创建连接管理器
		routeManager:      NewRouteManager(),                 // 创建路由管理器
		messagePool:       make(chan *Message, queueSize),    // 创建消息队列
		stopChan:          make(chan struct{}),               // 创建停止信号通道
		workerStopChan:    make(chan struct{}),               // 创建工作线程停止信号通道
		metrics:           metrics.NewMetrics(),              // 创建指标收集器
		transportType:     make(map[string]string),           // 创建端口到传输类型的映射
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
				return make([]byte, 4096) // 增大缓冲区大小
			},
		},
		whitelistBlacklist: NewWhitelistBlacklist(), // 白名单和黑名单管理器
		workerCount:        0,                  // 当前工作线程数
		configPath:         "config/config.yaml", // 配置文件路径
		configUpdateChan:   make(chan *config.Config), // 配置更新通道
		cache:              NewCache(),               // 缓存管理器
		loadBalancer:       NewLoadBalancer(),        // 负载均衡器
		resourceCircuitBreaker: NewCircuitBreaker("resource", 1, 1, 30*time.Second), // 资源熔断器
		resourceCheckInterval: 5 * time.Second, // 每5秒检查一次资源使用情况
	}

	// 使用atomic.Value存储配置
	gw.authSecret.Store(authSecret)
	gw.authRoutes.Store(authRoutes)
	gw.minWorkers.Store(int32(minWorkers))
	gw.maxWorkers.Store(int32(maxWorkers))
	gw.workerQueueSize.Store(int32(workerQueueSize))
	gw.cfg.Store(cfg)

	// 启动速率限制器
	gw.rateLimiter.Start()

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

	return gw
}

// addWorker 添加工作线程
func (g *GatewayGnet) addWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount >= int(g.maxWorkers.Load()) {
		return
	}

	g.workerCount++
	g.workerPool.Add(1)
	go g.messageWorker()
}

// removeWorker 移除工作线程
func (g *GatewayGnet) removeWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount <= int(g.minWorkers.Load()) {
		return
	}

	g.workerCount--
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
			timeFactor := averageProcessingTime / float64(5) // 处理时间超过5ms增加线程
			
			// 综合负载指标
			totalLoad := loadFactor + connectionFactor + timeFactor
			
			// 根据综合负载动态调整工作线程数
			if totalLoad > 0.8 && g.workerCount < int(g.maxWorkers.Load()) {
				// 负载较高，添加工作线程
				// 一次添加多个线程，根据负载程度
				addCount := int(totalLoad * 2) // 增加更多线程以快速响应负载
				if addCount > 20 {
					addCount = 20 // 最多一次添加20个线程
				}
				for i := 0; i < addCount && g.workerCount < int(g.maxWorkers.Load()); i++ {
					g.addWorker()
				}
				// 更新工作线程数指标
				g.metrics.SetWorkerCount(int64(g.workerCount))
			} else if totalLoad < 0.2 && g.workerCount > int(g.minWorkers.Load()) {
				// 负载较低，移除工作线程
				// 一次移除多个线程，快速减少空闲线程
				removeCount := g.workerCount - int(g.minWorkers.Load())
				if removeCount > 5 {
					removeCount = 5 // 最多一次移除5个线程
				}
				for i := 0; i < removeCount && g.workerCount > int(g.minWorkers.Load()); i++ {
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
	g.rateLimiter.Start()

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
		g.workerMutex.Lock()
		g.workerCount--
		g.workerMutex.Unlock()
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
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("pong", map[string]string{
			"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
		}))
	})

	// 注册getConnections路由
	g.routeManager.RegisterRoute("getConnections", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("connections", map[string]string{
			"count": fmt.Sprintf("%d", g.connectionManager.GetConnectionCount()),
		}))
	})

	// 注册broadcast路由
	g.routeManager.RegisterRoute("broadcast", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	g.routeManager.RegisterRoute("health", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("health", map[string]string{
			"status":            "healthy",
			"timestamp":         fmt.Sprintf("%d", time.Now().UnixMilli()),
			"activeConnections": fmt.Sprintf("%d", g.connectionManager.GetConnectionCount()),
			"totalConnections":  fmt.Sprintf("%d", g.metrics.GetConnectionsTotal()),
			"messagesReceived":  fmt.Sprintf("%d", g.metrics.GetMessagesReceived()),
			"messagesProcessed": fmt.Sprintf("%d", g.metrics.GetMessagesProcessed()),
			"messagesFailed":    fmt.Sprintf("%d", g.metrics.GetMessagesFailed()),
		}))
	})

	// 注册API文档路由
	g.routeManager.RegisterRoute("api-docs", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	g.routeManager.RegisterRoute("test", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("testResult", map[string]string{
			"success": "true",
			"message": "Test route works",
		}))
	})

	// 注册默认的错误处理路由
	g.routeManager.RegisterRoute("error", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	g.routeManager.RegisterRoute("queueTest", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	g.routeManager.RegisterRoute("addWhitelist", func(connectionID string, payload interface{}, callback func(interface{})) {
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

	g.routeManager.RegisterRoute("removeWhitelist", func(connectionID string, payload interface{}, callback func(interface{})) {
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

	g.routeManager.RegisterRoute("getWhitelist", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	g.routeManager.RegisterRoute("addBlacklist", func(connectionID string, payload interface{}, callback func(interface{})) {
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

	g.routeManager.RegisterRoute("removeBlacklist", func(connectionID string, payload interface{}, callback func(interface{})) {
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

	g.routeManager.RegisterRoute("getBlacklist", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	Buffer       []byte
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
		Buffer:       make([]byte, 4096), // 4KB 缓冲区
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
	// 处理连接关闭
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
	// 检查客户端IP是否在黑名单中
	clientAddr := c.RemoteAddr().(*net.TCPAddr)
	clientIP := clientAddr.IP.String()
	if g.whitelistBlacklist != nil && g.whitelistBlacklist.IsInBlacklist(clientIP) {
		// 输出警告日志
		tlog.Warn("请求被拒绝，IP在黑名单中", "clientIP", clientIP)
		// 发送黑名单响应
		errorMsg := NewErrorMessage("error", "IP address is blacklisted", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return gnet.Close
	}

	// 检查速率限制（IP维度）
	if !g.rateLimiter.Allow("ip", clientIP) {
		// 输出警告日志
		tlog.Warn("请求被限流（IP维度）", "clientIP", clientIP)
		// 发送限流响应
		errorMsg := NewErrorMessage("error", "Rate limit exceeded (IP dimension)", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return gnet.Close
	}

	// 读取数据 - 使用零拷贝技术
	// 使用 gnet 推荐的 Next 方法读取数据
	data, err := c.Next(-1) // 读取所有可用数据
	if err != nil {
		// 输出错误日志
		tlog.Error("读取数据失败", "error", err)
		return gnet.Close
	}

	// 增加接收字节数指标
	g.metrics.AddBytesReceived(int64(len(data)))

	// 输出调试日志
	tlog.Debug("收到数据", "data", string(data), "length", len(data))

	// 获取连接的端口
	port := c.LocalAddr().String()
	// 提取端口号
	for i := len(port) - 1; i >= 0; i-- {
		if port[i] == ':' {
			port = port[i+1:]
			break
		}
	}

	// 获取传输类型
	transportType := g.transportType[port]

	// 输出调试日志
	tlog.Debug("收到数据", "port", port, "transportType", transportType, "data", string(data))

	// 根据传输类型处理数据
	if transportType == "websocket" {
		// 使用新的WebSocket处理逻辑
		return g.HandleWebSocket(c, data)
	}

	// 检查是否是HTTP请求
	if isHTTPRequest(data) {
		// 处理HTTP请求
		return g.handleHTTPRequest(c, data)
	}

	// 处理TCP/UDP请求
	return g.handleTCPRequest(c, data)
}

// handleHTTPRequest 处理HTTP请求
func (g *GatewayGnet) handleHTTPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	// 检查是否是 Expect: 100-continue 请求
	if bytes.Contains(data, []byte("Expect: 100-continue")) {
		// 发送 100 Continue 响应
		continueResponse := []byte("HTTP/1.1 100 Continue\r\n\r\n")
		c.Write(continueResponse)
		// 不处理这个请求，等待后续的请求体
		return
	}

	// 解析HTTP请求
	// 这里简化处理，实际应该使用更完整的HTTP解析
	// 提取请求体作为payload
	payload := extractPayloadFromHTTP(data)

	// 从payload中提取route
	route, ok := payload["route"]
	if !ok || route == "" {
		// 如果payload中没有route字段，使用路径作为route
		route = extractRouteFromHTTP(data)
	}

	// 从payload中提取user_uuid
	userUUID := payload["user_uuid"]

	// 处理消息
	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*GnetConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		// 设置连接上下文
		c.SetContext(&GnetConnContext{
			ConnectionID: connectionID,
			Buffer:       make([]byte, 4096), // 4KB 缓冲区
		})
		tlog.Info("生成新的连接ID", "connectionID", connectionID, "userUUID", tempUserUUID)
	}

	// 如果用户UUID存在，更新连接的用户映射
	if userUUID != "" {
		// 生成临时用户UUID（如果是新连接）
		oldUserUUID := "temp_" + connectionID
		// 更新连接的用户映射
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, userUUID)
		tlog.Debug("收到用户UUID", "connectionID", connectionID, "userUUID", userUUID)
	}

	// 从对象池获取消息对象
	msg := GetMessage()
	msg.ConnectionID = connectionID
	msg.Route = route
	msg.Payload = payload
	msg.Conn = c

	select {
	case g.messagePool <- msg:
		// 消息已加入队列
	default:
		// 消息队列已满，直接处理
		g.handleMessage(msg)
	}

	return
}

// handleTCPRequest 处理TCP请求
func (g *GatewayGnet) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	// 检查数据长度
	if len(data) == 0 {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Empty data", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return
	}

	// 解析消息
	message := &protobuf.Message{}
	if err := proto.Unmarshal(data, message); err != nil {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Invalid message format", err.Error(), string(data))
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return
	}

	// 验证消息完整性
	if err := g.messageIntegrity.ProcessMessage(message); err != nil {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Message integrity error", err.Error(), "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return
	}

	// 处理消息
	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*GnetConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		// 设置连接上下文
		c.SetContext(&GnetConnContext{
			ConnectionID: connectionID,
			Buffer:       make([]byte, 4096), // 4KB 缓冲区
		})
		tlog.Info("生成新的连接ID", "connectionID", connectionID, "userUUID", tempUserUUID)
	}

	// 处理握手消息
	if message.Route == "handshake" {
		return g.handleHandshake(c, connectionID, message)
	}

	// 检查协议版本协商
	if _, exists := g.versionNegotiation.GetClientVersion(connectionID); !exists {
		// 未完成握手，发送错误响应
		errorMsg := NewErrorMessage("error", "Handshake required", "Protocol version negotiation is required", "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return
	}

	// 获取用户UUID
	userUUID := message.UserUuid

	// 如果用户UUID存在，更新连接的用户映射
	if userUUID != "" {
		// 生成临时用户UUID（如果是新连接）
		oldUserUUID := "temp_" + connectionID
		// 更新连接的用户映射
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, userUUID)
		tlog.Debug("收到用户UUID", "connectionID", connectionID, "userUUID", userUUID)
	}

	route := message.Route
	if route == "" {
		// 输出警告日志
		tlog.Warn("消息格式错误，缺少route字段", "message", message)// 发送错误响应
		errorMsg := NewErrorMessage("error", "Invalid message format: missing route", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return
	}

	// 获取payload
	payload := message.Payload
	if payload == nil {
		payload = make(map[string]string)
	}

	// 输出调试日志
	tlog.Debug("处理TCP消息", "connectionID", connectionID, "userUUID", userUUID, "route", route, "payload", payload)

	// 从对象池获取消息对象
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
func (g *GatewayGnet) handleHandshake(c gnet.Conn, connectionID string, message *protobuf.Message) gnet.Action {
	// 解析握手数据
	handshake := &protobuf.Handshake{}
	if err := proto.Unmarshal([]byte(message.Payload["handshake_data"]), handshake); err != nil {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Invalid handshake data", err.Error(), "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return gnet.None
	}

	// 处理握手
	negotiatedVersion, err := g.versionNegotiation.ProcessHandshake(connectionID, handshake)
	if err != nil {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Handshake failed", err.Error(), "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return gnet.None
	}

	// 生成握手响应
	response := g.versionNegotiation.GenerateHandshakeResponse(negotiatedVersion)
	// 添加消息完整性校验
	g.messageIntegrity.PrepareMessage(response)
	// 序列化响应
	responseData, err := proto.Marshal(response)
	if err != nil {
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Failed to generate handshake response", err.Error(), "")
		responseData, _ := proto.Marshal(errorMsg)
		c.Write(responseData)
		return gnet.None
	}

	// 发送响应
	c.Write(responseData)
	return gnet.None
}

// handleMessage 处理消息
func (g *GatewayGnet) handleMessage(msg *Message) {
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

	// 检查资源熔断器
	if !g.resourceCircuitBreaker.Allow() {
		// 记录事件
		g.tracer.AddEvent(span, "resource_circuit_breaker_open", map[string]string{
			"route": msg.Route,
		})

		// 收集处理失败的消息指标
		g.metrics.IncMessagesFailed()

		// 资源熔断器打开，拒绝请求
		errorMsg := NewErrorMessage("error", "Service temporarily unavailable", "Resource circuit breaker is open", "")
		responseData, _ := proto.Marshal(errorMsg)
		msg.Conn.Write(responseData)

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

		// 收集处理失败的消息指标
		g.metrics.IncMessagesFailed()

		// 熔断器打开，拒绝请求
		errorMsg := NewErrorMessage("error", "Service temporarily unavailable", "Circuit breaker is open", "")
		responseData, _ := proto.Marshal(errorMsg)
		msg.Conn.Write(responseData)

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

			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("处理消息异常",
				"connectionID", msg.ConnectionID,
				"route", msg.Route,
				"error", r)

			// 发送错误响应
			errorMsg := NewErrorMessage("error", "Internal server error", fmt.Sprintf("%v", r), "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
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

		// 收集处理失败的消息指标
		g.metrics.IncMessagesFailed()
		// 路由不存在，发送错误响应
		errorMsg := NewErrorMessage("error", "Route not found", "The requested route does not exist", "")
		responseData, _ := proto.Marshal(errorMsg)
		msg.Conn.Write(responseData)
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

			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输入验证失败，发送错误响应
			errorMsg := NewErrorMessage("error", "Invalid input detected", "Input validation failed", "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
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

			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Missing token", "Authentication token is required", "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
			return
		}

		// 验证token
		claims, err := ValidateToken(token, g.authSecret.Load().(string))
		if err != nil {
			// 记录事件
			g.tracer.AddEvent(span, "invalid_token", map[string]string{
				"route": msg.Route,
				"error": err.Error(),
			})

			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Invalid token", err.Error(), "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
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

	// 路由处理
	g.routeManager.HandleRoute(msg.ConnectionID, msg.Route, msg.Payload, func(response interface{}) {
		// 记录事件
		g.tracer.AddEvent(span, "route_handler_end", map[string]string{
			"route": msg.Route,
		})

		// 处理响应
		defer func() {
			if r := recover(); r != nil {
				// 记录事件
				g.tracer.AddEvent(span, "response_processing_panic", map[string]string{
					"error": fmt.Sprintf("%v", r),
				})

				// 收集处理失败的消息指标
				g.metrics.IncMessagesFailed()
				// 输出错误日志
				tlog.Error("处理响应异常",
					"connectionID", msg.ConnectionID,
					"route", msg.Route,
					"error", r)

				// 发送错误响应
				errorMsg := NewErrorMessage("error", "Internal server error", fmt.Sprintf("%v", r), "")
				responseData, _ := proto.Marshal(errorMsg)
				msg.Conn.Write(responseData)
			}
		}()

		// 检查是否是HTTP连接
		// 这里简化处理，实际应该根据连接类型判断
		// 假设HTTP连接的route包含点号（因为我们将/替换为.）
		isHTTP := false
		for _, c := range msg.Route {
			if c == '.' {
				isHTTP = true
				break
			}
		}

		// 检查连接上下文，判断是否是HTTP请求
		connCtx := msg.Conn.Context()
		if _, ok := connCtx.(*GnetConnContext); ok {
			// 检查连接的端口，判断是否是HTTP请求
			port := msg.Conn.LocalAddr().String()
			for i := len(port) - 1; i >= 0; i-- {
				if port[i] == ':' {
					port = port[i+1:]
					break
				}
			}
			// 获取传输类型
			transportType := g.transportType[port]
			// 如果传输类型不是websocket，默认认为是HTTP请求
			if transportType != "websocket" {
				// 检查route是否包含点号，如果不包含，说明不是HTTP请求
				if !isHTTP {
					isHTTP = false
				} else {
					isHTTP = true
				}
			}
		}

		var responseData []byte
		var err error

		if isHTTP {
			// 生成HTTP响应
			if responseMap, ok := response.(map[string]interface{}); ok {
				responseData = generateHTTPResponse(responseMap)
			} else {
				// 发送错误响应
				errorMsg := NewErrorMessage("error", "Invalid response format", "", "")
				responseData, _ = proto.Marshal(errorMsg)
				msg.Conn.Write(responseData)
				return
			}
		} else {
			// 生成Protocol Buffers响应
			if responseMsg, ok := response.(*protobuf.Message); ok {
				// 添加消息完整性校验
				g.messageIntegrity.PrepareMessage(responseMsg)
				responseData, err = proto.Marshal(responseMsg)
			} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
				// 添加消息完整性校验
				g.messageIntegrity.PrepareErrorResponse(errorMsg)
				responseData, err = proto.Marshal(errorMsg)
			} else {
				// 发送错误响应
				errorMsg := NewErrorMessage("error", "Invalid response format", "", "")
				responseData, _ = proto.Marshal(errorMsg)
				msg.Conn.Write(responseData)
				return
			}

			if err != nil {
				// 发送错误响应
				errorMsg := NewErrorMessage("error", "Failed to marshal response", err.Error(), "")
				responseData, _ = proto.Marshal(errorMsg)
				msg.Conn.Write(responseData)
				return
			}
		}

		// 发送响应
		if _, err := msg.Conn.Write(responseData); err != nil {
			// 记录事件
			g.tracer.AddEvent(span, "send_response_error", map[string]string{
				"error": err.Error(),
			})
			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("发送响应失败", "error", err, "connectionID", msg.ConnectionID)
			return
		}

		// 增加发送字节数指标
		g.metrics.AddBytesSent(int64(len(responseData)))

		// 收集处理成功的消息指标
		g.metrics.IncMessagesProcessed()

		// 记录事件
		g.tracer.AddEvent(span, "response_sent", map[string]string{
			"bytes": fmt.Sprintf("%d", len(responseData)),
		})

		// 记录处理时间
		g.metrics.AddProcessingTime(time.Since(start))
	})
}

// OnBoot 服务器启动时的回调
func (g *GatewayGnet) OnBoot(engine gnet.Engine) (action gnet.Action) {
	// 服务器启动时的初始化工作
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

// registerVersionRoute 注册版本路由
func (g *GatewayGnet) registerVersionRoute() {
	g.routeManager.RegisterRoute("version", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("version", map[string]string{
			"version":   g.GetVersion(),
			"clusterID": g.clusterID,
			"isLeader":  fmt.Sprintf("%t", g.isLeader),
		}))
	})
}

// registerPingRoute 注册ping路由
func (g *GatewayGnet) registerPingRoute() {
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{})) {
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
	// 发送停止信号
	close(g.stopChan)
	
	// 等待工作线程结束
	g.workerPool.Wait()
	
	// 清理资源
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
	authRoutes, ok := g.authRoutes.Load().(map[string]bool)
	if !ok {
		return false
	}
	return authRoutes[route]
}

// Start 启动网关
func (g *GatewayGnet) Start(addr string) error {
	// 配置gnet选项
	tcpKeepaliveTime := 30 * time.Second
	options := []gnet.Option{
		gnet.WithMulticore(true),
		gnet.WithReusePort(true),
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithTCPKeepAlive(tcpKeepaliveTime),
		gnet.WithNumEventLoop(runtime.GOMAXPROCS(0)),
		gnet.WithReadBufferCap(4096),
		gnet.WithWriteBufferCap(4096),
		gnet.WithSocketRecvBuffer(8192),
		gnet.WithSocketSendBuffer(8192),
		gnet.WithLogLevel(logging.InfoLevel),
		gnet.WithLogger(g),
	}

	// 启动服务器
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
