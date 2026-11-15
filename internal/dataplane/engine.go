package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	protoLogin "github.com/streasure/protocol/loginserver"
	"github.com/streasure/sgate/internal/admin"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/policy"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/util/gatewayutil"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/udiscovery"
)

// Engine 是数据面引擎：网关业务半区（连接接入视图、消息管道、安全链、
// 统计/健康、配置热更、逻辑链路消费）。生命周期壳（gateway.Gateway）嵌入
// 本类型并只负责 transport/gRPC/TLS 装配与完整关闭流程编排；
// 本类型可独立构造（测试/嵌入场景），收尾走 Close。
type Engine struct {
	connectionManager *ConnectionManager
	stopChan          chan struct{}
	ctx               context.Context
	gatewayID         string
	cfg               atomic.Pointer[config.Config]
	wsConnections     sync.Map
	configPath        string
	res               *component.Resources // 运行时资源容器（组合根注入，组件 Init 写入）
	configUpdateChan  chan *config.Config
	messageIntegrity  *MessageIntegrity
	tracer            *obs.Tracer
	serverID          string
	overloadProtector *OverloadProtector
	statsServer       *http.Server
	msgRate           *messageRateTracker // 消息速率滚动窗口（供 /health 与速率展示计算 msgs/sec）
	zone              string
	protection        atomic.Pointer[config.ProtectionConfig] // 热路径无锁读取
	grpcCfg           atomic.Pointer[config.GRPCConfig]       // 热更新原子替换
	streamCfg         atomic.Pointer[config.StreamConfig]     // 热更新原子替换
	// 管理端 HTTP 面（Bearer 鉴权 + 端点注册），经消费方定义的 admin.Backend 回调本实例
	adminSrv    *admin.Server
	maintenance *maintenanceState // 切维护状态（serverId/zone → reason）
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
	latencySeq     int64 // 延迟采样序号：每 8 条消息取 1 条计时（QPC 为 syscall，见 pipeline）

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
	// 构造期无条件初始化（方法可安全调用），实际收发受 udp.enabled 控制。
	udpAssoc *udpAssocTable

	// 连接生命周期指标
	connectionDurationSum     atomic.Int64        // 连接总存活时长（毫秒），用于计算平均值
	connectionDurationCount   atomic.Int64        // 已关闭连接数，用于计算平均值
	connectionDurationTracker *obs.LatencyTracker // 连接时长分位数追踪器
	loginServerMu             sync.RWMutex
	loginServerClient         protoLogin.LoginServiceClient                     // 测试注入的覆盖客户端（生产恒为 nil，生产路径经 loginClient 走句柄）
	loginService              *udiscovery.Client[protoLogin.LoginServiceClient] // loginserver 客户端句柄（发现由网关自持，见 InitLoginService）
	pipelineWorkerPool        *PipelineWorkerPool                               // 异步 pipeline 工作池
	shardedCoalescer          *ShardedWriteCoalescer                            // 分片写合并器（推送路径）

	// buildVersion 构建注入版本（HealthCheck 上报），组合方构造时传入。
	buildVersion string
	// shutdownHook 完整关闭流程钩子（gateway 壳注入 closeBody：含 transport 停止、
	// 连接排空与 gRPC 停止）；nil 时 transport 关闭握手触发的 Shutdown 回退 Close。
	shutdownHook func()
	// logicLink / logicPool 逻辑链路消费视图（*logiclink.LogicClient /
	// *logiclink.LogicClientPool 实现，gateway 壳 Start 期注入；dataplane 不依赖 logiclink）。
	logicLink LogicLink
	logicPool LogicLinkPool
	// closeOnce 保证 stopChan 只关闭一次（Close 与壳 closeBody 可能来自不同触发者）。
	closeOnce sync.Once
}

// GetProtection 无锁读取 ProtectionConfig（热路径使用；transport.Runtime 实现）。
func (g *Engine) GetProtection() config.ProtectionConfig {
	if p := g.protection.Load(); p != nil {
		return *p
	}
	return config.ProtectionConfig{}
}

// Cfg 返回当前配置快照（热更新原子替换；构造后恒非 nil）。
func (g *Engine) Cfg() *config.Config {
	return g.cfg.Load()
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
func (g *Engine) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// IsTrustedProxy 判断对端 IP 是否在反代信任列表（IP 或 CIDR）。
// 仅 WS 握手解析 XFF 时调用；TCP 无 HTTP 头不走此路径。
func (g *Engine) IsTrustedProxy(host string) bool {
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
func (g *Engine) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

// ===== construction =====

// NewEngine 构造数据面引擎：配置、配置文件路径、运行时资源容器与构建版本
// 由组合方显式传入；各组件按 Order 写入 res，Init（Order 最后）读取。
// 参数为配置、配置文件路径与资源容器，避免依赖单字段间接层。
func NewEngine(ctx context.Context, cfg *config.Config, configPath string, res *component.Resources, version string) *Engine {
	if cfg == nil {
		// fail-fast：半初始化 Engine 会在 Start 阶段 panic，更难排查
		panic("dataplane: nil *config.Config passed to constructor")
	}
	if res == nil {
		panic("dataplane: nil *component.Resources passed to constructor")
	}
	// 防御：外部传入的 cfg 可能未走 Load（测试/热更新拷贝）
	cfg.ApplyRuntimeDefaults()

	// 自描述 session ID（roadmap P3-1）：把网关身份编进本进程生成的会话 ID，
	// 定位时 ParseSessionGateway 解析即可得网关，无需外部登记表。
	SetSessionGatewayID(cfg.ServerID)

	e := &Engine{
		connectionManager: NewConnectionManager(cfg.Protection.MaxConnections, cfg.Protection.MaxConnectionsPerIP),
		stopChan:          make(chan struct{}),
		serverID:          cfg.ServerID,
		zone:              cfg.Zone,
		configPath:        configPath,
		res:               res,

		configUpdateChan:          make(chan *config.Config, 1),
		overloadProtector:         NewOverloadProtector(cfg.Protection),
		msgRate:                   newMessageRateTracker(60 * time.Second),
		gatewayID:                 gatewayInstanceID(*cfg),
		connectionDurationTracker: obs.NewLatencyTracker(10000),
		buildVersion:              version,
	}

	// 存指针副本，避免调用方后续原地改写共享配置
	stored := *cfg
	e.cfg.Store(&stored)
	e.breakerParams.Store(breakerParamsFrom(&stored))
	e.protection.Store(&stored.Protection)
	e.grpcCfg.Store(&stored.GRPC)
	e.streamCfg.Store(&stored.Stream)
	e.maintenance = newMaintenanceState()
	e.adminSrv = admin.New(e)
	e.ctx = ctx
	e.pipeline = NewMessagePipeline(e)
	e.loginSlots = make(chan struct{}, 256)
	e.udpAssoc = newUDPAssocTable()

	return e
}

// SetShutdownHook 注入完整关闭流程钩子（gateway 壳注入 closeBody）。
// 未注入时 transport 关闭握手触发的 Shutdown 回退业务半区收尾 Close。
func (g *Engine) SetShutdownHook(fn func()) {
	g.shutdownHook = fn
}

// SetLogicLink 注入逻辑链路单例视图（gateway 壳 Start 期注入 *logiclink.LogicClient）。
func (g *Engine) SetLogicLink(l LogicLink) {
	g.logicLink = l
}

// SetLogicPool 注入逻辑客户端池视图（gateway 壳 Start 期注入 *logiclink.LogicClientPool）。
func (g *Engine) SetLogicPool(p LogicLinkPool) {
	g.logicPool = p
}

// Init 读取前置组件按 Order 写入的资源容器，完成安全链装配校验。
func (g *Engine) Init() error {
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
func (g *Engine) validateSecurityWiring(cfg *config.Config) error {
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

// StartBusiness 启动业务半区后台组件：过载采样器、WS 心跳检测、封禁收敛
// 周期安全网、消息完整性校验、pipeline 工作池、分片写合并器、连接空闲检查、
// 配置中心/配置文件监听、UDP 关联清扫与配置热更新循环。
// 不含 gRPC/统计 HTTP/transport 监听——那些由生命周期壳 StartServices 启动。
func (g *Engine) StartBusiness() {
	cfg := g.cfg.Load()
	protection := g.GetProtection()

	// 过载采样器始终启动：保证 /stats 的 cpuPercent/memPercent 有值；
	// 是否拦截由 protection.dropOnOverload 控制（压测关拦截，采样展示保留）。
	g.overloadProtector.Start()
	// WS 心跳超时清理（monitoring.disableWSHeartbeatCheck=true 关闭：压测等
	// 不需要后台扫描的场景，避免与热路径争抢调度）
	if !cfg.Monitoring.DisableWSHeartbeatCheck {
		panicutil.GoLoop(g.ctx, "dataplane.wsHeartbeatChecker", g.wsHeartbeatChecker)
	}
	// 封禁收敛 P2：BatchValidate 周期安全网（loginValidation.enabled=false /
	// batchInterval<=0 时内部自停，无需外层开关）
	g.startBatchValidateSweep()
	g.messageIntegrity = NewMessageIntegrity(30000)

	if cfg.Pipeline.AsyncEnabled {
		g.pipelineWorkerPool = NewPipelineWorkerPool(g, cfg.Pipeline)
	}

	// 初始化分片写合并器（推送路径：logic → client）
	g.shardedCoalescer = NewShardedWriteCoalescer(g.connectionManager, 16)

	// 连接空闲检查（monitoring.disableConnectionChecker=true 关闭：压测等场景，
	// 空闲连接不被自动回收）
	if !cfg.Monitoring.DisableConnectionChecker {
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
	panicutil.GoLoop(g.ctx, "dataplane.configWatcher", g.configWatcher)
	// UDP 关联空闲淘汰/过期 token 清扫（空表扫描开销可忽略，不依赖 udp.enabled 状态）
	panicutil.GoLoop(g.ctx, "dataplane.udpAssocJanitor", g.udpAssocJanitor)
	panicutil.GoLoop(g.ctx, "dataplane.configUpdate", func() {
		for {
			select {
			case <-g.stopChan:
				return
			case newCfg := <-g.configUpdateChan:
				g.handleConfigUpdate(newCfg)
			}
		}
	})
}

// wsHeartbeatChecker 定期清理 WS 心跳超时连接。
// 每轮热读 protection 配置（热更生效）；由 panicutil.GoLoop 启动，
// panic 退避重启，单轮异常不终止清理协程。
func (g *Engine) wsHeartbeatChecker() {
	for {
		interval := time.Duration(g.GetProtection().WSCheckInterval) * time.Second
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
		timeout := time.Duration(g.GetProtection().WSHeartbeatTimeout) * time.Second
		g.runWSHeartbeatCheck(timeout)
	}
}

// runWSHeartbeatCheck 执行一轮检查，独立 recover：单轮异常不终止清理协程。
func (g *Engine) runWSHeartbeatCheck(timeout time.Duration) {
	defer panicutil.Recover(g.ctx, "dataplane.runWSHeartbeatCheck")
	g.checkWebSocketConnections(timeout)
}

func (g *Engine) checkWebSocketConnections(timeout time.Duration) {
	g.wsConnections.Range(func(key, value any) bool {
		conn, ok := key.(*WebSocketConnection)
		if !ok {
			return true
		}
		if time.Since(conn.GetLastPingTime()) > timeout {
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
func (g *Engine) tryEnqueueConfig(newCfg *config.Config) {
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

func (g *Engine) configWatcher() {
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

func (g *Engine) currentSecurityEnableFlags() securityEnableFlags {
	if c := g.cfg.Load(); c != nil {
		return securityEnableFlagsOf(c)
	}
	return securityEnableFlags{}
}

func (g *Engine) handleConfigUpdate(newCfg *config.Config) {
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

func (g *Engine) GetConnectionManager() *ConnectionManager {
	return g.connectionManager
}

func (g *Engine) GetGRPCConfig() config.GRPCConfig {
	if v := g.grpcCfg.Load(); v != nil {
		return *v
	}
	return config.GRPCConfig{}
}

func (g *Engine) GetStreamConfig() config.StreamConfig {
	if v := g.streamCfg.Load(); v != nil {
		return *v
	}
	return config.StreamConfig{}
}

func (g *Engine) GetShardedCoalescer() *ShardedWriteCoalescer {
	return g.shardedCoalescer
}

// GetGatewayID 返回在每个后端流中通告的稳定身份标识。
func (g *Engine) GetGatewayID() string {
	return g.gatewayID
}

// GetServerID 返回用于注册此网关的 etcd 实例ID。
func (g *Engine) GetServerID() string {
	return g.serverID
}

func (g *Engine) logMetrics() {
	tlog.Info(g.ctx, "gateway metrics connectionsActive=%d connectionsTotal=%d messagesReceived=%d messagesForwarded=%d messagesPushed=%d messagesProcessed=%d messagesFailed=%d asyncWriteFailed=%d",
		g.connectionsActive.Load(),
		g.connectionsTotal.Load(),
		g.messagesReceived.Load(),
		g.messagesForwarded.Load(),
		g.messagesPushedToClient.Load(),
		g.messagesProcessed.Load(),
		g.messagesFailed.Load(),
		AsyncWriteFailures(),
	)
}

// metricsLogEnabled 返回是否允许每秒打印 metrics 日志（热更新生效）。
// monitoring.disableMetricsLog=true 时关闭（线上默认建议打开该关闭项）。
func (g *Engine) metricsLogEnabled() bool {
	cfg := g.cfg.Load()
	if cfg == nil {
		return true
	}
	return !cfg.Monitoring.DisableMetricsLog
}

// OnTick 实现 transport.Runtime：每秒采样消息速率并按开关打印指标日志。
// 由 transport.Server.OnTick（gnet Ticker）回调。
func (g *Engine) OnTick() {
	// 每秒滚动采样消息速率（Stats/Health 依赖；原先仅在 Stats 调用时采样，单独查 /health 会失真）
	if g.msgRate != nil {
		g.msgRate.record(time.Now(), g.messagesReceived.Load())
	}
	if g.metricsLogEnabled() {
		g.logMetrics()
	}
}

// WSConnectionCount 返回当前登记的 WebSocket 连接数（测试/观测用）。
func (g *Engine) WSConnectionCount() int {
	n := 0
	g.wsConnections.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// ===== 关闭相位 =====
// 生产关闭由 transport 关闭握手驱动（gateway.closeBody 依序调用各相位）；
// 相位拆分使壳只负责 transport/gRPC 装配，业务半区拆除全部在 Engine 内。

// BeginClose 关闭停止信号通道（幂等：closeOnce 保证 Close 与壳 closeBody
// 来自不同触发者时也只关闭一次）。
func (g *Engine) BeginClose() {
	g.closeOnce.Do(func() {
		close(g.stopChan)
	})
}

// MarkForwardClosed 阶段0：将 Forward 连接置为 Closed，拒绝新消息。
// 必须在停止 transport engine 之前标记：gnet Stop 会拆除连接，之后状态标记已无意义。
func (g *Engine) MarkForwardClosed() {
	g.connectionManager.ForEach(func(conn *Connection) bool {
		if conn.GetState() == StateForward {
			conn.SetState(StateForward, StateClosed)
		}
		return true
	})
}

// DrainConnections 阶段2：等待连接管理器清理完成（连接数降为 0）或超时。
// Forward→Closed 的状态标记由调用方（壳 closeBody）在停止 engine 之前完成。
func (g *Engine) DrainConnections(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// 等待连接数降为0或超时
	for time.Now().Before(deadline) {
		if g.connectionManager.GetConnectionCount() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ShutdownLogic 停止逻辑链路（池 → 单链路 → loginserver 发现句柄）。
func (g *Engine) ShutdownLogic() {
	if g.logicPool != nil {
		g.logicPool.Close()
	}
	if g.logicLink != nil {
		g.logicLink.Close()
	}
	if g.loginService != nil {
		_ = g.loginService.Close()
	}
}

// ShutdownSafetyWorkers 停止安全/后台工作组件（过载保护器、消息完整性校验）。
func (g *Engine) ShutdownSafetyWorkers() {
	if g.overloadProtector != nil {
		g.overloadProtector.Stop()
	}
	if g.messageIntegrity != nil {
		g.messageIntegrity.Stop()
	}
}

// ShutdownDataPath 拆除数据面（连接检查器、pipeline 工作池、写合并器、全部连接）。
// gRPC 停止由壳在本相位之前执行（grpcServer 归壳持有：先断逻辑服入口再拆连接，
// 避免拆除期间仍有下行推入）。
func (g *Engine) ShutdownDataPath() {
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
}

// Close 执行业务半区一次性收尾：BeginClose → 拒绝新消息 → 逻辑链路 →
// 安全组件 → 数据面 → 统计服务。供测试与嵌入场景收尾；幂等（closeOnce）。
// 生产关闭流程见 gateway.closeBody：在本序列基础上先停 transport engine、
// 排空连接，并把 gRPC 停止夹在安全组件与数据面拆除之间。
func (g *Engine) Close() {
	g.BeginClose()
	g.MarkForwardClosed()
	g.ShutdownLogic()
	g.ShutdownSafetyWorkers()
	g.ShutdownDataPath()
	g.StopStatsServer()
}

// acquireLoginSlot 获取并发登录槽位；满返回 false（调用方回 429）。
func (g *Engine) acquireLoginSlot() bool {
	select {
	case g.loginSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseLoginSlot 释放并发登录槽位。
func (g *Engine) releaseLoginSlot() {
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
