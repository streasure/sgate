package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"github.com/streasure/sgate/gateway/protobuf"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"google.golang.org/protobuf/proto"
	tlog "github.com/streasure/treasure-slog"
)

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
}

// messagePool 消息对象池
// 功能: 复用消息对象，减少内存分配
var messagePool = sync.Pool{
	New: func() interface{} {
		return &Message{
			Payload: make(map[string]string),
		}
	},
}

// GetMessage 从对象池获取消息对象
// 功能: 从消息对象池获取一个消息对象，并清空其内容
// 返回值:
//
//	*Message: 消息对象
func GetMessage() *Message {
	msg := messagePool.Get().(*Message)
	// 清空消息对象
	msg.ConnectionID = ""
	msg.Route = ""
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
	// 清空消息对象
	msg.ConnectionID = ""
	msg.Route = ""
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
	connectionManager  *ConnectionManager  // 连接管理器
	routeManager       *RouteManager       // 路由管理器
	messagePool        chan *Message       // 消息队列
	workerPool         sync.WaitGroup      // 工作池
	stopChan           chan struct{}       // 停止信号通道
	workerStopChan     chan struct{}       // 工作线程停止信号通道
	metrics            *metrics.Metrics    // 指标收集器
	transportType      map[string]string   // 端口到传输类型的映射
	rateLimiter        *RateLimiter        // 速率限制器
	authSecret         string              // 认证密钥
	authRoutes         map[string]bool     // 需要认证的路由
	ctx                context.Context     // 上下文
	tlsConfig          *tls.Config         // TLS配置
	clusterID          string              // 集群ID
	isLeader           bool                // 是否是领导者
	bufferPool         sync.Pool           // 缓冲区池
	whitelistBlacklist *WhitelistBlacklist // 白名单和黑名单管理器
	workerMutex        sync.Mutex          // 工作池互斥锁
	workerCount        int                 // 当前工作线程数
	minWorkers         int                 // 最小工作线程数
	maxWorkers         int                 // 最大工作线程数
	workerQueueSize    int                 // 工作队列大小阈值
	cfg                *config.Config      // 配置实例
	wsConnections      sync.Map            // 活跃的WebSocket连接
	configPath         string              // 配置文件路径
	configUpdateChan   chan *config.Config // 配置更新通道
	cache              *Cache              // 缓存管理器
	loadBalancer       *LoadBalancer       // 负载均衡器
}

// NewGateway 创建网关实例
// 返回值:
//
//	*Gateway: 网关实例
func NewGateway() *Gateway {
	ctx := context.Background()

	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("加载配置失败，使用默认配置", "error", err)
	}

	// 初始化白名单和黑名单管理器
	whitelistBlacklist := NewWhitelistBlacklist()

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

	gw := &Gateway{
		connectionManager: NewConnectionManager(), // 创建连接管理器
		routeManager:      NewRouteManager(),                 // 创建路由管理器
		messagePool:       make(chan *Message, queueSize),    // 创建消息队列
		stopChan:          make(chan struct{}),               // 创建停止信号通道
		workerStopChan:    make(chan struct{}),               // 创建工作线程停止信号通道
		metrics:           metrics.NewMetrics(),              // 创建指标收集器
		transportType:     make(map[string]string),           // 创建端口到传输类型的映射
		rateLimiter:       NewRateLimiter(rateLimitRate, rateLimitWindow), // 创建速率限制器
		authSecret:        authSecret,                        // 认证密钥
		authRoutes:        authRoutes,                        // 需要认证的路由
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
		whitelistBlacklist: whitelistBlacklist, // 白名单和黑名单管理器
		workerCount:        0,                  // 当前工作线程数
		minWorkers:         minWorkers,         // 最小工作线程数
		maxWorkers:         maxWorkers,         // 最大工作线程数
		workerQueueSize:    workerQueueSize,    // 工作队列大小阈值
		cfg:                cfg,                // 配置实例
		configPath:         "config/config.yaml", // 配置文件路径
		configUpdateChan:   make(chan *config.Config), // 配置更新通道
		cache:              NewCache(),               // 缓存管理器
		loadBalancer:       NewLoadBalancer(),        // 负载均衡器
	}

	// 启动速率限制器
	gw.rateLimiter.Start()

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
func (g *Gateway) addWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount >= g.maxWorkers {
		return
	}

	g.workerCount++
	g.workerPool.Add(1)
	go g.messageWorker()
}

// removeWorker 移除工作线程
func (g *Gateway) removeWorker() {
	g.workerMutex.Lock()
	defer g.workerMutex.Unlock()

	if g.workerCount <= g.minWorkers {
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
func (g *Gateway) workerPoolManager() {
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
			loadFactor := float64(queueLength) / float64(g.workerQueueSize)
			connectionFactor := float64(activeConnections) / float64(500) // 每500个连接增加一个线程
			timeFactor := averageProcessingTime / float64(5) // 处理时间超过5ms增加线程
			
			// 综合负载指标
			totalLoad := loadFactor + connectionFactor + timeFactor
			
			// 根据综合负载动态调整工作线程数
			if totalLoad > 0.8 && g.workerCount < g.maxWorkers {
				// 负载较高，添加工作线程
				// 一次添加多个线程，根据负载程度
				addCount := int(totalLoad * 2) // 增加更多线程以快速响应负载
				if addCount > 20 {
					addCount = 20 // 最多一次添加20个线程
				}
				for i := 0; i < addCount && g.workerCount < g.maxWorkers; i++ {
					g.addWorker()
				}
				// 更新工作线程数指标
				g.metrics.SetWorkerCount(int64(g.workerCount))
			} else if totalLoad < 0.2 && g.workerCount > g.minWorkers {
				// 负载较低，移除工作线程
				// 一次移除多个线程，快速减少空闲线程
				removeCount := g.workerCount - g.minWorkers
				if removeCount > 5 {
					removeCount = 5 // 最多一次移除5个线程
				}
				for i := 0; i < removeCount && g.workerCount > g.minWorkers; i++ {
					g.removeWorker()
				}
				// 更新工作线程数指标
				g.metrics.SetWorkerCount(int64(g.workerCount))
			}
		}
	}
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
	// 更新配置
	g.cfg = newCfg

	// 更新认证路由
	authRoutes := make(map[string]bool)
	for _, route := range newCfg.Security.AuthRoutes {
		authRoutes[route] = true
	}
	g.authRoutes = authRoutes

	// 更新认证密钥
	g.authSecret = newCfg.Security.AuthSecret

	// 更新工作池配置
	g.minWorkers = newCfg.WorkerPool.MinWorkers
	g.maxWorkers = newCfg.WorkerPool.MaxWorkers
	g.workerQueueSize = newCfg.WorkerPool.QueueSizeThreshold

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

	tlog.Info("配置更新完成")
}

// messageWorker 消息处理工作线程
func (g *Gateway) messageWorker() {
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
func (g *Gateway) registerDefaultRoutes() {
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

// ConnContext 连接上下文结构
type ConnContext struct {
	ConnectionID string
	Buffer       []byte
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
	// 分配缓冲区并设置连接上下文
	connCtx := &ConnContext{
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

	// 检查速率限制
	if !g.rateLimiter.Allow(clientIP) {
		// 输出警告日志
		tlog.Warn("请求被限流", "clientIP", clientIP)
		// 发送限流响应
		errorMsg := NewErrorMessage("error", "Rate limit exceeded", "", "")
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

// isHTTPRequest 检查是否是HTTP请求
// 参数:
//
//	data: 数据
//
// 返回值:
//
//	bool: 是否是HTTP请求
func isHTTPRequest(data []byte) bool {
	// 检查是否以HTTP方法开头
	httpMethods := []string{"GET ", "POST ", "PUT ", "DELETE ", "PATCH ", "HEAD ", "OPTIONS ", "CONNECT ", "TRACE "}
	for _, method := range httpMethods {
		if len(data) >= len(method) && string(data[:len(method)]) == method {
			return true
		}
	}
	return false
}

// handleHTTPRequest 处理HTTP请求
// 参数:
//
//	c: 网络连接
//	data: 数据
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) handleHTTPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
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
	if ctx, ok := connCtx.(*ConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		// 设置连接上下文
		c.SetContext(&ConnContext{
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
// 参数:
//
//	c: 网络连接
//	data: 数据
//
// 返回值:
//
//	gnet.Action: 操作类型
func (g *Gateway) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
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

	// 处理消息
	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*ConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		// 设置连接上下文
		c.SetContext(&ConnContext{
			ConnectionID: connectionID,
			Buffer:       make([]byte, 4096), // 4KB 缓冲区
		})
		tlog.Info("生成新的连接ID", "connectionID", connectionID, "userUUID", tempUserUUID)
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

// extractRouteFromHTTP 从HTTP请求中提取路由
// 参数:
//
//	data: 数据
//
// 返回值:
//
//	string: 路由
func extractRouteFromHTTP(data []byte) string {
	// 简化处理，提取路径部分
	// 格式: METHOD /path HTTP/1.1
	lines := splitBytes(data, []byte{'\n'})
	if len(lines) > 0 {
		firstLine := lines[0]
		parts := splitBytes(firstLine, []byte{' '})
		if len(parts) >= 2 {
			path := string(parts[1])
			// 移除查询参数
			if idx := indexOf(path, '?'); idx != -1 {
				path = path[:idx]
			}
			// 移除开头的/，并将/替换为.
			path = trimPrefix(path, '/')
			path = replaceAll(path, '/', '.')
			if path == "" {
				return "index"
			}
			return path
		}
	}
	return "index"
}

// extractPayloadFromHTTP 从HTTP请求中提取payload
// 参数:
//
//	data: 数据
//
// 返回值:
//
//	map[string]string: payload
func extractPayloadFromHTTP(data []byte) map[string]string {
	// 简化处理，提取请求体
	payload := make(map[string]string)

	// 查找空行，分隔请求头和请求体
	emptyLineIndex := indexOfBytes(data, []byte{'\r', '\n', '\r', '\n'})
	// 如果没有找到 \r\n\r\n，尝试查找 \n\n
	if emptyLineIndex == -1 {
		emptyLineIndex = indexOfBytes(data, []byte{'\n', '\n'})
		if emptyLineIndex != -1 {
			body := data[emptyLineIndex+2:]
			// 尝试解析Protocol Buffers
			message := &protobuf.Message{}
			if err := proto.Unmarshal(body, message); err != nil {
				// 如果不是Protocol Buffers，作为原始数据处理
				payload["raw"] = string(body)
			} else {
				// 提取payload
				if message.Payload != nil {
					for k, v := range message.Payload {
						payload[k] = v
					}
				}
				// 提取route和user_uuid
				if message.Route != "" {
					payload["route"] = message.Route
				}
				if message.UserUuid != "" {
					payload["user_uuid"] = message.UserUuid
				}
			}
		}
	} else {
		body := data[emptyLineIndex+4:]
		// 尝试解析Protocol Buffers
		message := &protobuf.Message{}
		if err := proto.Unmarshal(body, message); err != nil {
			// 如果不是Protocol Buffers，作为原始数据处理
			payload["raw"] = string(body)
		} else {
			// 提取payload
			if message.Payload != nil {
				for k, v := range message.Payload {
					payload[k] = v
				}
			}
			// 提取route和user_uuid
			if message.Route != "" {
				payload["route"] = message.Route
			}
			if message.UserUuid != "" {
				payload["user_uuid"] = message.UserUuid
			}
		}
	}

	return payload
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
	// 服务器启动时的初始化工作
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

// registerVersionRoute 注册版本路由
func (g *Gateway) registerVersionRoute() {
	g.routeManager.RegisterRoute("version", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("version", map[string]string{
			"version":   g.GetVersion(),
			"clusterID": g.clusterID,
			"isLeader":  fmt.Sprintf("%t", g.isLeader),
		}))
	})
}

// registerPingRoute 注册ping路由
func (g *Gateway) registerPingRoute() {
	g.routeManager.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{})) {
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

	// 处理路由
	defer func() {
		if r := recover(); r != nil {
			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("处理消息异常",
				"connectionID", msg.ConnectionID,
				"route", msg.Route,
				"error", r)
		}
		// 归还消息对象到对象池
		PutMessage(msg)
	}()

	// 检查路由是否存在
	hasRoute := g.routeManager.HasRoute(msg.Route)
	if !hasRoute {
		// 路由不存在，发送错误响应
		errorMsg := NewErrorMessage("error", "Route not found", "", "")
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
			// 输入验证失败，发送错误响应
			errorMsg := NewErrorMessage("error", "Invalid input detected", "", "")
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
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Missing token", "", "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
			return
		}

		// 验证token
		claims, err := ValidateToken(token, g.authSecret)
		if err != nil {
			// 发送未授权响应
			errorMsg := NewErrorMessage("error", "Invalid token", err.Error(), "")
			responseData, _ := proto.Marshal(errorMsg)
			msg.Conn.Write(responseData)
			return
		}

		// 将用户信息添加到payload中
		msg.Payload = addUserInfoToPayloadMap(msg.Payload, claims.UserID, claims.Role)
	}

	g.routeManager.HandleRoute(msg.ConnectionID, msg.Route, msg.Payload, func(response interface{}) {
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
		if _, ok := connCtx.(*ConnContext); ok {
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
			} else if protoMsg, ok := response.(*protobuf.Message); ok {
				// 处理Protocol Buffers消息
				responseData, err = proto.Marshal(protoMsg)
				if err != nil {
					// 收集处理失败的消息指标
					g.metrics.IncMessagesFailed()
					// 输出错误日志
					tlog.Error("序列化响应失败", "error", err)
					return
				}
			} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
				// 处理Protocol Buffers错误消息
				responseData, err = proto.Marshal(errorMsg)
				if err != nil {
					// 收集处理失败的消息指标
					g.metrics.IncMessagesFailed()
					// 输出错误日志
					tlog.Error("序列化响应失败", "error", err)
					return
				}
			} else {
				// 转换为Protocol Buffers消息
				protoMsg := NewResponseMessage("response", response.(map[string]string))
				responseData, err = proto.Marshal(protoMsg)
				if err != nil {
					// 收集处理失败的消息指标
					g.metrics.IncMessagesFailed()
					// 输出错误日志
					tlog.Error("序列化响应失败", "error", err)
					return
				}
			}
		} else {
			// 生成Protocol Buffers响应
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
				// 收集处理失败的消息指标
				g.metrics.IncMessagesFailed()
				// 输出错误日志
				tlog.Error("序列化响应失败", "error", err)
				return
			}
		}

		// 检查是否是WebSocket连接
		// 从连接上下文获取WebSocket连接
		isWebSocket := false
		if wsConn, ok := msg.Conn.Context().(*WebSocketConnection); ok && atomic.LoadInt32(&wsConn.State) == int32(StateOpen) {
			isWebSocket = true
		}

		if isWebSocket {
			// 封装为WebSocket消息
			wsConn := msg.Conn.Context().(*WebSocketConnection)
			if err := g.sendWebSocketMessage(wsConn, ws.OpText, responseData); err != nil {
				// 收集处理失败的消息指标
				g.metrics.IncMessagesFailed()
				// 输出错误日志
				tlog.Error("发送WebSocket响应失败",
					"connectionID", msg.ConnectionID,
					"error", err)
				return
			}
			// 收集处理成功的消息指标
			g.metrics.IncMessagesProcessed()

			// 计算处理时间
			duration := time.Since(start)
			g.metrics.AddProcessingTime(duration)
			return
		}

		if _, err := msg.Conn.Write(responseData); err != nil {
			// 收集处理失败的消息指标
			g.metrics.IncMessagesFailed()
			// 输出错误日志
			tlog.Error("发送响应失败",
				"connectionID", msg.ConnectionID,
				"error", err)
			return
		}

		// 收集处理成功的消息指标
		g.metrics.IncMessagesProcessed()

		// 计算处理时间
		duration := time.Since(start)
		g.metrics.AddProcessingTime(duration)

	})
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
	return g.authRoutes[route]
}

// 预分配的HTTP响应缓冲区
var httpResponseBuffer = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

// generateHTTPResponse 生成HTTP响应
func generateHTTPResponse(response map[string]interface{}) []byte {
	// 从对象池获取缓冲区
	buf := httpResponseBuffer.Get().(*bytes.Buffer)
	buf.Reset()
	defer httpResponseBuffer.Put(buf)

	// 提取状态码
	statusCode := 200
	if code, ok := response["status"].(int); ok {
		statusCode = code
	}

	// 提取消息
	message := "OK"
	if msg, ok := response["message"].(string); ok {
		message = msg
	}

	// 提取数据
	data := response["data"]
	if data == nil {
		data = response
	}

	// 转换为map[string]string
	dataMap := make(map[string]string)
	if dataMapInterface, ok := data.(map[string]interface{}); ok {
		for k, v := range dataMapInterface {
			if vStr, ok := v.(string); ok {
				dataMap[k] = vStr
			} else {
				dataMap[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	// 序列化数据为Protocol Buffers
	protoMsg := &protobuf.Message{
		Route:   "response",
		Payload: dataMap,
	}
	dataBytes, err := proto.Marshal(protoMsg)
	if err != nil {
		dataBytes = []byte(`{"error": "Failed to serialize response"}`)
	}

	// 生成HTTP响应
	fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\n", statusCode, message)
	buf.WriteString("Content-Type: application/octet-stream\r\n")
	fmt.Fprintf(buf, "Content-Length: %d\r\n", len(dataBytes))
	buf.WriteString("Connection: close\r\n")
	buf.WriteString("\r\n")
	buf.Write(dataBytes)

	// 返回响应字节
	return buf.Bytes()
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
	// 关闭消息处理工作池
	close(g.stopChan)
	g.workerPool.Wait()

	// 关闭所有连接
	g.connectionManager.CloseAllConnections()
	// 输出信息日志
	tlog.Info("所有连接已关闭")
}
