package gateway

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/spf13/cast"
	"github.com/streasure/util/component"
	"github.com/streasure/util/monitor"
	"github.com/streasure/util/nacos"
	"github.com/streasure/sgate/cluster"
	"github.com/streasure/sgate/obs"
	"github.com/streasure/sgate/security"
	"github.com/streasure/sgate/traffic"
	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/internal/config"
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
	transportType      sync.Map
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
	tracer             *obs.Tracer
	logicClient        *LogicClient
	logicClientPool    *LogicClientPool
	serviceDiscovery   *nacos.Discovery
	overloadProtector  *OverloadProtector
	grpcServer         *grpc.Server
	promExporter       *monitor.Exporter // Prometheus 指标导出器（enabled=false 时为 nil）
	statsServer        *http.Server
	msgRate            *messageRateTracker // 消息速率滚动窗口（供 Stats() 计算 msgs/sec）
	zone               string
	protection         config.ProtectionConfig
	grpcCfg            config.GRPCConfig
	streamCfg          config.StreamConfig
	// 安全防护组件
	whitelistBlacklist *security.WhitelistBlacklist
	circuitBreakerMgr  *security.CircuitBreakerManager
	rateLimiter        *security.RateLimiter
	waf                *security.WAF
	cluster            *cluster.Cluster
	latencyTracker     *obs.LatencyTracker

	// 企业级扩展组件
	filterChain   *types.FilterChain        // SPI 过滤器链
	jwtAuth       *security.JWTAuthFilter   // JWT 鉴权
	balancer      *cluster.Balancer         // 负载均衡 + 故障节点摘除
	degradation   *traffic.DegradationManager // 降级管理
	configCenter  cluster.ConfigCenter      // 配置中心（Nacos/Apollo/etcd/Consul）
	otelTracer    *obs.OTelTracer           // 分布式追踪导出
	alertWebhook  *cluster.AlertWebhook     // 告警 webhook（企业微信/钉钉）
	canaryFilter  *traffic.CanaryFilter     // 灰度发布
	trafficMirror *traffic.TrafficMirror    // 流量镜像
	logSanitizer  *obs.LogSanitizer         // 日志脱敏

	// 转发统计计数器（用于极限压测时观测 sgate 转发能力）
	connectionsTotal                    atomic.Int64
	connectionsActive                   atomic.Int64
	messagesForwarded                   atomic.Int64
	messagesDroppedOverload             atomic.Int64
	messagesDroppedFull                 atomic.Int64
	messagesDroppedNoLogic              atomic.Int64
	messagesDroppedNoLogicNotConnected  atomic.Int64
	messagesReceived                    atomic.Int64
	messagesPushedToClient              atomic.Int64
	messagesPushDroppedNoConn           atomic.Int64
	messagesProcessed                   atomic.Int64
	messagesFailed                      atomic.Int64
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

	// Start all gateway services
	gw.StartServices()

	return gw
}

// StartServices launches gateway-specific services: gRPC server, stats HTTP,
// Prometheus metrics, overload protector, WebSocket heartbeat, config watcher.
func (g *Gateway) StartServices() {
	cfg := g.cfg.Load().(*config.Config)

	g.overloadProtector.Start()
	go g.wsHeartbeatChecker()
	g.messageIntegrity = NewMessageIntegrity(30000)
	g.versionNegotiation = NewVersionNegotiation([]string{"1.0.0", "1.1.0", "2.0.0"}, 10*time.Second)

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

	// Static connection pre-warm (no discovery)
	if g.serviceDiscovery == nil {
		tlog.Info("service discovery disabled, using static logic server connection")
		go func() {
			address := g.grpcCfg.LogicAddr
			if address == "" {
				address = fmt.Sprintf("localhost:%d", g.grpcCfg.Port)
			}
			backoff := time.Second
			for {
				tlog.Info("pre-warming logic server connection", "address", address)
				err := g.logicClient.Connect(address)
				if err == nil {
					tlog.Info("successfully connected to logic server (pre-warmed)")
					return
				}
				if errors.Is(err, ErrConnectionClosing) {
					return
				}
				tlog.Error("failed to connect to logic server, retrying", "error", err, "backoff", backoff)
				time.Sleep(backoff)
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}()
	}

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

	// Prometheus
	if cfg.Monitoring.Prometheus.Enabled {
		g.promExporter = monitor.NewExporter(monitor.ExporterConfig{
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
		g.connectionManager.RemoveConnection(connectionID)
		g.versionNegotiation.RemoveClientVersion(connectionID)
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
		var span *obs.TraceSpan
		if g.tracer != nil {
			traceID := obs.GenerateTraceID()
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
func (g *Gateway) getOrCreateBreaker(route string) *security.CircuitBreaker {
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

		if g.serviceDiscovery != nil {
			g.serviceDiscovery.Destroy()
		}

		if g.cluster != nil {
			g.cluster.Stop()
		}

		if g.logicClientPool != nil {
			g.logicClientPool.Close()
		}

		if g.logicClient != nil {
			g.logicClient.Close()
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
