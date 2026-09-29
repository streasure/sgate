package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/backend"

	"github.com/streasure/sgate/internal/connection"

	"github.com/panjf2000/gnet/v2"
	protoLogin "github.com/streasure/protocol/loginserver"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/prometheus"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"github.com/streasure/util/ugrpc"
	"google.golang.org/grpc"
)

// Gateway 表示网关实例，负责连接接入、协议处理、逻辑转发和组件生命周期。
type Gateway struct {
	connectionManager *connection.ConnectionManager
	stopChan          chan struct{}
	closeOnce         sync.Once
	transportType     sync.Map
	ctx               context.Context
	tlsConfig         *tls.Config
	clusterID         string
	gatewayID         string
	isLeader          bool
	cfg               atomic.Value
	wsConnections     sync.Map
	configPath        string
	configUpdateChan  chan *config.Config
	messageIntegrity  *MessageIntegrity
	tracer            *obs.Tracer
	logicClient       *backend.LogicClient
	logicClientPool   *backend.LogicClientPool
	gatewayClientPool *backend.GatewayClientPool
	serverID          string
	serviceDiscovery  *uetcd.Component
	gatewayDiscovery  *uetcd.Component // 发现其他网关（Gateway:{zone}）
	gatewayEvents     []uetcd.ServiceEvent
	overloadProtector *OverloadProtector
	grpcServer        *ugrpc.Server
	promExporter      *prometheus.Exporter // Prometheus 指标导出器（enabled=false 时为空）。
	statsServer       *http.Server
	msgRate           *messageRateTracker // 消息速率滚动窗口（供 Stats() 计算 msgs/sec）
	zone              string
	protection        atomic.Value // stores config.ProtectionConfig — 热路径无锁读取
	grpcCfg           atomic.Value // stores config.GRPCConfig（热更新原子替换）
	streamCfg         atomic.Value // stores config.StreamConfig（热更新原子替换）
	// 安全防护组件
	whitelistBlacklist *security.WhitelistBlacklist
	circuitBreakerMgr  *security.CircuitBreakerManager
	rateLimiter        *security.RateLimiter
	waf                *security.WAF
	cluster            *cluster.Cluster
	latencyTracker     *obs.LatencyTracker
	engines            []gnet.Engine // 所有 transport engine，关闭时逐个 Stop
	enginesMu          sync.Mutex
	grpcServerMu       sync.Mutex // 保护 grpcServer 赋值/读取（启动协程与 Close 竞态）

	// 企业级扩展组件
	filterChain   *types.FilterChain          // SPI 过滤器链
	jwtAuth       *security.JWTAuthFilter     // JWT 鉴权
	balancer      *cluster.Balancer           // 负载均衡 + 故障节点摘除
	degradation   *traffic.DegradationManager // 降级管理
	configCenter  cluster.ConfigCenter        // etcd 配置中心
	otelTracer    *obs.OTelTracer             // 分布式追踪导出
	alertWebhook  *cluster.AlertWebhook       // 告警 webhook（企业微信/钉钉）
	canaryFilter  *traffic.CanaryFilter       // 灰度发布
	trafficMirror *traffic.TrafficMirror      // 流量镜像
	logSanitizer  *obs.LogSanitizer           // 日志脱敏

	// 转发统计计数器（用于极限压测时观测 sgate 转发能力）
	pipeline                           *MessagePipeline
	loginSlots                         chan struct{} // 并发登录协程上限（finishLoginGate）
	connectionsTotal                   atomic.Int64
	connectionsActive                  atomic.Int64
	messagesForwarded                  atomic.Int64
	messagesDroppedOverload            atomic.Int64
	messagesDroppedFull                atomic.Int64
	messagesDroppedNoLogic             atomic.Int64
	messagesDroppedNoLogicNotConnected atomic.Int64
	messagesReceived                   atomic.Int64
	messagesPushedToClient             atomic.Int64
	messagesPushDroppedNoConn          atomic.Int64
	messagesProcessed                  atomic.Int64
	messagesFailed                     atomic.Int64
	// 细分丢弃原因（与过载保护区分，便于排障）
	messagesDroppedBlacklist   atomic.Int64 // 黑名单/白名单拦截
	messagesDroppedRateLimit   atomic.Int64 // 限流拦截
	messagesDroppedWAF         atomic.Int64 // WAF 拦截
	messagesDroppedCircuit     atomic.Int64 // 熔断器拦截
	messagesDroppedIntegrity   atomic.Int64 // 完整性校验失败
	messagesDroppedFilterChain atomic.Int64 // filter chain 中止
	messagesDroppedAuth        atomic.Int64 // 认证拦截（缺少 serverID 或 userUUID）

	// 连接生命周期指标
	connectionDurationSum     atomic.Int64        // 连接总存活时长（毫秒），用于计算平均值
	connectionDurationCount   atomic.Int64        // 已关闭连接数，用于计算平均值
	connectionDurationTracker *obs.LatencyTracker // 连接时长分位数追踪器
	loginServerMu             sync.RWMutex
	loginServerClient         protoLogin.LoginServiceClient
	loginServerConn           *grpc.ClientConn
	loginServerAddr           string
	pipelineWorkerPool        *PipelineWorkerPool               // 异步 pipeline 工作池
	shardedCoalescer          *connection.ShardedWriteCoalescer // 分片写合并器（推送路径）
}

// SetTransportType 设置监听端口对应的传输类型。
func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType.Store(port, transportType)
}

// getProtection 无锁读取 ProtectionConfig（热路径使用）。
func (g *Gateway) getProtection() config.ProtectionConfig {
	return g.protection.Load().(config.ProtectionConfig)
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：逻辑服到网关再到客户端）。
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

// NewGateway 创建网关实例。配置从 config.Get() 读取（main 必须先 config.Load），
// 运行时资源由各组件在 Init/Start 中写入 package 级全局变量后读取。
func NewGateway() *Gateway {
	cfg := config.Get()
	if cfg == nil {
		// fail-fast：main 已保证 Load 成功；半初始化 Gateway 会在 Start 阶段 panic，更难排查
		panic("gateway: config not loaded, call config.Load first")
	}
	return newGateway(cfg)
}

// buildTLSConfig 根据配置构建 TLS 配置；证书加载失败时保持无证书（仅记录错误）。
func buildTLSConfig(cfg *config.Config) *tls.Config {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
		return tlsConfig
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		tlog.Error(context.TODO(), "failed to load TLS certificate error=%v", err)
		return tlsConfig
	}
	tlsConfig.Certificates = []tls.Certificate{cert}
	if strings.EqualFold(cfg.TLS.MinVersion, "TLS1.3") {
		tlsConfig.MinVersion = tls.VersionTLS13
	}
	return tlsConfig
}

func (g *Gateway) Name() string { return "gateway" }
func (g *Gateway) Order() int   { return 1000 }

func (g *Gateway) Init() error {
	// 前置组件已完成 Init，此处统一读取其写入的全局资源。
	g.filterChain = types.GetFilterChain()
	g.logSanitizer = component.LogSanitizer()
	g.whitelistBlacklist = component.WhitelistBlacklist()
	g.waf = component.WAF()
	g.rateLimiter = component.RateLimiter()
	g.jwtAuth = component.JWTAuth()
	g.circuitBreakerMgr = component.CircuitBreakerMgr()
	g.tracer = component.Tracer()
	g.otelTracer = component.OTelTracer()
	g.latencyTracker = component.LatencyTracker()
	g.canaryFilter = component.CanaryFilter()
	g.trafficMirror = component.TrafficMirror()
	g.degradation = component.Degradation()
	g.serviceDiscovery = component.Discovery()
	g.gatewayDiscovery = component.GatewayDiscovery()
	g.gatewayEvents = component.GatewayEvents()
	g.balancer = component.Balancer()
	g.configCenter = component.ConfigCenter()
	g.cluster = component.ClusterNode()
	g.alertWebhook = component.AlertWebhook()
	return nil
}
func (g *Gateway) Start() error {
	// ClusterComponent 在 Start 中创建 discovery，需在其后重新读取。
	g.serviceDiscovery = component.Discovery()
	g.gatewayDiscovery = component.GatewayDiscovery()
	g.gatewayEvents = component.GatewayEvents()
	g.balancer = component.Balancer()
	g.configCenter = component.ConfigCenter()
	g.cluster = component.ClusterNode()
	g.alertWebhook = component.AlertWebhook()
	g.setLoginServerDiscovery(component.LoginDiscovery())
	g.StartServices()
	return nil
}

func (g *Gateway) Destroy() { g.Close() }

// StartServices 启动网关特定服务：gRPC服务器、统计HTTP服务、
// Prometheus监控指标、过载保护器、WebSocket心跳检测、配置文件监听
func (g *Gateway) StartServices() {
	cfg := g.cfg.Load().(*config.Config)

	g.overloadProtector.Start()
	go g.wsHeartbeatChecker()
	g.messageIntegrity = NewMessageIntegrity(30000)

	if cfg.Pipeline.AsyncEnabled {
		g.pipelineWorkerPool = NewPipelineWorkerPool(g, cfg.Pipeline)
	}

	// 初始化分片写合并器（推送路径：logic → client）
	g.shardedCoalescer = connection.NewShardedWriteCoalescer(g.connectionManager, 16)

	connCheckInterval, _ := time.ParseDuration(g.getProtection().ConnCheckInterval)
	if connCheckInterval <= 0 {
		connCheckInterval = 5 * time.Minute
	}
	connIdleTimeout, _ := time.ParseDuration(g.getProtection().ConnIdleTimeout)
	if connIdleTimeout <= 0 {
		connIdleTimeout = 30 * time.Second
	}
	g.connectionManager.StartConnectionChecker(connIdleTimeout, connCheckInterval)

	if g.configCenter != nil {
		g.startConfigCenterWatcher()
	}
	go g.configWatcher()
	go func() {
		for {
			select {
			case <-g.stopChan:
				return
			case newCfg := <-g.configUpdateChan:
				g.handleConfigUpdate(newCfg)
			}
		}
	}()

	g.logicClient.SetGateway(g)
	g.logicClientPool = backend.NewLogicClientPool(g)

	if g.serviceDiscovery != nil {
		g.logicClientPool.SetDiscovery(g.serviceDiscovery)
	}
	if g.balancer != nil {
		g.logicClientPool.SetBalancer(g.balancer)
		g.balancer.SetHealthCheckFunc(func(id, addr string) bool {
			return g.logicClientPool.IsClientConnected(id)
		})
	}

	// 网关到网关客户端池仅在集群模式下创建
	if g.gatewayDiscovery != nil {
		g.gatewayClientPool = backend.NewGatewayClientPool(g)
		g.gatewayClientPool.SetDiscovery(g.gatewayDiscovery)
		g.gatewayClientPool.LoadEvents(g.gatewayEvents)
	}

	// gRPC服务器
	grpcCfg := g.GetGRPCConfig()
	grpcPort := fmt.Sprintf(":%d", grpcCfg.Port)
	tlog.Info(context.TODO(), "starting gRPC server port=%s", grpcPort)
	go func() {
		if server, err := backend.StartGRPCServer(g, grpcPort, grpcCfg.MaxMessageSize, grpcCfg.WindowSize); err != nil {
			tlog.Error(context.TODO(), "failed to start gRPC server error=%v", err)
		} else {
			g.grpcServerMu.Lock()
			g.grpcServer = server
			g.grpcServerMu.Unlock()
			tlog.Info(context.TODO(), "gRPC server started port=%s", grpcPort)
		}
	}()

	// 统计HTTP服务器
	g.StartStatsServer(fmt.Sprintf(":%d", cfg.HttpPort))

	// 启动TCP/WebSocket传输层
	g.startTransports(cfg)

	// Prometheus监控指标导出
	if cfg.Monitoring.Prometheus.Enabled {
		g.promExporter = prometheus.NewExporter(prometheus.ExporterConfig{
			Enabled: true,
			Addr:    cfg.Monitoring.Prometheus.Addr,
			Path:    cfg.Monitoring.Prometheus.Path,
			Prefix:  cfg.Monitoring.Prometheus.Prefix,
		}, g)
		g.promExporter.Init()
		g.promExporter.Start()
	}
}

func (g *Gateway) startTransports(cfg *config.Config) {
	for _, transport := range cfg.Transports {
		port := transport.Port
		transportType := transport.Type
		g.SetTransportType(fmt.Sprintf("%d", port), transportType)

		addr := fmt.Sprintf("tcp://:%d", port)
		options := []gnet.Option{
			gnet.WithMulticore(true),
			gnet.WithReusePort(true),
			gnet.WithReadBufferCap(262144),
			gnet.WithWriteBufferCap(262144),
			gnet.WithSocketRecvBuffer(4 * 1024 * 1024),
			gnet.WithSocketSendBuffer(4 * 1024 * 1024),
		}
		if transportType == "" || transportType == "websocket" {
			options = append(options, gnet.WithTCPNoDelay(gnet.TCPNoDelay))
		}
		tlog.Info(context.TODO(), "starting gateway transport addr=%s type=%s", addr, transportType)
		go func(addr, transportType string) {
			if err := gnet.Run(g, addr, options...); err != nil {
				tlog.Error(context.TODO(), "gateway transport stopped addr=%s error=%v", addr, err)
			}
		}(addr, transportType)
	}
}

func (g *Gateway) wsHeartbeatChecker() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "wsHeartbeatChecker panic recovered error=%v", r)
		}
	}()
	checkInterval := time.Duration(g.getProtection().WSCheckInterval) * time.Second
	heartbeatTimeout := time.Duration(g.getProtection().WSHeartbeatTimeout) * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			g.checkWebSocketConnections(heartbeatTimeout)
		}
	}
}

func (g *Gateway) checkWebSocketConnections(timeout time.Duration) {
	g.wsConnections.Range(func(key, value any) bool {
		conn, ok := key.(*WebSocketConnection)
		if !ok {
			return true
		}
		if time.Since(conn.getLastPingTime()) > timeout {
			tlog.Warn(context.TODO(), "WebSocket connection timeout, closing connectionID=%s", conn.ConnectionID())
			if conn.Conn != nil {
				conn.Conn.Close()
			}
			if conn.ConnectionID() != "" {
				g.connectionManager.RemoveConnection(conn.ConnectionID())
			}
			g.wsConnections.Delete(conn)
		}
		return true
	})
}

// tryEnqueueConfig 非阻塞投递配置更新：通道满时丢弃最旧一项，保证始终保留最新配置。
func (g *Gateway) tryEnqueueConfig(newCfg *config.Config) {
	select {
	case g.configUpdateChan <- newCfg:
	default:
		select {
		case <-g.configUpdateChan: // drop oldest pending
		default:
		}
		select {
		case g.configUpdateChan <- newCfg:
		default:
		}
	}
}

func (g *Gateway) configWatcher() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "configWatcher panic recovered error=%v", r)
		}
	}()
	// 优先使用 config.Load 记录的真实路径，避免 -conf 与默认路径不一致导致热更新失效
	if p := config.Path(); p != "" {
		g.configPath = p
	}
	if g.configPath == "" {
		g.configPath = "config/config.yaml"
	}
	if _, err := os.Stat(g.configPath); os.IsNotExist(err) {
		altPaths := []string{"config/config.yaml", "../config/config.yaml", "../../config/config.yaml"}
		found := false
		for _, path := range altPaths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				g.configPath = path
				found = true
				break
			}
		}
		if !found {
			return
		}
	}

	fileInfo, err := os.Stat(g.configPath)
	if err != nil {
		return
	}

	lastModTime := fileInfo.ModTime()

	for {
		select {
		case <-g.stopChan:
			return
		default:
			fileInfo, err := os.Stat(g.configPath)
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}

			if fileInfo.ModTime() != lastModTime {
				lastModTime = fileInfo.ModTime()
				newCfg, err := config.Load(g.configPath)
				if err != nil {
					tlog.Error(context.TODO(), "config reload failed path=%s error=%v", g.configPath, err)
					time.Sleep(5 * time.Second)
					continue
				}
				g.tryEnqueueConfig(newCfg)
			}

			time.Sleep(5 * time.Second)
		}
	}
}

func (g *Gateway) handleConfigUpdate(newCfg *config.Config) {
	// 先补齐运行时默认值，再原子替换（防止零值热更把连接踢飞）
	newCfg.ApplyRuntimeDefaults()
	g.cfg.Store(newCfg)

	// 动态更新限流阈值（无需重启）
	if g.rateLimiter != nil && newCfg.Security.RateLimit.Enabled {
		refresh := time.Second
		if d, err := time.ParseDuration(newCfg.Security.RateLimit.TokenRefresh); err == nil && d > 0 {
			refresh = d
		}
		tokens := newCfg.Security.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = 10000
		}
		g.rateLimiter.UpdateRate(tokens, refresh)
		tlog.Info(context.TODO(), "rate limiter updated maxTokens=%d refresh=%v", tokens, refresh)
	}

	// 动态更新白名单/黑名单（原子替换，避免清空-重填中间态）
	if g.whitelistBlacklist != nil {
		if newCfg.Security.Enabled {
			g.whitelistBlacklist.ReplaceWhitelist(newCfg.Security.Whitelist)
			g.whitelistBlacklist.ReplaceBlacklist(newCfg.Security.Blacklist)
			tlog.Info(context.TODO(), "whitelist/blacklist updated whitelist=%d blacklist=%d",
				len(newCfg.Security.Whitelist),
				len(newCfg.Security.Blacklist))
		} else {
			// 关闭安全链时清空残留列表，避免 fail-stale 继续拦截
			g.whitelistBlacklist.ReplaceWhitelist(nil)
			g.whitelistBlacklist.ReplaceBlacklist(nil)
			tlog.Info(context.TODO(), "security disabled, whitelist/blacklist cleared")
		}
	}

	// 动态更新过载保护阈值
	if g.overloadProtector != nil {
		g.protection.Store(newCfg.Protection)
	}

	// 动态更新连接限制参数
	g.connectionManager.UpdateLimits(newCfg.Protection.MaxConnections, newCfg.Protection.MaxConnectionsPerIP)

	// gRPC/stream 运行时快照随配置更新（GetGRPCConfig/GetStreamConfig 消费）
	g.grpcCfg.Store(newCfg.GRPC)
	g.streamCfg.Store(newCfg.Stream)

	// 动态更新 JWT 密钥
	if g.jwtAuth != nil && newCfg.JWTAuth.Enabled {
		g.jwtAuth.UpdateSecret(newCfg.JWTAuth.Secret)
		tlog.Info(context.TODO(), "jwt secret updated")
	}

	// 动态更新灰度规则
	if g.canaryFilter != nil && newCfg.Canary.Enabled {
		g.canaryFilter.UpdateConfig(newCfg.Canary)
		tlog.Info(context.TODO(), "canary config updated percent=%d", newCfg.Canary.Percent)
	}

	// 动态更新流量镜像比例
	if g.trafficMirror != nil && newCfg.TrafficMirror.Enabled {
		g.trafficMirror.UpdatePercent(newCfg.TrafficMirror.Percent)
		tlog.Info(context.TODO(), "traffic mirror updated percent=%d", newCfg.TrafficMirror.Percent)
	}

	// 动态更新降级规则
	if g.degradation != nil && newCfg.Degradation.Enabled {
		for _, rc := range newCfg.Degradation.Rules {
			g.degradation.AddRule(rc)
		}
		tlog.Info(context.TODO(), "degradation rules updated count=%d", len(newCfg.Degradation.Rules))
	}

	// 登录校验开关随 g.cfg 热更新；打一条便于确认是否误关
	tlog.Info(context.TODO(), "login validation enabled=%v", newCfg.LoginValidation.Enabled)

	tlog.Info(context.TODO(), "config updated dynamically")
}

func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	g.enginesMu.Lock()
	g.engines = append(g.engines, engine)
	g.enginesMu.Unlock()
	return
}

func (g *Gateway) GetConnectionManager() *connection.ConnectionManager {
	return g.connectionManager
}

func (g *Gateway) GetGRPCConfig() config.GRPCConfig {
	v, _ := g.grpcCfg.Load().(config.GRPCConfig)
	return v
}

func (g *Gateway) GetStreamConfig() config.StreamConfig {
	v, _ := g.streamCfg.Load().(config.StreamConfig)
	return v
}

func (g *Gateway) GetShardedCoalescer() *connection.ShardedWriteCoalescer {
	return g.shardedCoalescer
}

// GetGatewayID 返回在每个后端流中通告的稳定身份标识。
func (g *Gateway) GetGatewayID() string {
	return g.gatewayID
}

// GetServerID 返回用于注册此网关的 etcd 实例ID。
func (g *Gateway) GetServerID() string {
	return g.serverID
}

func (g *Gateway) logMetrics() {
	tlog.Info(context.TODO(), "gateway metrics connectionsActive=%d connectionsTotal=%d messagesReceived=%d messagesForwarded=%d messagesPushed=%d messagesProcessed=%d messagesFailed=%d",
		g.connectionsActive.Load(),
		g.connectionsTotal.Load(),
		g.messagesReceived.Load(),
		g.messagesForwarded.Load(),
		g.messagesPushedToClient.Load(),
		g.messagesProcessed.Load(),
		g.messagesFailed.Load(),
	)
}

// metricsLogEnabled 返回是否允许每秒打印 metrics 日志（热更新生效）。
// monitoring.disableMetricsLog=true 时关闭（线上默认建议打开该关闭项）。
func (g *Gateway) metricsLogEnabled() bool {
	cfg, ok := g.cfg.Load().(*config.Config)
	if !ok || cfg == nil {
		return true
	}
	return !cfg.Monitoring.DisableMetricsLog
}

func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	// 每秒滚动采样消息速率（Stats/Health 依赖；原先仅在 Stats 调用时采样，单独查 /health 会失真）
	if g.msgRate != nil {
		g.msgRate.record(time.Now(), g.messagesReceived.Load())
	}
	if g.metricsLogEnabled() {
		g.logMetrics()
	}
	return 1 * time.Second, gnet.None
}

func (g *Gateway) OnShutdown(engine gnet.Engine) {
	// gnet 在 engine 自身关闭流程中回调 OnShutdown；Close 会对所有 engine 调 Stop，
	// 对正在回调的 engine 调 Stop 会与之互等死锁。先把该 engine 摘除再走 Close。
	g.enginesMu.Lock()
	for i, e := range g.engines {
		if e == engine {
			g.engines = append(g.engines[:i], g.engines[i+1:]...)
			break
		}
	}
	g.enginesMu.Unlock()
	g.Close()
}

func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		close(g.stopChan)

		// 阶段1：停止接受新连接（所有 transport engine）
		g.enginesMu.Lock()
		engines := append([]gnet.Engine(nil), g.engines...)
		g.enginesMu.Unlock()
		for i := range engines {
			_ = engines[i].Stop(context.Background())
		}

		// 阶段2：排空进行中的消息（最多2分钟）
		drainTimeout := 2 * time.Minute
		drainDone := make(chan struct{})
		go func() {
			g.drainConnections(drainTimeout)
			close(drainDone)
		}()

		drainTimer := time.NewTimer(drainTimeout)
		select {
		case <-drainDone:
			if !drainTimer.Stop() {
				select {
				case <-drainTimer.C:
				default:
				}
			}
			tlog.Info(context.TODO(), "connection drain completed")
		case <-drainTimer.C:
			tlog.Warn(context.TODO(), "connection drain timed out, forcing close")
		}

		if g.logicClientPool != nil {
			g.logicClientPool.Close()
		}
		if g.gatewayClientPool != nil {
			g.gatewayClientPool.Close()
		}
		if g.logicClient != nil {
			g.logicClient.Close()
		}
		if g.overloadProtector != nil {
			g.overloadProtector.Stop()
		}
		if g.messageIntegrity != nil {
			g.messageIntegrity.Stop()
		}

		g.grpcServerMu.Lock()
		grpcSrv := g.grpcServer
		g.grpcServerMu.Unlock()
		if grpcSrv != nil {
			grpcSrv.Stop()
		}

		g.connectionManager.StopConnectionChecker()

		// 停止 pipeline worker pool（等待所有 worker 退出）
		if g.pipelineWorkerPool != nil {
			g.pipelineWorkerPool.Stop()
		}

		// 停止分片写合并器（最终 flush）
		if g.shardedCoalescer != nil {
			g.shardedCoalescer.Stop()
		}

		g.connectionManager.CloseAllConnections()

		if g.promExporter != nil {
			g.promExporter.Destroy()
		}
		g.StopStatsServer()

		tlog.Info(context.TODO(), "gateway closed")
	})
}

// drainConnections 等待所有连接完成进行中的工作。它将每个连接转换为 connection.StateClosed 状态并等待连接管理器清理。
func (g *Gateway) drainConnections(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// 将所有 Forward 状态的连接转换为 Closed（拒绝新消息）
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		if conn.GetState() == connection.StateForward {
			conn.SetState(connection.StateForward, connection.StateClosed)
		}
		return true
	})

	// 等待连接数降为0或超时
	for time.Now().Before(deadline) {
		if g.connectionManager.GetConnectionCount() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ===== construction =====

// newGateway 用已加载并补齐默认值的配置构造网关。
// 参数仅为 *config.Config，避免 GatewayDeps 单字段间接层。
func newGateway(cfg *config.Config) *Gateway {
	// 防御：外部传入的 cfg 可能未走 Load（测试/热更新拷贝）
	cfg.ApplyRuntimeDefaults()

	gw := &Gateway{
		connectionManager: connection.NewConnectionManager(cfg.Protection.MaxConnections, cfg.Protection.MaxConnectionsPerIP),
		stopChan:          make(chan struct{}),
		serverID:          cfg.ServerID,
		zone:              cfg.Zone,

		configUpdateChan:          make(chan *config.Config, 1),
		overloadProtector:         NewOverloadProtector(cfg.Protection),
		logicClient:               backend.NewLogicClient(nil),
		msgRate:                   newMessageRateTracker(60 * time.Second),
		clusterID:                 "sgate-cluster",
		gatewayID:                 gatewayInstanceID(*cfg),
		isLeader:                  false,
		connectionDurationTracker: obs.NewLatencyTracker(10000),
		tlsConfig:                 buildTLSConfig(cfg),
	}

	// 存指针副本，避免调用方后续原地改写共享配置
	stored := *cfg
	gw.cfg.Store(&stored)
	gw.protection.Store(stored.Protection)
	gw.grpcCfg.Store(stored.GRPC)
	gw.streamCfg.Store(stored.Stream)
	gw.ctx = context.Background()
	gw.pipeline = NewMessagePipeline(gw)
	gw.loginSlots = make(chan struct{}, 256)

	return gw
}

// acquireLoginSlot 获取并发登录槽位；满返回 false（调用方回 429）。
func (g *Gateway) acquireLoginSlot() bool {
	select {
	case g.loginSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseLoginSlot 释放并发登录槽位。
func (g *Gateway) releaseLoginSlot() {
	select {
	case <-g.loginSlots:
	default:
	}
}

func gatewayInstanceID(cfg config.Config) string {
	if cfg.Cluster.NodeID != "" {
		return cfg.Cluster.NodeID
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "sgate"
	}
	return fmt.Sprintf("%s-%d-%d", hostname, os.Getpid(), cfg.GRPC.Port)
}

// 编译期校验：Gateway 实现 backend.GatewayInterface。
var _ backend.GatewayInterface = (*Gateway)(nil)
