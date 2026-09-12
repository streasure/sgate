package gateway

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
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/prometheus"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protoGw.StreamData) error
}

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
	serviceDiscovery  *etcd.Component
	gatewayDiscovery  *etcd.Component // discovery for other gateways (Gateway:{zone})
	gatewayEvents     []etcd.ServiceEvent
	overloadProtector *OverloadProtector
	grpcServer        *grpc.Server
	promExporter      *prometheus.Exporter // Prometheus 指标导出器（enabled=false 时为 nil）
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
	engine             *gnet.Engine // stored on boot for graceful shutdown

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
}

func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType.Store(port, transportType)
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：logic->sgate->client）
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

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

	// Create shared FilterChain
	fc := types.NewFilterChain()

	// Create all lifecycle components
	secComp := NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
	obsComp := NewObservabilityComponent(cfg.OTelTracer, pprofAddrFromEnv(), fc)
	traComp := NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
	clsComp := NewClusterComponent(*cfg, cfg.GRPC.Port, nil)

	// Init + Start all components
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

	// Load SPI filters from config
	for _, fi := range cfg.FilterChain.Filters {
		if err := fc.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn("failed to load filter from config", "name", fi.Name, "error", err)
		}
	}

	// Build Gateway via dependency injection
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

	// TLS
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

// StartServices launches gateway-specific services: gRPC server, stats HTTP,
// Prometheus metrics, overload protector, WebSocket heartbeat, config watcher.
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

	// Gateway-to-gateway client pool uses its dedicated Gateway:{zone} watcher.
	g.gatewayClientPool = NewGatewayClientPool(g)
	if g.gatewayDiscovery != nil {
		g.gatewayClientPool.SetDiscovery(g.gatewayDiscovery)
	}
	g.gatewayClientPool.LoadEvents(g.gatewayEvents)

	// gRPC server
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

	// Stats HTTP server
	g.StartStatsServer(fmt.Sprintf(":%d", cfg.Port))

	// Start TCP/WS transports
	g.startTransports(cfg)

	// Prometheus
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

func pprofAddrFromEnv() string {
	if addr := os.Getenv("SGATE_PPROF_ADDR"); addr != "" {
		return addr
	}
	return ":6060"
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
	wsDebug(fmt.Sprintf("handleNormalTraffic enter: fd=%d", c.Fd()))
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
	wsDebug(fmt.Sprintf("handleNormalTraffic: connCtx type=%T dataLen=%d", connCtx, len(data)))
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

	// Process each complete frame using the normal protocol path. The logic
	// stream carries the real client command for every message; it does not
	// define a gateway-private batch command.
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
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if offset+totalLen > len(ctx.FrameBuf) {
			break // incomplete frame, wait for more data
		}
		if batchCount == 0 {
			cmd, _, _, ok := gateway.ExtractMessageFrame(ctx.FrameBuf[offset+4 : offset+totalLen])
			if !ok {
				ctx.FrameBuf = nil
				return gnet.Close
			}
			if cmd == gateway.CmdLoginGate {
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
		// Check every frame's cmd — a batch with a pre-auth first frame must not
		// smuggle non-pre-auth commands through when the connection is unauthenticated.
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
	// The logic login command is the stable protocol boundary. Keep it as a
	// built-in fallback so an older dynamic configuration cannot lock out all
	// newly connected sessions after a command-range migration.
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

// LookupLogicAddress queries the service discovery for a logic server address by serverID.
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
		// Delegate validation to logic server — the gateway trusts the
		// logic response. If logic rejects the user, it will not send
		// back a UserKey, and the connection stays unauthenticated.
		return true
	default: // "none" or empty
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
	// Keep the selected logic server in the gateway-side identity so session
	// indexes cannot collide across logic shards.
	g.connectionManager.UpdateConnectionUserUUID(connectionID, req.ServerId+":"+userUUID)
	writeAck(0, "ok", req.ServerId)

	// Forward login StreamData to logic so it can register session and
	// handle login-specific logic (e.g. joining groups, setting up state).
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

// GetGatewayID returns the stable identity advertised with every backend stream.
func (g *Gateway) GetGatewayID() string {
	return g.gatewayID
}

// GetServerID returns the etcd instance ID used to register this gateway.
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

		// Phase 1: Stop accepting new connections (engine.Stop)
		if g.engine != nil {
			g.engine.Stop(context.Background())
		}

		// Phase 2: Drain in-flight messages (max 2min)
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

// drainConnections waits for all connections to finish in-flight work.
// It transitions each connection to StateClosed and waits for the
// connection manager to clean up.
func (g *Gateway) drainConnections(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// Transition all Forward connections to Closed (reject new messages)
	g.connectionManager.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		if conn.GetState() == StateForward {
			conn.SetState(StateForward, StateClosed)
		}
		return true
	})

	// Wait for connection count to drop to 0 or timeout
	for time.Now().Before(deadline) {
		if g.connectionManager.GetConnectionCount() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// validateHMACLoginKey validates login_key as HMAC-SHA256(userID, secret).
// The client is expected to compute: loginKey = hex(HMAC-SHA256(secret, userID)).
func validateHMACLoginKey(userID, loginKey, secret string) bool {
	if secret == "" || userID == "" || loginKey == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userID))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(loginKey), []byte(expected))
}
