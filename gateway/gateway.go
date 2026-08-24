package gateway

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cast"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"github.com/streasure/sgate/monitor"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protobuf.Message) error
}

var (
	frameHeaderPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 4)
			return &buf
		},
	}
)

func extractRouteFast(data []byte) string {
	return protobuf.ExtractRouteFast(data)
}

func extractCmdFast(data []byte) int32 {
	_, cmd := protobuf.ExtractRouteAndCmd(data)
	return cmd
}

// extractRouteAndCmd delegates to protobuf.ExtractRouteAndCmd (shared with logic server).
func extractRouteAndCmd(data []byte) (string, int32) {
	return protobuf.ExtractRouteAndCmd(data)
}

type Gateway struct {
	connectionManager  *ConnectionManager
	stopChan           chan struct{}
	closeOnce          sync.Once
	metrics            *metrics.Metrics
	transportType      map[string]string
	ctx                context.Context
	tlsConfig          *tls.Config
	clusterID          string
	isLeader           bool
	bufferPool         *sync.Pool
	minBufferSize      int
	maxBufferSize      int
	defaultBufferSize  int
	cfg                atomic.Value
	wsConnections      sync.Map
	configPath         string
	configUpdateChan   chan *config.Config
	messageIntegrity   *MessageIntegrity
	versionNegotiation *VersionNegotiation
	tracer             *Tracer
	logicClient        *LogicClient
	logicClientPool    *LogicClientPool
	serviceDiscovery   *ServiceDiscovery
	redisClient        *redis.Client
	overloadProtector  *OverloadProtector
	grpcServer         *grpc.Server
	promExporter       *monitor.PrometheusExporter // Prometheus 指标导出器（enabled=false 时为 nil）
	statsServer        *http.Server
	msgRate            *messageRateTracker // 消息速率滚动窗口（供 Stats() 计算 msgs/sec）
	zone               string
	protection         config.ProtectionConfig
	grpcCfg            config.GRPCConfig
	streamCfg          config.StreamConfig
	// 安全防护组件
	whitelistBlacklist *WhitelistBlacklist
	circuitBreakerMgr  *CircuitBreakerManager
	rateLimiter        *RateLimiter
	waf                *WAF
	cluster            *Cluster
	latencyTracker     *LatencyTracker

	// 企业级扩展组件
	filterChain   *FilterChain        // SPI 过滤器链
	jwtAuth       *JWTAuthFilter      // JWT 鉴权
	balancer      *Balancer           // 负载均衡 + 故障节点摘除
	degradation   *DegradationManager // 降级管理
	configCenter  ConfigCenter        // 配置中心（Nacos/Apollo/etcd/Consul）
	otelTracer    *OTelTracer         // 分布式追踪导出
	alertWebhook  *AlertWebhook       // 告警 webhook（企业微信/钉钉）
	canaryFilter  *CanaryFilter       // 灰度发布
	trafficMirror *TrafficMirror      // 流量镜像
	logSanitizer  *LogSanitizer       // 日志脱敏

	// 转发统计计数器（用于极限压测时观测 sgate 转发能力）
	messagesForwarded                  atomic.Int64
	messagesDroppedOverload            atomic.Int64
	messagesDroppedFull                atomic.Int64
	messagesDroppedNoLogic             atomic.Int64
	messagesDroppedNoLogicNotConnected atomic.Int64
	messagesReceived                   atomic.Int64
	messagesPushedToClient             atomic.Int64
	messagesPushDroppedNoConn          atomic.Int64
	// 细分丢弃原因（与过载保护区分，便于排障）
	messagesDroppedBlacklist   atomic.Int64 // 黑名单/白名单拦截
	messagesDroppedRateLimit   atomic.Int64 // 限流拦截
	messagesDroppedWAF         atomic.Int64 // WAF 拦截
	messagesDroppedCircuit     atomic.Int64 // 熔断器拦截
	messagesDroppedIntegrity   atomic.Int64 // 完整性校验失败
	messagesDroppedFilterChain atomic.Int64 // filter chain 中止

	// Prometheus 看板扩展计数器（Grafana dashboard 引用）
	circuitBreakerTripped  atomic.Int64
	degradationTriggered   atomic.Int64
	canaryHit              atomic.Int64
	trafficMirrorForwarded atomic.Int64
	trafficMirrorDropped   atomic.Int64
	alertSent              atomic.Int64
	alertDropped           atomic.Int64
}

func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType[port] = transportType
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：logic->sgate->client）
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

func (g *Gateway) GetBuffer(size int) []byte {
	buf := g.bufferPool.Get().([]byte)
	if cap(buf) < size {
		newSize := cap(buf) * 2
		if newSize < size {
			newSize = size
		}
		if newSize > g.maxBufferSize {
			newSize = g.maxBufferSize
		}
		newBuf := make([]byte, newSize)
		g.bufferPool.Put(buf)
		return newBuf
	}
	return buf[:cap(buf)]
}

func (g *Gateway) PutBuffer(buf []byte) {
	if cap(buf) >= g.minBufferSize && cap(buf) <= g.maxBufferSize {
		buf = buf[:cap(buf)]
		g.bufferPool.Put(buf)
	}
}

var protobufMessagePool = sync.Pool{
	New: func() interface{} {
		return &protobuf.Message{
			Payload: make(map[string]string, 32),
		}
	},
}

const preallocatedProtobufMessages = 64

func init() {
	for i := 0; i < preallocatedProtobufMessages; i++ {
		protobufMessagePool.Put(&protobuf.Message{
			Payload: make(map[string]string, 32),
		})
	}
}

func GetProtobufMessage() *protobuf.Message {
	msg := protobufMessagePool.Get().(*protobuf.Message)
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	} else {
		msg.Payload = make(map[string]string, 32)
	}
	return msg
}

func PutProtobufMessage(msg *protobuf.Message) {
	if msg == nil {
		return
	}
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	msg.Cmd = 0
	msg.Data = nil
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
	protobufMessagePool.Put(msg)
}

func NewGateway() *Gateway {
	ctx := context.Background()

	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
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
	}

	protection := cfg.Protection
	if protection.MaxFrameSize <= 0 {
		protection.MaxFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxFrameBufSize <= 0 {
		protection.MaxFrameBufSize = 4 * 1024 * 1024
	}
	if protection.MaxWSFrameSize <= 0 {
		protection.MaxWSFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxWSBufferSize <= 0 {
		protection.MaxWSBufferSize = 4 * 1024 * 1024
	}
	if protection.WSHeartbeatTimeout <= 0 {
		protection.WSHeartbeatTimeout = 60
	}
	if protection.WSCheckInterval <= 0 {
		protection.WSCheckInterval = 30
	}

	grpcCfg := cfg.GRPC
	if grpcCfg.Port <= 0 {
		grpcCfg.Port = 50051
	}
	if grpcCfg.WindowSize <= 0 {
		grpcCfg.WindowSize = 524288
	}
	if grpcCfg.MaxMessageSize <= 0 {
		grpcCfg.MaxMessageSize = 4 * 1024 * 1024
	}

	streamCfg := cfg.Stream
	if streamCfg.SendChannelSize <= 0 {
		streamCfg.SendChannelSize = 65536
	}
	if streamCfg.ReceiveBatchSize <= 0 {
		streamCfg.ReceiveBatchSize = 64
	}

	gw := &Gateway{
		connectionManager: NewConnectionManager(),
		stopChan:          make(chan struct{}),
		metrics:           metrics.NewMetrics(),
		transportType:     make(map[string]string),
		ctx:               ctx,
		tlsConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS13,
			// 硬件加速 cipher suite（AES-NI / AES-CLMUL 自动启用）
			// 仅保留 AES-GCM 套件，避免 ChaCha20（无硬件加速时才用）
			CipherSuites: []uint16{
				// TLS 1.3 套件（Go 自动选择，列出仅文档意义）
				tls.TLS_AES_128_GCM_SHA256,
				tls.TLS_AES_256_GCM_SHA384,
				tls.TLS_CHACHA20_POLY1305_SHA256, // fallback：客户端无 AES-NI 时
				// TLS 1.2 套件
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			},
			PreferServerCipherSuites: true,
			// 椭圆曲线偏好：X25519（CPU 友好）→ P256（Haswell 加速）
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
			},
		},
		filterChain:       NewFilterChain(),
		logSanitizer:      NewLogSanitizer(),
		clusterID:         "sgate-cluster",
		isLeader:          false,
		minBufferSize:     4096,
		maxBufferSize:     65536,
		defaultBufferSize: 16384,
		bufferPool: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 16384)
			},
		},
		configUpdateChan:   make(chan *config.Config),
		overloadProtector:  NewOverloadProtector(protection),
		logicClient:        NewLogicClient(GatewayInterface(nil)),
		protection:         protection,
		grpcCfg:            grpcCfg,
		streamCfg:          streamCfg,
		whitelistBlacklist: NewWhitelistBlacklist(),
		circuitBreakerMgr:  NewCircuitBreakerManager(),
		msgRate:            newMessageRateTracker(60 * time.Second),
	}

	// 加载 TLS 证书
	if cfg.TLS.Enabled && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			tlog.Error("failed to load TLS certificate", "error", err)
		} else {
			gw.tlsConfig.Certificates = []tls.Certificate{cert}
			// 强制 TLS 1.3 开关：当 minVersion 配置为 TLS1.3 时
			if strings.EqualFold(cfg.TLS.MinVersion, "TLS1.3") {
				gw.tlsConfig.MinVersion = tls.VersionTLS13
				tlog.Info("TLS 1.3 enforced", "certFile", cfg.TLS.CertFile)
			} else {
				tlog.Info("TLS certificate loaded", "certFile", cfg.TLS.CertFile,
					"minVersion", cfg.TLS.MinVersion)
			}
		}
	}

	// 初始化负载均衡器
	gw.balancer = NewBalancer(cfg.Balancer)

	// 初始化 JWT 鉴权
	if cfg.JWTAuth.Enabled {
		gw.jwtAuth = NewJWTAuthFilter(cfg.JWTAuth)
		gw.filterChain.AddFilter(gw.jwtAuth)
	}

	// 初始化灰度发布
	if cfg.Canary.Enabled {
		gw.canaryFilter = NewCanaryFilter(cfg.Canary)
		gw.filterChain.AddFilter(gw.canaryFilter)
	}

	// 初始化流量镜像
	if cfg.TrafficMirror.Enabled {
		gw.trafficMirror = NewTrafficMirror(cfg.TrafficMirror)
		gw.filterChain.AddFilter(&MirrorFilter{tm: gw.trafficMirror})
	}

	// 初始化降级管理
	if cfg.Degradation.Enabled {
		gw.degradation = NewDegradationManager(cfg.Degradation.Rules)
		gw.filterChain.AddFilter(gw.degradation)
	}

	// 初始化 OTel 分布式追踪
	if cfg.OTelTracer.Enabled {
		gw.otelTracer = NewOTelTracer(cfg.OTelTracer)
		gw.filterChain.AddFilter(&OTelSpanFilter{tracer: gw.otelTracer})
	}

	// 初始化告警 webhook
	if cfg.Alert.Enabled {
		gw.alertWebhook = NewAlertWebhook(cfg.Alert)
	}

	// 初始化配置中心
	if cfg.ConfigCenter.Enabled {
		gw.configCenter = NewConfigCenter(cfg.ConfigCenter)
		gw.startConfigCenterWatcher()
	}

	// 动态加载配置中声明的 SPI 过滤器
	for _, fi := range cfg.FilterChain.Filters {
		if err := gw.filterChain.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn("failed to load filter from config",
				"name", fi.Name, "error", err)
		}
	}

	// 初始化限流器
	if cfg.Security.RateLimit.Enabled {
		refresh := time.Second
		if d, err := time.ParseDuration(cfg.Security.RateLimit.TokenRefresh); err == nil {
			refresh = d
		}
		tokens := cfg.Security.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = 10000
		}
		gw.rateLimiter = NewRateLimiter(tokens, refresh)
	}

	// 初始化 WAF
	if cfg.WAF.Enabled {
		gw.waf = NewWAF(cfg.WAF)
	}

	// 初始化白名单/黑名单配置
	if cfg.Security.Enabled {
		for _, ip := range cfg.Security.Whitelist {
			gw.whitelistBlacklist.AddToWhitelist(ip)
		}
		for _, ip := range cfg.Security.Blacklist {
			gw.whitelistBlacklist.AddToBlacklist(ip)
		}
	}

	gw.cfg.Store(cfg)

	gw.overloadProtector.Start()

	go gw.wsHeartbeatChecker()

	gw.messageIntegrity = NewMessageIntegrity(30000)

	supportedVersions := []string{"1.0.0", "1.1.0", "2.0.0"}
	gw.versionNegotiation = NewVersionNegotiation(supportedVersions, 10*time.Second)

	gw.tracer = NewTracer(5 * time.Minute)
	gw.latencyTracker = NewLatencyTracker(10000)

	connCheckInterval, _ := time.ParseDuration(protection.ConnCheckInterval)
	if connCheckInterval <= 0 {
		connCheckInterval = 5 * time.Minute
	}
	connIdleTimeout, _ := time.ParseDuration(protection.ConnIdleTimeout)
	if connIdleTimeout <= 0 {
		connIdleTimeout = 30 * time.Second
	}
	gw.connectionManager.StartConnectionChecker(connCheckInterval, connIdleTimeout)

	go gw.configWatcher()

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

	if cfg.Zone == "" {
		cfg.Zone = "default"
	}
	gw.zone = cfg.Zone
	// C3 修复: 统一 zone 来源。discovery.zone 未单独配置时回退到顶层 zone，
	// 避免两个 zone 字段不一致导致过滤逻辑失效。
	if cfg.Discovery.Zone == "" {
		cfg.Discovery.Zone = cfg.Zone
	}

	tlog.Info("gateway zone configured", "zone", gw.zone)

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
		// 连接预热：立即建立 gRPC 连接，避免首个请求冷启动突刺
		go func() {
			tlog.Info("pre-warming logic server connection", "address", "localhost:50052")
			if err := gw.logicClient.Connect("localhost:50052"); err != nil {
				tlog.Error("failed to connect to logic server", "error", err)
			} else {
				tlog.Info("successfully connected to logic server (pre-warmed)")
			}
		}()
	}

	grpcPort := fmt.Sprintf(":%d", grpcCfg.Port)
	tlog.Info("about to start gRPC server", "port", grpcPort)
	go func() {
		tlog.Info("starting gRPC server", "port", grpcPort)
		if server, err := StartGRPCServer(gw, grpcPort, gw.grpcCfg.MaxMessageSize, gw.grpcCfg.WindowSize); err != nil {
			tlog.Error("failed to start gRPC server", "error", err)
		} else {
			gw.grpcServer = server
			tlog.Info("gRPC server started", "port", grpcPort)
		}
	}()

	// 初始化集群（依赖 Redis 做 Leader 选举与服务注册）
	if cfg.Cluster.Enabled && gw.redisClient != nil {
		gw.cluster = NewCluster(gw.redisClient, cfg.Cluster, gw.zone)
		gw.cluster.Start(ctx)
	}

	// 启动统计 HTTP 服务（暴露 /stats /health /ready /live）
	// 端口来自 config.yaml 的 port 字段（默认 8081，避开 Nacos 8080）
	gw.StartStatsServer(fmt.Sprintf(":%d", cfg.Port))

	// 启动 Prometheus 指标导出器（可插拔，通过配置开关控制）
	// 使用独立的 monitor 包，可被任意 Go 项目 import
	// 关闭时 sgate 单体也能正常运行，只是不暴露 /metrics 端点
	if cfg.Monitoring.Prometheus.Enabled {
		gw.promExporter = monitor.NewPrometheusExporter(
			gw, // Gateway 实现 monitor.StatsProvider 接口
			cfg.Monitoring.Prometheus.Addr,
			cfg.Monitoring.Prometheus.Path,
		)
		gw.promExporter.Start()
	}

	return gw
}

func (g *Gateway) wsHeartbeatChecker() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("wsHeartbeatChecker panic recovered", "error", r)
		}
	}()
	checkInterval := time.Duration(g.protection.WSCheckInterval) * time.Second
	heartbeatTimeout := time.Duration(g.protection.WSHeartbeatTimeout) * time.Second
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
	var connections []*WebSocketConnection
	g.wsConnections.Range(func(key, value interface{}) bool {
		if conn, ok := key.(*WebSocketConnection); ok {
			connections = append(connections, conn)
		}
		return true
	})

	for _, conn := range connections {
		if time.Since(conn.LastPingTime) > timeout {
			tlog.Warn("WebSocket connection timeout, closing", "connectionID", conn.ConnectionID)
			if conn.Conn != nil {
				conn.Conn.Close()
			}
			if conn.ConnectionID != "" {
				g.connectionManager.RemoveConnection(conn.ConnectionID)
			}
			g.wsConnections.Delete(conn)
			wsConnectionPool.Put(conn)
		}
	}
}

func (g *Gateway) configWatcher() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("configWatcher panic recovered", "error", r)
		}
	}()
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
				newCfg, err := config.LoadConfig()
				if err != nil {
					time.Sleep(5 * time.Second)
					continue
				}
				select {
				case g.configUpdateChan <- newCfg:
				default:
				}
			}

			time.Sleep(5 * time.Second)
		}
	}
}

func (g *Gateway) handleConfigUpdate(newCfg *config.Config) {
	oldCfg := g.cfg.Load().(*config.Config)
	g.cfg.Store(newCfg)

	// 动态更新限流阈值（无需重启）
	if g.rateLimiter != nil && newCfg.Security.RateLimit.Enabled {
		refresh := time.Second
		if d, err := time.ParseDuration(newCfg.Security.RateLimit.TokenRefresh); err == nil {
			refresh = d
		}
		tokens := newCfg.Security.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = 10000
		}
		g.rateLimiter.UpdateRate(tokens, refresh)
		tlog.Info("rate limiter updated", "maxTokens", tokens, "refresh", refresh)
	}

	// 动态更新白名单/黑名单
	if g.whitelistBlacklist != nil && newCfg.Security.Enabled {
		// 清空旧名单
		for _, ip := range g.whitelistBlacklist.GetWhitelist() {
			g.whitelistBlacklist.RemoveFromWhitelist(ip)
		}
		for _, ip := range g.whitelistBlacklist.GetBlacklist() {
			g.whitelistBlacklist.RemoveFromBlacklist(ip)
		}
		// 加载新名单
		for _, ip := range newCfg.Security.Whitelist {
			g.whitelistBlacklist.AddToWhitelist(ip)
		}
		for _, ip := range newCfg.Security.Blacklist {
			g.whitelistBlacklist.AddToBlacklist(ip)
		}
		tlog.Info("whitelist/blacklist updated",
			"whitelist", len(newCfg.Security.Whitelist),
			"blacklist", len(newCfg.Security.Blacklist))
	}

	// 动态更新过载保护阈值
	if g.overloadProtector != nil {
		g.protection = newCfg.Protection
	}

	// 动态更新 JWT 密钥
	if g.jwtAuth != nil && newCfg.JWTAuth.Enabled {
		g.jwtAuth.UpdateSecret(newCfg.JWTAuth.Secret)
		tlog.Info("jwt secret updated")
	}

	// 动态更新灰度规则
	if g.canaryFilter != nil && newCfg.Canary.Enabled {
		g.canaryFilter.UpdateConfig(newCfg.Canary)
		tlog.Info("canary config updated", "percent", newCfg.Canary.Percent)
	}

	// 动态更新流量镜像比例
	if g.trafficMirror != nil && newCfg.TrafficMirror.Enabled {
		g.trafficMirror.UpdatePercent(newCfg.TrafficMirror.Percent)
		tlog.Info("traffic mirror updated", "percent", newCfg.TrafficMirror.Percent)
	}

	// 动态更新降级规则
	if g.degradation != nil && newCfg.Degradation.Enabled {
		for _, rc := range newCfg.Degradation.Rules {
			g.degradation.AddRule(rc)
		}
		tlog.Info("degradation rules updated", "count", len(newCfg.Degradation.Rules))
	}

	_ = oldCfg
	tlog.Info("config updated dynamically")
}

var connContextPool = sync.Pool{
	New: func() interface{} {
		return &ConnContext{
			ConnectionID: "",
			FrameBuf:     nil,
		}
	},
}

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

type ConnContext struct {
	ConnectionID string
	UserUUID     string
	FrameBuf     []byte
	FrameOff     int
}

func (g *Gateway) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnOpen panic recovered", "error", r)
			action = gnet.Close
		}
	}()

	localAddr := c.LocalAddr().String()
	isWS := false
	for port, t := range g.transportType {
		if strings.HasSuffix(localAddr, ":"+port) && t == "websocket" {
			isWS = true
			break
		}
	}

	if isWS {
		wsConn := NewWebSocketConnection(c)
		c.SetContext(wsConn)
		g.wsConnections.Store(wsConn, true)
	} else {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
		connCtx := GetConnContext()
		connCtx.ConnectionID = connectionID
		c.SetContext(connCtx)
	}

	g.metrics.IncConnectionsTotal()
	g.metrics.IncConnectionsActive()

	tlog.Debug("new connection", "localAddr", localAddr, "isWS", isWS)
	return
}

func (g *Gateway) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnClose panic recovered", "error", r)
		}
	}()

	var connectionID string
	connCtx := c.Context()

	if connCtx != nil {
		if ctx, ok := connCtx.(*ConnContext); ok {
			connectionID = ctx.ConnectionID
			PutConnContext(ctx)
		} else if wsConn, ok := connCtx.(*WebSocketConnection); ok {
			connectionID = wsConn.ConnectionID
			g.wsConnections.Delete(wsConn)
			wsConn.Buffer = nil
			wsConn.ConnectionID = ""
			wsConn.Conn = nil
			atomic.StoreInt32(&wsConn.State, int32(WSStateClosed))
			wsConnectionPool.Put(wsConn)
		} else if id, ok := connCtx.(string); ok {
			connectionID = id
		}
	}

	if connectionID != "" {
		g.connectionManager.RemoveConnection(connectionID)
		g.versionNegotiation.RemoveClientVersion(connectionID)
		g.metrics.DecConnectionsActive()
		tlog.Debug("connection closed", "connectionID", connectionID, "error", err)
	}

	return
}

func (g *Gateway) OnTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnTraffic panic recovered", "error", fmt.Sprintf("%v", r))
			action = gnet.Close
		}
	}()

	return g.handleNormalTraffic(c)
}

func (g *Gateway) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("handleNormalTraffic panic recovered", "error", cast.ToString(r))
			action = gnet.Close
		}
	}()

	data, err := c.Next(-1)
	if err != nil {
		return gnet.Close
	}

	connCtx := c.Context()
	if connCtx == nil {
		return gnet.Close

	}
	if wsConn, ok := connCtx.(*WebSocketConnection); ok {
		return g.handleWebSocketMessage(wsConn, data)
	}

	ctx, ok := connCtx.(*ConnContext)
	if !ok {
		if len(data) > 3 && data[0] == 'G' && data[1] == 'E' && data[2] == 'T' {
			wsConn := NewWebSocketConnection(c)
			c.SetContext(wsConn)
			g.wsConnections.Store(wsConn, wsConn)
			return g.handleWebSocketMessage(wsConn, data)
		}
		return gnet.Close
	}

	maxFrameBuf := g.protection.MaxFrameBufSize
	if len(ctx.FrameBuf)+len(data) > maxFrameBuf {
		ctx.FrameBuf = nil
		ctx.FrameOff = 0
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	// 批量路径：VerifyInbound 关闭时，将本次 OnTraffic 的完整帧打包为
	// 单个 RouteBatch 消息，消除逐帧 protobuf 解析、深拷贝和通道发送开销。
	if !g.protection.VerifyInbound {
		return g.handleBatchTraffic(c, ctx)
	}

	// Slow path: per-frame processing for VerifyInbound or handshake
	maxFrame := g.protection.MaxFrameSize
	for len(ctx.FrameBuf) >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[:4])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			ctx.FrameBuf = nil
			ctx.FrameOff = 0
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if len(ctx.FrameBuf) < totalLen {
			return
		}

		frameData := ctx.FrameBuf[4:totalLen]

		if len(ctx.FrameBuf) > totalLen {
			ctx.FrameBuf = ctx.FrameBuf[totalLen:]
		} else {
			ctx.FrameBuf = nil
		}
		ctx.FrameOff = 0

		if ret := g.handleTCPRequest(c, frameData); ret == gnet.Close {
			return gnet.Close
		}
	}

	return
}

// handleBatchTraffic collects all complete frames from FrameBuf into a single
// RouteBatch message and forwards it via one SendMessage call. This reduces
// per-frame overhead (proto parse, deep copy, alloc, channel send) to per-batch.
//
// Zero-copy optimization: FrameBuf already contains [4-byte frameLen][frameData]
// repeated, which is exactly the RouteBatch Data format. Instead of copying
// frames into a separate batch buffer, we transfer ownership of the FrameBuf
// slice to the batch message and let the next OnTraffic call allocate a fresh
// buffer. This eliminates the per-batch 256KB allocation + copy that caused
// GC pressure at 20M QPS.
//
// Batch format (single-conn): RouteBatch message with:
//
//	ConnectionId = ctx.ConnectionID (shared by all frames from this connection)
//	Data = FrameBuf[:offset] (transferred ownership, zero copy)
//	Cmd = frame count
//
// The logic server unmarshals each payload to get route and dispatches individually.
// ConnectionId is set from the outer message if the inner message doesn't have one.
func (g *Gateway) handleBatchTraffic(c gnet.Conn, ctx *ConnContext) (action gnet.Action) {
	maxFrame := g.protection.MaxFrameSize

	// Count complete frames and find the split point.
	// FrameBuf format: [4-byte frameLen][frameData] repeated
	// = exactly the batch format needed by the logic server.
	offset := 0
	batchCount := 0
	for offset+4 <= len(ctx.FrameBuf) {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[offset : offset+4])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			ctx.FrameBuf = nil
			ctx.FrameOff = 0
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if offset+totalLen > len(ctx.FrameBuf) {
			break // incomplete frame, wait for more data
		}

		// Quick handshake check on first frame only.
		if batchCount == 0 {
			frameData := ctx.FrameBuf[offset+4 : offset+totalLen]
			route := extractRouteFast(frameData)
			if route == protobuf.RouteHandshake {
				// Fall back to per-frame handler for handshake
				rest := ctx.FrameBuf[offset+totalLen:]
				if len(rest) > 0 {
					tail := make([]byte, len(rest))
					copy(tail, rest)
					ctx.FrameBuf = tail
				} else {
					ctx.FrameBuf = nil
				}
				ctx.FrameOff = 0
				return g.handleTCPRequest(c, frameData)
			}
		}

		offset += totalLen
		batchCount++
	}

	if batchCount == 0 {
		return
	}

	g.messagesReceived.Add(int64(batchCount))

	// Split FrameBuf FIRST: transfer complete frames to batchData, keep
	// incomplete tail in FrameBuf. This must happen before any early return
	// (overload, no logic client) to prevent frames from being recounted
	// on the next OnTraffic call — which would cause unbounded growth of
	// messagesReceived/dropped counters and FrameBuf memory.
	var batchData []byte
	if offset == len(ctx.FrameBuf) {
		batchData = ctx.FrameBuf
		ctx.FrameBuf = nil
	} else {
		batchData = ctx.FrameBuf[:offset]
		tail := make([]byte, len(ctx.FrameBuf)-offset)
		copy(tail, ctx.FrameBuf[offset:])
		ctx.FrameBuf = tail
	}
	ctx.FrameOff = 0

	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(int64(batchCount))
		g.messagesDroppedOverload.Add(int64(batchCount))
		return
	}

	logicClient := g.getLogicClient()
	if logicClient == nil {
		g.messagesDroppedNoLogicNotConnected.Add(int64(batchCount))
		return
	}

	batchMsg := &protobuf.Message{
		ConnectionId: ctx.ConnectionID,
		Route:        protobuf.RouteBatch,
		Data:         batchData,
		Cmd:          int32(batchCount),
	}

	if err := logicClient.SendMessage(batchMsg); err != nil {
		g.messagesDroppedFull.Add(int64(batchCount))
	} else {
		g.messagesForwarded.Add(int64(batchCount))
	}

	return
}

func (g *Gateway) isLogicConnected() bool {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return true
	}
	if g.logicClient != nil && g.logicClient.IsConnected() {
		return true
	}
	return false
}

func (g *Gateway) getLogicClient() LogicClientProvider {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return g.logicClientPool
	}
	if g.logicClient != nil && g.logicClient.IsConnected() {
		return g.logicClient
	}
	return nil
}

func (g *Gateway) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		return
	}

	g.messagesReceived.Add(1)

	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		return
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
	}

	logicClient := g.getLogicClient()
	if logicClient != nil {
		route, cmd := extractRouteAndCmd(data)
		if route == protobuf.RouteHandshake {
			message := GetProtobufMessage()
			defer PutProtobufMessage(message)
			if err := proto.Unmarshal(data, message); err != nil {
				return
			}
			return g.handleHandshake(c, connectionID, message)
		}

		// IP 白名单/黑名单检查
		if g.whitelistBlacklist != nil {
			remoteIP := getRemoteIP(c)
			if g.whitelistBlacklist.IsInBlacklist(remoteIP) {
				g.messagesDroppedBlacklist.Add(1)
				return
			}
			// 白名单非空时，仅放行白名单 IP
			whitelist := g.whitelistBlacklist.GetWhitelist()
			if len(whitelist) > 0 && !g.whitelistBlacklist.IsInWhitelist(remoteIP) {
				g.messagesDroppedBlacklist.Add(1)
				return
			}
		}

		// 限流检查（按 IP 维度）
		if g.rateLimiter != nil {
			remoteIP := getRemoteIP(c)
			if !g.rateLimiter.Allow("ip", remoteIP) {
				g.messagesDroppedRateLimit.Add(1)
				return
			}
			if !g.rateLimiter.Allow("route", route) {
				g.messagesDroppedRateLimit.Add(1)
				return
			}
		}

		// WAF 检查（SQL 注入/XSS/大 payload）
		if g.waf != nil {
			if !g.waf.Inspect(data) {
				g.messagesDroppedWAF.Add(1)
				return
			}
		}

		// 熔断器检查（按 route 维度，自动创建）
		if g.circuitBreakerMgr != nil {
			breaker := g.getOrCreateBreaker(route)
			if !breaker.Allow() {
				g.messagesDroppedCircuit.Add(1)
				return
			}
		}

		// 入方向消息完整性校验
		if g.protection.VerifyInbound {
			verifyMsg := GetProtobufMessage()
			if uerr := proto.Unmarshal(data, verifyMsg); uerr == nil {
				if verr := g.messageIntegrity.ProcessMessage(verifyMsg); verr != nil {
					PutProtobufMessage(verifyMsg)
					g.messagesDroppedIntegrity.Add(1)
					return
				}
			}
			PutProtobufMessage(verifyMsg)
		}

		// Tracer: 采样追踪转发延迟
		var span *TraceSpan
		if g.tracer != nil {
			traceID := GenerateTraceID()
			span = g.tracer.StartSpan(traceID, "forward", "")
			g.tracer.AddAttribute(span, "route", route)
			g.tracer.AddAttribute(span, "connectionID", connectionID)
		}

		// SPI 过滤器链：JWT 鉴权 / 灰度 / 镜像 / OTel / 降级等
		// 过滤器可修改 route/data/userUUID，或中止请求
		protoMsg, ok := g.applyForwardFilters(c, data, connectionID, route, cmd)
		if !ok {
			if span != nil && g.tracer != nil {
				g.tracer.EndSpan(span)
			}
			return
		}
		if protoMsg == nil {
			// 兼容 filter chain 未启用场景：构造默认消息
			protoMsg = &protobuf.Message{
				ConnectionId: connectionID,
				Route:        route,
				Data:         append([]byte(nil), data...),
			}
			if cmd > 0 {
				protoMsg.Cmd = cmd
			}
		} else if cmd > 0 && protoMsg.Cmd == 0 {
			protoMsg.Cmd = cmd
		}

		if err := logicClient.SendMessage(protoMsg); err != nil {
			g.messagesDroppedFull.Add(1)
			if g.circuitBreakerMgr != nil {
				breaker := g.getOrCreateBreaker(route)
				breaker.RecordFailure()
			}
			if g.balancer != nil {
				g.balancer.RecordFailure(protoMsg.Route)
			}
			if g.degradation != nil {
				g.degradation.RecordResult(protoMsg.Route, true)
			}
		} else {
			g.messagesForwarded.Add(1)
			if g.circuitBreakerMgr != nil {
				breaker := g.getOrCreateBreaker(route)
				breaker.RecordSuccess()
			}
			if g.balancer != nil {
				g.balancer.RecordSuccess(protoMsg.Route)
			}
			if g.degradation != nil {
				g.degradation.RecordResult(protoMsg.Route, false)
			}
		}

		if span != nil && g.tracer != nil {
			g.tracer.EndSpan(span)
			if g.latencyTracker != nil {
				g.latencyTracker.Record(span.Duration)
			}
		}
		return
	}

	g.messagesDroppedNoLogicNotConnected.Add(1)
	return
}

// getRemoteIP 从 gnet.Conn 获取客户端 IP
func getRemoteIP(c gnet.Conn) string {
	addr := c.RemoteAddr()
	if addr == nil {
		return "unknown"
	}
	s := addr.String()
	// 去掉端口部分
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i]
		}
	}
	return s
}

// getOrCreateBreaker 获取或创建指定 route 的熔断器
func (g *Gateway) getOrCreateBreaker(route string) *CircuitBreaker {
	timeout := 30 * time.Second
	if d, err := time.ParseDuration(g.protection.ConnIdleTimeout); err == nil && d > 0 {
		timeout = d
	}
	return g.circuitBreakerMgr.GetCircuitBreaker(route, 5, 3, timeout)
}

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
		return gnet.None
	}

	negotiatedVersion, err := g.versionNegotiation.ProcessHandshake(connectionID, handshake)
	if err != nil {
		return gnet.None
	}

	response := g.versionNegotiation.GenerateHandshakeResponse(negotiatedVersion)
	g.messageIntegrity.PrepareMessage(response)
	writeMsgFrame(c, response)

	if serverID := message.Payload["serverId"]; serverID != "" {
		g.connectionManager.SetConnectionServerID(connectionID, serverID)
	}

	return gnet.None
}

func writeFrame(c gnet.Conn, data []byte) {
	headerPtr := frameHeaderPool.Get().(*[]byte)
	binary.BigEndian.PutUint32(*headerPtr, uint32(len(data)))
	c.Writev([][]byte{*headerPtr, data})
	frameHeaderPool.Put(headerPtr)
}

func writeErrorFrame(c gnet.Conn, errMsg *protobuf.ErrorResponse) {
	data, _ := proto.Marshal(errMsg)
	writeFrame(c, data)
}

func writeMsgFrame(c gnet.Conn, msg *protobuf.Message) {
	data, _ := proto.Marshal(msg)
	writeFrame(c, data)
}

func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	return
}

func (g *Gateway) GetVersion() string {
	return "1.0.0"
}

func (g *Gateway) GetConnectionManager() *ConnectionManager {
	return g.connectionManager
}

func (g *Gateway) GetGRPCConfig() config.GRPCConfig {
	return g.grpcCfg
}

func (g *Gateway) GetStreamConfig() config.StreamConfig {
	return g.streamCfg
}

func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	g.metrics.LogMetrics()
	return 1 * time.Second, gnet.None
}

func (g *Gateway) OnShutdown(engine gnet.Engine) {
	g.Close()
}

func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		close(g.stopChan)

		if g.serviceDiscovery != nil {
			g.serviceDiscovery.Stop()
		}

		if g.logicClientPool != nil {
			g.logicClientPool.Close()
		}

		if g.grpcServer != nil {
			stopped := make(chan struct{})
			go func() {
				g.grpcServer.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				g.grpcServer.Stop()
			}
		}

		if g.redisClient != nil {
			g.redisClient.Close()
		}

		g.connectionManager.StopConnectionChecker()
		g.connectionManager.CloseAllConnections()

		g.overloadProtector.Stop()
		g.tracer.Stop()

		if g.promExporter != nil {
			g.promExporter.Stop()
		}
		g.StopStatsServer()

		tlog.Info("gateway closed")
	})
}
