package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/logiclink"

	"github.com/streasure/sgate/internal/dataplane"

	"github.com/panjf2000/gnet/v2"
	protoLogin "github.com/streasure/protocol/loginserver"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/policy"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/transport"
	"github.com/streasure/util/gatewayutil"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/udiscovery"
	"github.com/streasure/util/ugrpc"
)

// Gateway 表示网关实例，负责连接接入、协议处理、逻辑转发和组件生命周期。
type Gateway struct {
	connectionManager *dataplane.ConnectionManager
	stopChan          chan struct{}
	closing           atomic.Bool   // 关闭流程状态：首个调用者执行，其余等待完成
	closeDone         chan struct{} // 关闭流程完成信号（Close 等待者挂在此）
	transportType     sync.Map
	ctx               context.Context
	ctxCancel         context.CancelFunc // 关闭 g.ctx（closeBody 调用）
	tlsConfig         *tls.Config
	gatewayID         string
	cfg               atomic.Pointer[config.Config]
	wsConnections     sync.Map
	configPath        string
	res               *component.Resources // 运行时资源容器（组合根注入，组件 Init 写入）
	configUpdateChan  chan *config.Config
	messageIntegrity  *MessageIntegrity
	tracer            *obs.Tracer
	logicClient       *logiclink.LogicClient
	logicClientPool   *logiclink.LogicClientPool
	serverID          string
	overloadProtector *OverloadProtector
	grpcServer        *ugrpc.Server
	statsServer       *http.Server
	msgRate           *messageRateTracker // 消息速率滚动窗口（供 /health 与速率展示计算 msgs/sec）
	zone              string
	protection        atomic.Pointer[config.ProtectionConfig] // 热路径无锁读取
	grpcCfg           atomic.Pointer[config.GRPCConfig]       // 热更新原子替换
	streamCfg         atomic.Pointer[config.StreamConfig]     // 热更新原子替换
	maintenance       *maintenanceState                       // 切维护状态（serverId/zone → reason）
	// 安全防护组件
	whitelistBlacklist *security.WhitelistBlacklist
	circuitBreakerMgr  *security.CircuitBreakerManager
	// breakerParams 熔断器参数快照：构造/热更时解析一次，
	// 热路径（每消息两次 getOrCreateBreaker）只做一次原子读，
	// 避免每消息 cfg.Load + time.ParseDuration。
	breakerParams  atomic.Pointer[breakerParams]
	rateLimiter    *security.RateLimiter
	waf            *security.WAF
	latencyTracker *obs.LatencyTracker
	latencySeq     int64         // 延迟采样序号：每 8 条消息取 1 条计时（QPC 为 syscall，见 pipeline）
	engines        []gnet.Engine // 所有 transport engine，关闭时逐个 Stop
	enginesMu      sync.Mutex
	grpcServerMu   sync.Mutex // 保护 grpcServer 赋值/读取（启动协程与 Close 竞态）

	// 企业级扩展组件
	filterChain   *policy.FilterChain         // SPI 过滤器链
	jwtAuth       *security.JWTAuthFilter     // JWT 鉴权
	degradation   *traffic.DegradationManager // 降级管理
	configCenter  cluster.ConfigCenter        // etcd 配置中心
	otelTracer    *obs.OTelTracer             // 分布式追踪导出
	alertWebhook  *cluster.AlertWebhook       // 告警 webhook（企业微信/钉钉）
	canaryFilter  *traffic.CanaryFilter       // 灰度发布
	trafficMirror *traffic.TrafficMirror      // 流量镜像

	// trustedProxies 反向代理信任前缀（IP/CIDR）：仅 WS 握手解析 XFF/X-Real-IP 时用。
	// Init 时从 config.Security.TrustedProxies 解析一次；空列表=不信任任何代理头。
	trustedProxies []netip.Prefix

	// 转发统计计数器（用于极限压测时观测 sgate 转发能力）
	pipeline                           *MessagePipeline
	loginSlots                         chan struct{} // 并发登录协程上限（finishLoginGate）
	connectionsTotal                   atomic.Int64
	connectionsActive                  atomic.Int64
	messagesForwarded                  atomic.Int64
	messagesDroppedOverload            atomic.Int64
	messagesDroppedFull                atomic.Int64
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
	messagesDroppedUnsupported atomic.Int64 // 传输不承载（UDP 业务消息）

	// WS+UDP 关联通道埋点（docs/udp-ws-design.md §8.4，方案 B 数据驱动触发条件）。
	udpAssocSweptWSSurviving  atomic.Int64 // 关联空闲淘汰时 WS 会话仍存活（stale 关联缺口签名）
	udpAssocUnassociatedDrops atomic.Int64 // 已开启 UDP 时未关联源地址丢包（源地址漂移旁证）

	// udpAssoc WS+UDP 关联表（docs/udp-ws-design.md §5.1）。
	// newGateway 无条件初始化（方法可安全调用），实际收发受 udp.enabled 控制。
	udpAssoc *udpAssocTable

	// 连接生命周期指标
	connectionDurationSum     atomic.Int64        // 连接总存活时长（毫秒），用于计算平均值
	connectionDurationCount   atomic.Int64        // 已关闭连接数，用于计算平均值
	connectionDurationTracker *obs.LatencyTracker // 连接时长分位数追踪器
	loginServerMu             sync.RWMutex
	loginServerClient         protoLogin.LoginServiceClient                     // 测试注入的覆盖客户端（生产恒为 nil，生产路径经 loginClient 走句柄）
	loginService              *udiscovery.Client[protoLogin.LoginServiceClient] // loginserver 客户端句柄（发现由网关自持，见 initLoginService）
	pipelineWorkerPool        *PipelineWorkerPool                               // 异步 pipeline 工作池
	shardedCoalescer          *dataplane.ShardedWriteCoalescer                  // 分片写合并器（推送路径）
}

// SetTransportType 设置监听端口对应的传输类型。
func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType.Store(port, transportType)
}

// getProtection 无锁读取 ProtectionConfig（热路径使用）。
func (g *Gateway) getProtection() config.ProtectionConfig {
	if p := g.protection.Load(); p != nil {
		return *p
	}
	return config.ProtectionConfig{}
}

// breakerParams 预解析的熔断器参数；nil 表示熔断器未启用（热路径直接短路）。
type breakerParams struct {
	failureThreshold int
	successThreshold int
	timeout          time.Duration
}

// breakerParamsFrom 从配置解析熔断器参数，未启用时返回 nil。
func breakerParamsFrom(cfg *config.Config) *breakerParams {
	if cfg == nil || !cfg.Security.CircuitBreaker.Enabled {
		return nil
	}
	cb := cfg.Security.CircuitBreaker
	p := &breakerParams{
		failureThreshold: 5,
		successThreshold: 3,
		timeout:          30 * time.Second,
	}
	if cb.FailureThreshold > 0 {
		p.failureThreshold = cb.FailureThreshold
	}
	if cb.SuccessThreshold > 0 {
		p.successThreshold = cb.SuccessThreshold
	}
	if d := gatewayutil.ParseDurationDefault(cb.Timeout, 0); d > 0 {
		p.timeout = d
	}
	return p
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：逻辑服到网关再到客户端）。
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// IsTrustedProxy 判断对端 IP 是否在反代信任列表（IP 或 CIDR）。
// 仅 WS 握手解析 XFF 时调用；TCP 无 HTTP 头不走此路径。
func (g *Gateway) IsTrustedProxy(host string) bool {
	if len(g.trustedProxies) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range g.trustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

// NewGateway 创建网关实例：配置、配置文件路径与运行时资源容器均由组合根
// （main）显式传入；各组件按 Order 写入 res，本实例 Init（Order 最后）读取。
func NewGateway(cfg *config.Config, configPath string, res *component.Resources) *Gateway {
	return newGateway(cfg, configPath, res)
}

// buildTLSConfig 根据配置构建 TLS 配置；证书加载失败时保持无证书（仅记录错误）。
func buildTLSConfig(ctx context.Context, cfg *config.Config) *tls.Config {
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
		tlog.Error(ctx, "failed to load TLS certificate error=%v", err)
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
	// 前置组件已按 Order 完成 Init 并写入注入的资源容器，此处统一读取。
	res := g.res
	g.filterChain = res.FC
	g.whitelistBlacklist = res.WhitelistBlacklist
	g.waf = res.WAF
	g.rateLimiter = res.RateLimiter
	g.jwtAuth = res.JWTAuth
	g.circuitBreakerMgr = res.CircuitBreakerMgr
	g.tracer = res.Tracer
	g.otelTracer = res.OTelTracer
	g.latencyTracker = res.LatencyTracker
	g.canaryFilter = res.CanaryFilter
	g.trafficMirror = res.TrafficMirror
	g.degradation = res.Degradation
	g.configCenter = res.ConfigCenter
	g.alertWebhook = res.AlertWebhook
	// 反代信任列表（仅 WS XFF 用；配置已 Validate，此处静默跳过非法项）
	if cfg := g.cfg.Load(); cfg != nil {
		for _, s := range cfg.Security.TrustedProxies {
			if p, err := netutil.ParseIPOrPrefix(s); err == nil {
				g.trustedProxies = append(g.trustedProxies, p)
			}
		}
		if err := g.validateSecurityWiring(cfg); err != nil {
			return err
		}
	}
	// 管理器主动移除连接（空闲超时/关闭全部）时先发离线通知
	g.connectionManager.SetOnRemoveClose(g.notifyLogicOffline)
	return nil
}

// validateSecurityWiring 启动期装配校验：配置已启用的安全环节对应资源为 nil
// 说明组件未装配（漏 Add/顺序错位），运行期安全链会静默放行（fail-open）。
// 此处 fail-fast，拒绝带残缺安全链的启动。
func (g *Gateway) validateSecurityWiring(cfg *config.Config) error {
	if cfg.Security.Enabled && g.whitelistBlacklist == nil {
		return fmt.Errorf("security.enabled=true but whitelist/blacklist resource is nil (security component not wired)")
	}
	if cfg.Security.RateLimit.Enabled && g.rateLimiter == nil {
		return fmt.Errorf("security.rateLimit.enabled=true but rate limiter resource is nil (security component not wired)")
	}
	if cfg.WAF.Enabled && g.waf == nil {
		return fmt.Errorf("waf.enabled=true but WAF resource is nil (security component not wired)")
	}
	if cfg.Security.CircuitBreaker.Enabled && g.circuitBreakerMgr == nil {
		return fmt.Errorf("security.circuitBreaker.enabled=true but circuit breaker resource is nil (security component not wired)")
	}
	if cfg.JWTAuth.Enabled && g.jwtAuth == nil {
		return fmt.Errorf("jwtAuth.enabled=true but JWT auth resource is nil (security component not wired)")
	}
	if cfg.FilterChain.Enabled && len(cfg.FilterChain.Filters) > 0 {
		if g.filterChain == nil || g.filterChain.Len() == 0 {
			return fmt.Errorf("filterChain.enabled=true with %d filters configured but filter chain is empty", len(cfg.FilterChain.Filters))
		}
	}
	return nil
}
func (g *Gateway) Start() error {
	// loginserver 发现由网关自持（消费方持有发现，不依赖共享组件）
	if err := g.initLoginService(); err != nil {
		return err
	}
	if err := g.StartServices(); err != nil {
		return err
	}
	return nil
}

func (g *Gateway) Destroy() { g.Close() }

// StartServices 启动网关特定服务：gRPC服务器、统计HTTP服务、
// 过载保护器、WebSocket心跳检测、配置文件监听
func (g *Gateway) StartServices() error {
	cfg := g.cfg.Load()
	protection := g.getProtection()
	// 压测模式：跳过与压测无关的后台周期任务，避免与热路径争抢调度。
	benchMode := protection.BenchMode

	// 过载采样器始终启动：benchMode 下为 observe-only（见 OverloadProtector.observeOnly），
	// 保证 /stats 的 cpuPercent/memPercent 有值；benchMode 仅跳过 WS 心跳检测。
	g.overloadProtector.Start()
	if !benchMode {
		panicutil.GoLoop(g.ctx, "gateway.wsHeartbeatChecker", g.wsHeartbeatChecker)
		// 封禁收敛 P2：BatchValidate 周期安全网（enabled=false/batchInterval<=0 时内部自停）
		g.startBatchValidateSweep()
	}
	g.messageIntegrity = NewMessageIntegrity(30000)

	if cfg.Pipeline.AsyncEnabled {
		g.pipelineWorkerPool = NewPipelineWorkerPool(g, cfg.Pipeline)
	}

	// 初始化分片写合并器（推送路径：logic → client）
	g.shardedCoalescer = dataplane.NewShardedWriteCoalescer(g.connectionManager, 16)

	if !benchMode {
		connCheckInterval := gatewayutil.ParseDurationDefault(protection.ConnCheckInterval, 5*time.Minute)
		if connCheckInterval <= 0 {
			connCheckInterval = 5 * time.Minute
		}
		connIdleTimeout := gatewayutil.ParseDurationDefault(protection.ConnIdleTimeout, 30*time.Second)
		if connIdleTimeout <= 0 {
			connIdleTimeout = 30 * time.Second
		}
		g.connectionManager.StartConnectionChecker(connIdleTimeout, connCheckInterval)
	}

	if g.configCenter != nil {
		g.startConfigCenterWatcher()
	}
	panicutil.GoLoop(g.ctx, "gateway.configWatcher", g.configWatcher)
	// UDP 关联空闲淘汰/过期 token 清扫（空表扫描开销可忽略，不依赖 udp.enabled 状态）
	panicutil.GoLoop(g.ctx, "gateway.udpAssocJanitor", g.udpAssocJanitor)
	panicutil.GoLoop(g.ctx, "gateway.configUpdate", func() {
		for {
			select {
			case <-g.stopChan:
				return
			case newCfg := <-g.configUpdateChan:
				g.handleConfigUpdate(newCfg)
			}
		}
	})

	g.logicClient.SetGateway(g)
	g.logicClientPool = logiclink.NewLogicClientPool(g)

	// gRPC服务器
	grpcCfg := g.GetGRPCConfig()
	grpcPort := fmt.Sprintf(":%d", grpcCfg.Port)
	tlog.Info(g.ctx, "starting gRPC server port=%s", grpcPort)
	panicutil.Go(g.ctx, "gateway.grpcBootstrap", func() {
		if server, err := logiclink.StartGRPCServer(g, g.logicClientPool, grpcPort, grpcCfg.MaxMessageSize, grpcCfg.WindowSize, cfg.Security.KickInternalToken); err != nil {
			tlog.Error(g.ctx, "failed to start gRPC server error=%v", err)
		} else {
			g.grpcServerMu.Lock()
			g.grpcServer = server
			g.grpcServerMu.Unlock()
			tlog.Info(g.ctx, "gRPC server started port=%s", grpcPort)
		}
	})

	// 统计HTTP服务器
	g.StartStatsServer(fmt.Sprintf(":%d", cfg.HttpPort))

	// 启动TCP/WebSocket传输层
	g.startTransports(cfg)
	return nil
}

func (g *Gateway) startTransports(cfg *config.Config) {
	for _, tr := range cfg.Transports {
		port := tr.Port
		transportType := tr.Type
		g.SetTransportType(fmt.Sprintf("%d", port), transportType)

		// 协议插件化：从注册表查找协议实现（tcp/websocket/udp）
		// 配置 type 为空时默认 tcp（历史兼容）
		protoName := transportType
		if protoName == "" {
			protoName = "tcp"
		}
		proto := transport.Get(protoName)
		if proto == nil {
			tlog.Error(g.ctx, "unknown protocol type=%s port=%d (no plugin registered)", protoName, port)
			continue
		}

		tlog.Info(g.ctx, "starting gateway transport port=%d type=%s", port, protoName)
		addr := fmt.Sprintf(":%d", port) // 仅用于日志标识
		panicutil.Go(g.ctx, fmt.Sprintf("gateway.transportRun addr=%s", addr), func() {
			if err := proto.Start(g, tr, cfg); err != nil {
				tlog.Error(g.ctx, "gateway transport stopped addr=%s error=%v", addr, err)
			}
		})
	}
}

// wsHeartbeatChecker 定期清理 WS 心跳超时连接。
// 每轮热读 protection 配置（热更生效）；由 panicutil.GoLoop 启动，
// panic 退避重启，单轮异常不终止清理协程。
func (g *Gateway) wsHeartbeatChecker() {
	for {
		interval := time.Duration(g.getProtection().WSCheckInterval) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second // 防 time.NewTimer(0) 忙轮询
		}
		timer := time.NewTimer(interval)
		select {
		case <-g.stopChan:
			timer.Stop()
			return
		case <-timer.C:
		}

		// 检查前热读超时值，使 wsHeartbeatTimeout 热更立即生效
		timeout := time.Duration(g.getProtection().WSHeartbeatTimeout) * time.Second
		g.runWSHeartbeatCheck(timeout)
	}
}

// runWSHeartbeatCheck 执行一轮检查，独立 recover：单轮异常不终止清理协程。
func (g *Gateway) runWSHeartbeatCheck(timeout time.Duration) {
	defer panicutil.Recover(g.ctx, "gateway.runWSHeartbeatCheck")
	g.checkWebSocketConnections(timeout)
}

func (g *Gateway) checkWebSocketConnections(timeout time.Duration) {
	g.wsConnections.Range(func(key, value any) bool {
		conn, ok := key.(*WebSocketConnection)
		if !ok {
			return true
		}
		if time.Since(conn.getLastPingTime()) > timeout {
			tlog.Warn(g.ctx, "WebSocket connection timeout, closing connectionID=%s", conn.ConnectionID())
			if connID := conn.ConnectionID(); connID != "" {
				// 先通知逻辑服离线再关闭/移除，避免 OnClose 时 conn 已查不到而漏发
				if connObj := g.connectionManager.GetConnection(connID); connObj != nil {
					g.notifyLogicOffline(connObj)
				}
				g.releaseUDPAssoc(connID) // 会话是根：WS 心跳超时也拆 UDP 关联
				g.connectionManager.RemoveConnection(connID)
			}
			if conn.Conn != nil {
				conn.Conn.Close()
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
	// 配置文件路径由构造器传入（组合根传 -conf 实际路径，测试传空串走默认回退）
	if g.configPath == "" {
		g.configPath = "config/gateway.yaml"
	}
	if _, err := os.Stat(g.configPath); errors.Is(err, fs.ErrNotExist) {
		altPaths := []string{"config/gateway.yaml", "../config/gateway.yaml", "../../config/gateway.yaml"}
		found := false
		for _, path := range altPaths {
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
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

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			fileInfo, err := os.Stat(g.configPath)
			if err != nil {
				continue
			}

			if fileInfo.ModTime() != lastModTime {
				lastModTime = fileInfo.ModTime()
				newCfg, err := config.Load(g.configPath)
				if err != nil {
					tlog.Error(g.ctx, "config reload failed path=%s error=%v", g.configPath, err)
					continue
				}
				g.tryEnqueueConfig(newCfg)
			}
		}
	}
}

// securityEnableFlags 安全组件启停开关快照。
// 与 security_component.Init 的创建条件一一对应：任一开关变化都意味着
// 需要重建组件实例/重挂过滤器，热更路径无法安全完成 → 拒绝热切。
type securityEnableFlags struct {
	security       bool
	rateLimit      bool
	waf            bool
	jwtAuth        bool
	circuitBreaker bool
}

func securityEnableFlagsOf(c *config.Config) securityEnableFlags {
	return securityEnableFlags{
		security:       c.Security.Enabled,
		rateLimit:      c.Security.RateLimit.Enabled,
		waf:            c.WAF.Enabled,
		jwtAuth:        c.JWTAuth.Enabled,
		circuitBreaker: c.Security.CircuitBreaker.Enabled,
	}
}

func (g *Gateway) currentSecurityEnableFlags() securityEnableFlags {
	if c := g.cfg.Load(); c != nil {
		return securityEnableFlagsOf(c)
	}
	return securityEnableFlags{}
}

func (g *Gateway) handleConfigUpdate(newCfg *config.Config) {
	// 先补齐运行时默认值，再原子替换（防止零值热更把连接踢飞）
	newCfg.ApplyRuntimeDefaults()
	// 必填项校验（启动 Load 会走 Validate，热更路径此前缺失）：
	// 非法配置整体拒绝，保留当前配置继续运行。
	if err := newCfg.Validate(); err != nil {
		tlog.Error(g.ctx, "config update rejected, keeping current config error=%v", err)
		return
	}
	// 安全组件启停开关不可热切：组件只在启动时按配置创建/注册过滤器，
	// 热切 enable 会出现「配置 true 实际不生效（fail-open）」或「配置 false
	// 旧阈值继续拦（fail-stale）」。检测到开关变化整单拒绝，要求重启生效。
	if oldFlags, newFlags := g.currentSecurityEnableFlags(), securityEnableFlagsOf(newCfg); oldFlags != newFlags {
		tlog.Error(g.ctx, "config update rejected: security enable flags cannot hot-toggle, restart required old=%+v new=%+v",
			oldFlags, newFlags)
		return
	}
	g.cfg.Store(newCfg)
	g.breakerParams.Store(breakerParamsFrom(newCfg))
	// 动态更新限流阈值（无需重启）
	if g.rateLimiter != nil && newCfg.Security.RateLimit.Enabled {
		refresh := time.Second
		if d := gatewayutil.ParseDurationDefault(newCfg.Security.RateLimit.TokenRefresh, 0); d > 0 {
			refresh = d
		}
		tokens := newCfg.Security.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = config.DefaultRateLimitMaxTokens
		}
		g.rateLimiter.UpdateRate(tokens, refresh)
		// Set*(0) 恢复默认派生值，配置改回 0 也能热更生效。
		g.rateLimiter.SetRouteMaxTokens(newCfg.Security.RateLimit.RouteMaxTokens)
		g.rateLimiter.SetGlobalMaxTokens(newCfg.Security.RateLimit.GlobalMaxTokens)
		tlog.Info(g.ctx, "rate limiter updated maxTokens=%d refresh=%v routeMax=%d globalMax=%d",
			tokens, refresh, newCfg.Security.RateLimit.RouteMaxTokens, newCfg.Security.RateLimit.GlobalMaxTokens)
	}

	// 动态更新白名单/黑名单（原子替换，避免清空-重填中间态）
	if g.whitelistBlacklist != nil {
		if newCfg.Security.Enabled {
			g.whitelistBlacklist.ReplaceWhitelist(newCfg.Security.Whitelist)
			g.whitelistBlacklist.ReplaceBlacklist(newCfg.Security.Blacklist)
			tlog.Info(g.ctx, "whitelist/blacklist updated whitelist=%d blacklist=%d",
				len(newCfg.Security.Whitelist),
				len(newCfg.Security.Blacklist))
		} else {
			// 关闭安全链时清空残留列表，避免 fail-stale 继续拦截
			g.whitelistBlacklist.ReplaceWhitelist(nil)
			g.whitelistBlacklist.ReplaceBlacklist(nil)
			tlog.Info(g.ctx, "security disabled, whitelist/blacklist cleared")
		}
	}

	// 动态更新过载保护阈值
	if g.overloadProtector != nil {
		g.protection.Store(&newCfg.Protection)
	}

	// 动态更新连接限制参数
	g.connectionManager.UpdateLimits(newCfg.Protection.MaxConnections, newCfg.Protection.MaxConnectionsPerIP)

	// gRPC/stream 运行时快照随配置更新（GetGRPCConfig/GetStreamConfig 消费）
	g.grpcCfg.Store(&newCfg.GRPC)
	g.streamCfg.Store(&newCfg.Stream)

	// 动态更新 JWT 密钥（拒绝空 secret：空键可伪造任意令牌）
	if g.jwtAuth != nil && newCfg.JWTAuth.Enabled {
		if newCfg.JWTAuth.Secret == "" {
			tlog.Warn(g.ctx, "jwt secret update rejected: empty secret")
		} else {
			g.jwtAuth.UpdateSecret(newCfg.JWTAuth.Secret)
			tlog.Info(g.ctx, "jwt secret updated")
		}
	}

	// 动态更新灰度规则
	if g.canaryFilter != nil && newCfg.Canary.Enabled {
		g.canaryFilter.UpdateConfig(newCfg.Canary)
		tlog.Info(g.ctx, "canary config updated percent=%d", newCfg.Canary.Percent)
	}

	// 动态更新流量镜像比例：只热更 percent —— enabled/targetAddr 决定发送端
	// 连接，随组件装配一次性生效，与运行态不一致时告警（否则"改了没生效"不可见）
	if g.trafficMirror != nil && newCfg.TrafficMirror.Enabled {
		g.trafficMirror.UpdatePercent(newCfg.TrafficMirror.Percent)
		tlog.Info(g.ctx, "traffic mirror updated percent=%d", newCfg.TrafficMirror.Percent)
	} else if running := g.trafficMirror != nil; running != newCfg.TrafficMirror.Enabled {
		tlog.Warn(g.ctx, "trafficMirror.enabled not hot-applied, restart required running=%v configured=%v",
			running, newCfg.TrafficMirror.Enabled)
	}

	// 动态更新降级规则
	if g.degradation != nil && newCfg.Degradation.Enabled {
		for _, rc := range newCfg.Degradation.Rules {
			g.degradation.AddRule(rc)
		}
		tlog.Info(g.ctx, "degradation rules updated count=%d", len(newCfg.Degradation.Rules))
	}

	// 登录校验开关随 g.cfg 热更新；打一条便于确认是否误关
	tlog.Info(g.ctx, "login validation enabled=%v", newCfg.LoginValidation.Enabled)

	tlog.Info(g.ctx, "config updated dynamically")
}

func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	g.enginesMu.Lock()
	g.engines = append(g.engines, engine)
	g.enginesMu.Unlock()
	return
}

func (g *Gateway) GetConnectionManager() *dataplane.ConnectionManager {
	return g.connectionManager
}

func (g *Gateway) GetGRPCConfig() config.GRPCConfig {
	if v := g.grpcCfg.Load(); v != nil {
		return *v
	}
	return config.GRPCConfig{}
}

func (g *Gateway) GetStreamConfig() config.StreamConfig {
	if v := g.streamCfg.Load(); v != nil {
		return *v
	}
	return config.StreamConfig{}
}

func (g *Gateway) GetShardedCoalescer() *dataplane.ShardedWriteCoalescer {
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
	tlog.Info(g.ctx, "gateway metrics connectionsActive=%d connectionsTotal=%d messagesReceived=%d messagesForwarded=%d messagesPushed=%d messagesProcessed=%d messagesFailed=%d asyncWriteFailed=%d",
		g.connectionsActive.Load(),
		g.connectionsTotal.Load(),
		g.messagesReceived.Load(),
		g.messagesForwarded.Load(),
		g.messagesPushedToClient.Load(),
		g.messagesProcessed.Load(),
		g.messagesFailed.Load(),
		dataplane.AsyncWriteFailures(),
	)
}

// metricsLogEnabled 返回是否允许每秒打印 metrics 日志（热更新生效）。
// monitoring.disableMetricsLog=true 时关闭（线上默认建议打开该关闭项）。
func (g *Gateway) metricsLogEnabled() bool {
	cfg := g.cfg.Load()
	if cfg == nil {
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

// OnShutdown 在 gnet engine 关闭流程中被回调（事件循环 goroutine 上执行）。
// 必须先摘除该 engine（Close 会对所有 engine 调 Stop，与正在回调的 engine 互等死锁），
// 且绝不能在此等待关闭流程完成：Close 可能正持有关闭状态并阻塞在 engines[i].Stop
// 的 inShutdown 轮询上（gnet 需等本回调返回才置位），等待将形成互锁。
func (g *Gateway) OnShutdown(engine gnet.Engine) {
	g.enginesMu.Lock()
	for i, e := range g.engines {
		if e == engine {
			g.engines = append(g.engines[:i], g.engines[i+1:]...)
			break
		}
	}
	g.enginesMu.Unlock()

	if g.closing.CompareAndSwap(false, true) {
		// 首触发（engine 自行关闭、无人调用过 Close）：在独立协程执行，
		// 本回调立即返回，让 gnet 完成 inShutdown 置位。
		panicutil.Go(g.ctx, "gateway.closeAsync", func() {
			defer close(g.closeDone)
			g.closeBody()
		})
	}
}

// Close 执行一次性关闭流程：首个调用者同步执行，其余调用者等待其完成。
// 不能用 sync.Once：gnet 的 Stop 需等事件循环回调 OnShutdown 返回后才置 inShutdown，
// 若 OnShutdown 重入 Once.Do，将与持有 Once 的 Stop 轮询互锁（优雅退出挂死）。
func (g *Gateway) Close() {
	if !g.closing.CompareAndSwap(false, true) {
		<-g.closeDone
		return
	}
	defer close(g.closeDone)
	g.closeBody()
}

// closeBody 关闭流程主体（Close/OnShutdown 已置 closing，仅执行一次）。
func (g *Gateway) closeBody() {
	close(g.stopChan)
	if g.ctxCancel != nil {
		g.ctxCancel()
	}

	// 阶段0：先拒绝新消息——将 Forward 连接置为 Closed。
	// 必须在停止 engine 之前标记：gnet Stop 会拆除连接，之后状态标记已无意义。
	g.connectionManager.ForEach(func(conn *dataplane.Connection) bool {
		if conn.GetState() == dataplane.StateForward {
			conn.SetState(dataplane.StateForward, dataplane.StateClosed)
		}
		return true
	})

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
	panicutil.Go(g.ctx, "gateway.drainConnections", func() {
		defer close(drainDone)
		g.drainConnections(drainTimeout)
	})

	drainTimer := time.NewTimer(drainTimeout)
	select {
	case <-drainDone:
		if !drainTimer.Stop() {
			select {
			case <-drainTimer.C:
			default:
			}
		}
		tlog.Info(g.ctx, "connection drain completed")
	case <-drainTimer.C:
		tlog.Warn(g.ctx, "connection drain timed out, forcing close")
	}

	if g.logicClientPool != nil {
		g.logicClientPool.Close()
	}
	if g.logicClient != nil {
		g.logicClient.Close()
	}
	if g.loginService != nil {
		_ = g.loginService.Close()
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

	g.StopStatsServer()

	tlog.Info(g.ctx, "gateway closed")
}

// drainConnections 等待连接管理器清理完成（连接数降为 0）或超时。
// Forward→Closed 的状态标记在 closeBody 阶段0（停止 engine 之前）完成。
func (g *Gateway) drainConnections(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

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
// 参数为配置、配置文件路径与资源容器，避免 GatewayDeps 单字段间接层。
func newGateway(cfg *config.Config, configPath string, res *component.Resources) *Gateway {
	if cfg == nil {
		// fail-fast：半初始化 Gateway 会在 Start 阶段 panic，更难排查
		panic("gateway: nil *config.Config passed to constructor")
	}
	if res == nil {
		panic("gateway: nil *component.Resources passed to constructor")
	}
	// 防御：外部传入的 cfg 可能未走 Load（测试/热更新拷贝）
	cfg.ApplyRuntimeDefaults()

	// 网关生命周期 context：newGateway 创建，closeBody 取消（Close 路径）
	ctx, cancel := context.WithCancel(context.Background())

	// 自描述 session ID（roadmap P3-1）：把网关身份编进本进程生成的会话 ID，
	// 定位时 ParseSessionGateway 解析即可得网关，无需外部登记表。
	dataplane.SetSessionGatewayID(cfg.ServerID)

	gw := &Gateway{
		connectionManager: dataplane.NewConnectionManager(cfg.Protection.MaxConnections, cfg.Protection.MaxConnectionsPerIP),
		stopChan:          make(chan struct{}),
		closeDone:         make(chan struct{}),
		serverID:          cfg.ServerID,
		zone:              cfg.Zone,
		configPath:        configPath,
		res:               res,

		configUpdateChan:          make(chan *config.Config, 1),
		overloadProtector:         NewOverloadProtector(cfg.Protection),
		logicClient:               logiclink.NewLogicClient(nil),
		msgRate:                   newMessageRateTracker(60 * time.Second),
		gatewayID:                 gatewayInstanceID(*cfg),
		connectionDurationTracker: obs.NewLatencyTracker(10000),
		tlsConfig:                 buildTLSConfig(ctx, cfg),
	}

	// 存指针副本，避免调用方后续原地改写共享配置
	stored := *cfg
	gw.cfg.Store(&stored)
	gw.breakerParams.Store(breakerParamsFrom(&stored))
	gw.protection.Store(&stored.Protection)
	gw.grpcCfg.Store(&stored.GRPC)
	gw.streamCfg.Store(&stored.Stream)
	gw.maintenance = newMaintenanceState()
	gw.ctx = ctx
	gw.ctxCancel = cancel
	gw.pipeline = NewMessagePipeline(gw)
	gw.loginSlots = make(chan struct{}, 256)
	gw.udpAssoc = newUDPAssocTable()

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

// 编译期校验：Gateway 实现 logiclink.GatewayInterface。
var _ logiclink.GatewayInterface = (*Gateway)(nil)
