package internal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/spf13/cast"
	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/gateway"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/uetcd"
	"github.com/streasure/util/prometheus"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// LogicClientProvider 定义逻辑客户端的连接状态和消息发送能力。
type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protoGw.StreamData) error
}

// GatewayClientProvider 定义网关客户端的连接状态和客户端访问能力。
type GatewayClientProvider interface {
	IsConnected() bool
	Client() protoGw.GatewayClient
}

func extractRouteAndCmd(data []byte) (string, int32) {
	return gateway.ExtractRouteAndCmd(data)
}

func newErrorResponse(route, message, details, data string) *commonstruct.ErrorResponse {
	return &commonstruct.ErrorResponse{
		Route: route,
		Error: &commonstruct.ErrorData{
			Message: message,
			Code:    details,
			Details: data,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

// Gateway 表示网关实例，负责连接接入、协议处理、逻辑转发和组件生命周期。
type Gateway struct {
	connectionManager *ConnectionManager
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
	logicClient       *LogicClient
	logicClientPool   *LogicClientPool
	gatewayClientPool *GatewayClientPool
	serverID          string
	serviceDiscovery  *uetcd.Component
	gatewayDiscovery  *uetcd.Component // 发现其他网关（Gateway:{zone}）
	gatewayEvents     []uetcd.ServiceEvent
	overloadProtector *OverloadProtector
	grpcServer        *grpc.Server
	promExporter      *prometheus.Exporter // Prometheus 指标导出器（enabled=false 时为空）。
	statsServer       *http.Server
	msgRate           *messageRateTracker // 消息速率滚动窗口（供 Stats() 计算 msgs/sec）
	zone              string
	protection        config.ProtectionConfig
	grpcCfg           config.GRPCConfig
	streamCfg         config.StreamConfig
	// 安全防护组件
	whitelistBlacklist *security.WhitelistBlacklist
	circuitBreakerMgr  *security.CircuitBreakerManager
	rateLimiter        *security.RateLimiter
	waf                *security.WAF
	cluster            *cluster.Cluster
	latencyTracker     *obs.LatencyTracker
	engine             *gnet.Engine // 启动时保存，用于优雅关闭

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

	// Prometheus 看板扩展计数器（Grafana dashboard 引用）
	circuitBreakerTripped  atomic.Int64
	degradationTriggered   atomic.Int64
	canaryHit              atomic.Int64
	trafficMirrorForwarded atomic.Int64
	trafficMirrorDropped   atomic.Int64
	alertSent              atomic.Int64
	alertDropped           atomic.Int64

	// 连接生命周期指标
	connectionDurationSum     atomic.Int64        // 连接总存活时长（毫秒），用于计算平均值
	connectionDurationCount   atomic.Int64        // 已关闭连接数，用于计算平均值
	connectionDurationTracker *obs.LatencyTracker // 连接时长分位数追踪器
}

// SetTransportType 设置监听端口对应的传输类型。
func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType.Store(port, transportType)
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：逻辑服到网关再到客户端）。
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

// NewGateway 加载配置并创建、初始化、启动网关组件。
func NewGateway(configFiles ...string) *Gateway {
	cfg, err := config.LoadConfig(configFiles...)
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

	// 创建共享过滤器链
	fc := types.NewFilterChain()

	// 创建所有生命周期组件
	secComp := NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
	obsComp := NewObservabilityComponent(cfg.OTelTracer, cfg.Monitoring.PprofAddr, fc)
	traComp := NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
	clsComp := NewClusterComponent(*cfg, cfg.GRPC.Port, nil)

	// 初始化并启动所有组件
	for _, comp := range []component.Component{secComp, obsComp, traComp, clsComp} {
		if err := comp.Init(); err != nil {
			panic(fmt.Sprintf("component %s init failed: %v", comp.Name(), err))
		}
	}
	for _, comp := range []component.Component{secComp, obsComp, traComp, clsComp} {
		if err := comp.Start(); err != nil {
			panic(fmt.Sprintf("component %s start failed: %v", comp.Name(), err))
		}
	}

	// 从配置加载 SPI 过滤器
	for _, fi := range cfg.FilterChain.Filters {
		if err := fc.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn("failed to load filter from config", "name", fi.Name, "error", err)
		}
	}

	// 通过依赖注入构建网关
	gw := NewGatewayWithDeps(GatewayDeps{
		Config:             *cfg,
		FilterChain:        fc,
		LogSanitizer:       obsComp.LogSanitizer,
		WhitelistBlacklist: secComp.WhitelistBlacklist,
		WAF:                secComp.WAF,
		RateLimiter:        secComp.RateLimiter,
		JWTAuth:            secComp.JWTAuth,
		CircuitBreakerMgr:  secComp.CircuitBreakerMgr,
		Tracer:             obsComp.Tracer,
		OTelTracer:         obsComp.OTelTracer,
		LatencyTracker:     obsComp.LatencyTracker,
		CanaryFilter:       traComp.CanaryFilter,
		TrafficMirror:      traComp.TrafficMirror,
		Degradation:        traComp.Degradation,
		Discovery:          clsComp.Discovery,
		GatewayDiscovery:   clsComp.GatewayDiscovery,
		GatewayEvents:      clsComp.GatewayEvents(),
		Balancer:           clsComp.Balancer,
		ConfigCenter:       clsComp.ConfigCenter,
		ClusterNode:        clsComp.Cluster,
		AlertWebhook:       clsComp.AlertWebhook,
	})

	// TLS加密配置
	gw.tlsConfig = &tls.Config{
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
		PreferServerCipherSuites: true,
		CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP256},
	}
	if cfg.TLS.Enabled && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			tlog.Error("failed to load TLS certificate", "error", err)
		} else {
			gw.tlsConfig.Certificates = []tls.Certificate{cert}
			if strings.EqualFold(cfg.TLS.MinVersion, "TLS1.3") {
				gw.tlsConfig.MinVersion = tls.VersionTLS13
			}
		}
	}

	gw.cfg.Store(cfg)
	gw.ctx = context.Background()

	return gw
}

// StartServices 启动网关特定服务：gRPC服务器、统计HTTP服务、
// Prometheus监控指标、过载保护器、WebSocket心跳检测、配置文件监听
func (g *Gateway) StartServices() {
	cfg := g.cfg.Load().(*config.Config)

	g.overloadProtector.Start()
	go g.wsHeartbeatChecker()
	g.messageIntegrity = NewMessageIntegrity(30000)

	connCheckInterval, _ := time.ParseDuration(g.protection.ConnCheckInterval)
	if connCheckInterval <= 0 {
		connCheckInterval = 5 * time.Minute
	}
	connIdleTimeout, _ := time.ParseDuration(g.protection.ConnIdleTimeout)
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

	g.logicClient.gateway = g
	g.logicClientPool = NewLogicClientPool(g)

	if g.serviceDiscovery != nil {
		g.logicClientPool.SetDiscovery(g.serviceDiscovery)
	}
	if g.balancer != nil {
		g.logicClientPool.SetBalancer(g.balancer)
		g.balancer.SetHealthCheckFunc(func(id, addr string) bool {
			pool := g.logicClientPool
			pool.mu.RLock()
			client, ok := pool.clients[id]
			pool.mu.RUnlock()
			if !ok || client == nil {
				return false
			}
			return client.IsConnected()
		})
	}

	// 网关到网关客户端池仅在集群模式下创建
	if g.gatewayDiscovery != nil {
		g.gatewayClientPool = NewGatewayClientPool(g)
		g.gatewayClientPool.SetDiscovery(g.gatewayDiscovery)
		g.gatewayClientPool.LoadEvents(g.gatewayEvents)
	}

	// gRPC服务器
	grpcPort := fmt.Sprintf(":%d", g.grpcCfg.Port)
	tlog.Info("starting gRPC server", "port", grpcPort)
	go func() {
		if server, err := StartGRPCServer(g, grpcPort, g.grpcCfg.MaxMessageSize, g.grpcCfg.WindowSize); err != nil {
			tlog.Error("failed to start gRPC server", "error", err)
		} else {
			g.grpcServer = server
			tlog.Info("gRPC server started", "port", grpcPort)
		}
	}()

	// 统计HTTP服务器
	g.StartStatsServer(fmt.Sprintf(":%d", cfg.Port))

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
		tlog.Info("starting gateway transport", "addr", addr, "type", transportType)
		go func(addr, transportType string) {
			if err := gnet.Run(g, addr, options...); err != nil {
				tlog.Error("gateway transport stopped", "addr", addr, "error", err)
			}
		}(addr, transportType)
	}
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

	// 动态更新连接限制参数
	g.connectionManager.UpdateLimits(newCfg.Protection.MaxConnections, newCfg.Protection.MaxConnectionsPerIP)
	g.protection.MaxMessagesPerConn = newCfg.Protection.MaxMessagesPerConn

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
	FrameBuf     []byte
}

func (g *Gateway) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnOpen panic recovered", "error", r)
			action = gnet.Close
		}
	}()

	// 连接数限制检查（P0: 防止 OOM 和连接耗尽）
	remoteIP := getRemoteIP(c)
	if !g.connectionManager.CanAccept(remoteIP) {
		tlog.Warn("连接数限制，拒绝新连接",
			"remoteIP", remoteIP,
			"activeConnections", g.connectionManager.GetConnectionCount(),
			"maxConnections", g.connectionManager.maxConnections,
			"ipConnections", g.connectionManager.GetIPConnectionCount(remoteIP),
			"maxPerIP", g.connectionManager.maxConnectionsPerIP)
		return nil, gnet.Close
	}

	localAddr := c.LocalAddr().String()
	isWS := false
	g.transportType.Range(func(key, value interface{}) bool {
		port := key.(string)
		t := value.(string)
		if strings.HasSuffix(localAddr, ":"+port) && t == "websocket" {
			isWS = true
			return false
		}
		return true
	})

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

	g.connectionsTotal.Add(1)
	g.connectionsActive.Add(1)

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
		if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
			// P1: 记录连接生命周期指标
			duration := time.Now().UnixMilli() - conn.CreatedAt
			g.connectionDurationSum.Add(duration)
			g.connectionDurationCount.Add(1)
			g.connectionDurationTracker.Record(time.Duration(duration) * time.Millisecond)
			g.notifyLogicOffline(conn)
		}
		g.connectionManager.RemoveConnection(connectionID)
		g.connectionsActive.Add(-1)
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
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	// 使用正常协议路径处理每个完整帧。逻辑流携带每条消息的真实客户端命令；它不定义网关私有的批处理命令。
	maxFrame := g.protection.MaxFrameSize
	for len(ctx.FrameBuf) >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[:4])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			ctx.FrameBuf = nil
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

		if ret := g.handleTCPRequest(c, frameData); ret == gnet.Close {
			return gnet.Close
		}
	}

	return
}

// handleBatchTraffic 将 FrameBuf 中的所有完整帧收集到单个 RouteBatch 消息中，
// 并通过一次 SendMessage 调用转发。这将每帧开销（proto解析、深拷贝、分配、通道发送）
// 降低到每批次。
//
// 零拷贝优化：FrameBuf 已包含 [4字节帧长度][帧数据] 的重复结构，
// 这恰好是 RouteBatch 数据格式。我们不需要将帧复制到单独的批处理缓冲区中，
// 而是将 FrameBuf 切片的所有权转移给批处理消息，让下一次 OnTraffic 调用分配
// 新缓冲区。这消除了在2000万QPS时导致GC压力的每批256KB分配和拷贝。
//
// 批处理格式（单连接）：RouteBatch 消息包含：
//
//	ConnectionId = ctx.ConnectionID（此连接的所有帧共享）
//	Data = FrameBuf[:offset]（转移所有权，零拷贝）
//	Cmd = 帧数量
//
// 逻辑服反序列化每个负载以获取路由并分别分发。
// 如果内部消息没有 ConnectionId，则从外部消息设置。
func (g *Gateway) handleBatchTraffic(c gnet.Conn, ctx *ConnContext) (action gnet.Action) {
	maxFrame := g.protection.MaxFrameSize

	// 统计完整帧数并找到分割点。
	// FrameBuf 格式：[4字节帧长度][帧数据] 重复
	// = 逻辑服所需的批处理格式。
	offset := 0
	batchCount := 0
	for offset+4 <= len(ctx.FrameBuf) {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[offset : offset+4])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			ctx.FrameBuf = nil
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if offset+totalLen > len(ctx.FrameBuf) {
			break // 不完整帧，等待更多数据
		}
		if batchCount == 0 {
			cmd, _, _, ok := gateway.ExtractMessageFrame(ctx.FrameBuf[offset+4 : offset+totalLen])
			if !ok {
				ctx.FrameBuf = nil
				return gnet.Close
			}
			if cmd == gateway.CmdLoginGate {
				// 登录命令不能被批处理，需要立即处理
				frameData := append([]byte(nil), ctx.FrameBuf[offset+4:offset+totalLen]...)
				ctx.FrameBuf = append(ctx.FrameBuf[:0], ctx.FrameBuf[offset+totalLen:]...)
				return g.handleTCPRequest(c, frameData)
			}
		}

		offset += totalLen
		batchCount++
	}

	if batchCount == 0 {
		return
	}

	conn := g.connectionManager.GetConnection(ctx.ConnectionID)
	if conn != nil && !conn.IsAuthenticated() {
		// 检查每帧的命令——未认证连接的批处理中，如果首帧是预认证命令，不允许混入非预认证命令。
		off := 0
		for off+4 <= len(ctx.FrameBuf) {
			frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[off : off+4])
			totalLen := 4 + int(frameLen)
			if off+totalLen > len(ctx.FrameBuf) {
				break
			}
			cmd, _, _, ok := gateway.ExtractMessageFrame(ctx.FrameBuf[off+4 : off+totalLen])
			if !ok || !g.isPreAuthCommand(cmd) {
				errorResp := newErrorResponse("error", "unauthorized", "connection not authenticated", "")
				respData, _ := proto.Marshal(errorResp)
				writeFrame(c, respData)
				g.messagesDroppedAuth.Add(int64(batchCount))
				return gnet.Close
			}
			off += totalLen
		}
	}

	g.messagesReceived.Add(int64(batchCount))

	// 首先分割 FrameBuf：将完整帧转移到 batchData，不完整尾部保留在 FrameBuf 中。
	// 这必须在任何提前返回（过载、无逻辑服）之前发生，以防止帧在下次 OnTraffic 调用时被重复计数。
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

	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(int64(batchCount))
		g.messagesDroppedOverload.Add(int64(batchCount))
		errorResp := newErrorResponse("error", "server overload", "cpu threshold exceeded", "")
		respData, _ := proto.Marshal(errorResp)
		writeFrame(c, respData)
		return
	}

	conn = g.connectionManager.GetConnection(ctx.ConnectionID)
	if conn == nil || !conn.IsBound() {
		return gnet.Close
	}
	logicClient := g.GetLogicClient(conn.GetServerID())
	if logicClient == nil {
		g.messagesDroppedNoLogicNotConnected.Add(int64(batchCount))
		return
	}

	batchMsg := &protoGw.StreamData{
		SessionId: ctx.ConnectionID,
		Data:      batchData,
		Cmd:       int32(batchCount),
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

func (g *Gateway) isPreAuthCommand(cmd int32) bool {
	// 逻辑登录命令是稳定的协议边界。将其作为内置回退保留，
	// 以便旧的动态配置在命令范围迁移后不会锁定所有新连接的会话。
	if cmd == gateway.CmdLogicLoginReq {
		return true
	}
	for _, allowed := range g.protection.PreAuthCommands {
		if cmd == allowed {
			return true
		}
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

func (g *Gateway) GetLogicClient(serverID string) LogicClientProvider {
	if g.logicClientPool == nil {
		return nil
	}
	return g.logicClientPool.GetClient(serverID)
}

// LookupLogicAddress 通过 serverID 在服务发现中查询逻辑服地址。
func (g *Gateway) LookupLogicAddress(serverID string) string {
	if g.logicClientPool == nil {
		return ""
	}
	return g.logicClientPool.LookupAddress(serverID)
}

func (g *Gateway) GetGatewayClient(serverID string) GatewayClientProvider {
	if g.gatewayClientPool == nil {
		return nil
	}
	return g.gatewayClientPool.GetClient(serverID)
}

func (g *Gateway) validateLoginKey(userID, loginKey string) bool {
	mode := g.protection.LoginAuth.Mode
	switch mode {
	case "hmac":
		return validateHMACLoginKey(userID, loginKey, g.protection.LoginAuth.Secret)
	case "delegate":
		// 将验证委托给逻辑服——网关信任逻辑服的响应。如果逻辑服拒绝用户，它不会返回 UserKey。
		return true
	default: // "none" 或空值
		return true
	}
}

func (g *Gateway) handleLoginGate(c gnet.Conn, connectionID string, message *protoGw.StreamData) gnet.Action {
	req := new(protoGw.LoginGateReq)
	writeAck := func(code int32, text, serverID string) {
		ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: gateway.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
	}
	if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
		writeAck(400, "invalid login gate request", req.ServerId)
		return gnet.None
	}
	if !g.validateLoginKey(req.UserId, req.LoginKey) {
		writeAck(401, "invalid login key", req.ServerId)
		return gnet.None
	}
	g.connectionManager.SetConnectionServerID(connectionID, req.ServerId)
	userUUID := req.UserId
	if userUUID == "" {
		userUUID = connectionID
	}
	fullUUID := req.ServerId + ":" + userUUID

	// P1: 主动关闭同用户的旧连接（重连场景），防止资源泄漏
	if oldConnID, exists := g.connectionManager.GetUserConnection(fullUUID); exists && oldConnID != connectionID {
		if oldConn := g.connectionManager.GetConnection(oldConnID); oldConn != nil {
			tlog.Info("检测到重复登录，关闭旧连接",
				"oldConnectionID", oldConnID,
				"newConnectionID", connectionID,
				"userUUID", fullUUID)
			g.notifyLogicOffline(oldConn)
			if oldConn.Conn != nil {
				oldConn.Conn.Close()
			}
		}
	}

	// 保持选中的逻辑服在网关侧的身份标识中，以防止逻辑分片之间的会话索引冲突。
	g.connectionManager.UpdateConnectionUserUUID(connectionID, fullUUID)
	writeAck(0, "ok", req.ServerId)

	// 将登录 StreamData 转发给逻辑服，以便它注册会话并处理登录特定逻辑（如加入群组、设置状态）。
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj != nil {
		if lc := g.GetLogicClient(req.ServerId); lc != nil {
			forwardMsg := &protoGw.StreamData{
				SessionId: connectionID,
				UserKey:   connObj.GetUserUUID(),
				Data:      append([]byte(nil), message.Data...),
				Cmd:       message.Cmd,
				SeqId:     message.SeqId,
			}
			_ = lc.SendMessage(forwardMsg)
		}
	}

	return gnet.None
}

func (g *Gateway) notifyLogicOffline(conn *Connection) {
	serverID := conn.GetServerID()
	if serverID == "" {
		return
	}
	client := g.GetLogicClient(serverID)
	if client == nil {
		return
	}
	ntf := &protoGw.UserOfflineNtf{SessionId: conn.ID(), UserKey: conn.GetUserUUID(), ServerId: serverID, OfflineTime: time.Now().UnixMilli()}
	body, _ := proto.Marshal(ntf)
	_ = client.SendMessage(&protoGw.StreamData{SessionId: conn.ID(), UserKey: conn.GetUserUUID(), Cmd: gateway.CmdUserOffline, Data: body})
}

func (g *Gateway) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		return
	}

	g.messagesReceived.Add(1)

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

	message, ok := decodeClientMessage(data)
	if !ok {
		return gnet.Close
	}
	cmd := message.Cmd
	if cmd == gateway.CmdLoginGate {
		return g.handleLoginGate(c, connectionID, message)
	}

	result := g.pipeline.Process(c, data, message, connectionID)
	if result.Error != nil {
		errorResp := newErrorResponse("error", result.Error.Error(), "", "")
		respData, _ := proto.Marshal(errorResp)
		writeFrame(c, respData)
	}
	return result.Action
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
func (g *Gateway) getOrCreateBreaker(route string) *security.CircuitBreaker {
	timeout := 30 * time.Second
	if d, err := time.ParseDuration(g.protection.ConnIdleTimeout); err == nil && d > 0 {
		timeout = d
	}
	return g.circuitBreakerMgr.GetCircuitBreaker(route, 5, 3, timeout)
}

func writeFrame(c gnet.Conn, data []byte) {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	c.Writev([][]byte{header, data})
}

func writeMsgFrame(c gnet.Conn, msg *protoGw.StreamData) {
	data, _ := marshalClientMessage(msg)
	writeFrame(c, data)
}

func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	g.engine = &engine
	return
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

// GetGatewayID 返回在每个后端流中通告的稳定身份标识。
func (g *Gateway) GetGatewayID() string {
	return g.gatewayID
}

// GetServerID 返回用于注册此网关的 etcd 实例ID。
func (g *Gateway) GetServerID() string {
	return g.serverID
}

func (g *Gateway) logMetrics() {
	tlog.Info("gateway metrics",
		"connectionsActive", g.connectionsActive.Load(),
		"connectionsTotal", g.connectionsTotal.Load(),
		"messagesReceived", g.messagesReceived.Load(),
		"messagesForwarded", g.messagesForwarded.Load(),
		"messagesPushed", g.messagesPushedToClient.Load(),
		"messagesProcessed", g.messagesProcessed.Load(),
		"messagesFailed", g.messagesFailed.Load(),
	)
}

func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	g.logMetrics()
	return 1 * time.Second, gnet.None
}

func (g *Gateway) OnShutdown(engine gnet.Engine) {
	g.Close()
}

func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		close(g.stopChan)

		// 阶段1：停止接受新连接（engine.Stop）
		if g.engine != nil {
			g.engine.Stop(context.Background())
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
			tlog.Info("connection drain completed")
		case <-drainTimer.C:
			tlog.Warn("connection drain timed out, forcing close")
		}

		if g.serviceDiscovery != nil {
			g.serviceDiscovery.Destroy()
		}
		if g.gatewayDiscovery != nil {
			g.gatewayDiscovery.Destroy()
		}

		if g.cluster != nil {
			g.cluster.Stop()
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
		if g.messageIntegrity != nil {
			g.messageIntegrity.Stop()
		}
		if g.trafficMirror != nil {
			g.trafficMirror.Stop()
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

		g.connectionManager.StopConnectionChecker()
		g.connectionManager.CloseAllConnections()

		g.overloadProtector.Stop()
		if g.tracer != nil {
			g.tracer.Stop()
		}

		if g.promExporter != nil {
			g.promExporter.Destroy()
		}
		g.StopStatsServer()

		tlog.Info("gateway closed")
	})
}

// drainConnections 等待所有连接完成进行中的工作。它将每个连接转换为 StateClosed 状态并等待连接管理器清理。
func (g *Gateway) drainConnections(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// 将所有 Forward 状态的连接转换为 Closed（拒绝新消息）
	g.connectionManager.connections.Range(func(_ string, conn *Connection) bool {
		if conn.GetState() == StateForward {
			conn.SetState(StateForward, StateClosed)
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

// validateHMACLoginKey 验证 login_key 是否为 HMAC-SHA256(userID, secret) 的值。
// 客户端应计算：loginKey = hex(HMAC-SHA256(secret, userID))。
func validateHMACLoginKey(userID, loginKey, secret string) bool {
	if secret == "" || userID == "" || loginKey == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userID))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(loginKey), []byte(expected))
}
