package control

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/dataplane"
	"github.com/streasure/sgate/internal/logiclink"
	"github.com/streasure/sgate/internal/transport"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/ugrpc"
)

// Controller 是网关生命周期壳：嵌入 dataplane.Engine（业务半区），自身只负责
// 组合装配——transport 平面服务端（gnet 监听/关闭握手）、gRPC 服务器、
// 逻辑链路具体类型、TLS 配置与完整关闭流程编排（连接排空、gRPC 停止）。
type Controller struct {
	*dataplane.Engine

	ctx       context.Context
	ctxCancel context.CancelFunc // 关闭 g.ctx（closeBody 调用）
	tlsConfig *tls.Config
	// transportSrv transport 平面服务端（gnet EventHandler + engine 容器），
	// 构造时注入本实例为 Runtime；关闭握手与监听启动均委托它。
	transportSrv *transport.Server
	// grpcServer 逻辑服 gRPC 服务端（StartServices 异步启动，closeBody 停止）
	grpcServer   *ugrpc.Server
	grpcServerMu sync.Mutex // 保护 grpcServer 赋值/读取（启动协程与 Close 竞态）

	// 逻辑链路具体类型：壳持有具体类型，Start 期经 SetLogicLink/SetLogicPool
	// 注入 Engine 的消费方接口视图（dataplane 不依赖 logiclink）。
	logicClient     *logiclink.LogicClient
	logicClientPool *logiclink.LogicClientPool
}

// New 创建网关实例：配置、配置文件路径与运行时资源容器均由组合根
// （main）显式传入；各组件按 Order 写入 res，引擎 Init（Order 最后）读取。
func New(cfg *config.Config, configPath string, res *component.Resources) *Controller {
	return newController(cfg, configPath, res)
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

func (g *Controller) Name() string { return "gateway" }
func (g *Controller) Order() int   { return 1000 }

func (g *Controller) Start() error {
	// loginserver 发现由网关自持（消费方持有发现，不依赖共享组件）
	if err := g.InitLoginService(); err != nil {
		return err
	}
	if err := g.StartServices(); err != nil {
		return err
	}
	return nil
}

func (g *Controller) Destroy() { g.Close() }

// StartServices 启动网关服务：业务半区后台组件 → 逻辑链路注入 → gRPC 服务器 →
// 统计 HTTP → transport 监听（顺序保证：业务组件就绪后才接受连接与转发）。
func (g *Controller) StartServices() error {
	cfg := g.Cfg()

	// 业务半区：过载采样、WS 心跳、封禁收敛、消息完整性、pipeline 工作池、
	// 分片写合并器、连接空闲检查、配置中心/文件监听、UDP 关联清扫
	g.StartBusiness()

	// 逻辑链路注入：具体类型在壳持有，Engine 只消费消费方接口视图
	g.logicClient.SetGateway(g)
	g.logicClientPool = logiclink.NewLogicClientPool(g)
	g.SetLogicLink(g.logicClient)
	g.SetLogicPool(g.logicClientPool)

	// gRPC服务器
	grpcCfg := g.GetGRPCConfig()
	grpcPort := fmt.Sprintf(":%d", grpcCfg.Port)
	tlog.Info(g.ctx, "starting gRPC server port=%s", grpcPort)
	panicutil.Go(g.ctx, "control.grpcBootstrap", func() {
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
	g.transportSrv.Start(cfg)
	return nil
}

// Close 执行一次性关闭流程（委托 transport 关闭握手：首个触发者同步执行，
// 其余等待完成）。不能用 sync.Once：gnet 的 Stop 需等事件循环回调 OnShutdown
// 返回后才置 inShutdown，若 OnShutdown 重入 Once.Do，将与持有 Once 的 Stop
// 轮询互锁（优雅退出挂死）。握手实现见 transport.Server.RequestClose。
func (g *Controller) Close() {
	g.transportSrv.RequestClose()
}

// closeBody 关闭流程主体（经 SetShutdownHook 注入 transport 关闭握手；
// 握手保证仅执行一次）。顺序：停业务信号 → 取消 ctx → 拒收新消息 →
// 停 transport → 排空 → 逻辑链路 → 安全组件 → gRPC → 数据面 → 统计。
func (g *Controller) closeBody() {
	g.BeginClose()
	if g.ctxCancel != nil {
		g.ctxCancel()
	}

	// 阶段0：先拒绝新消息——将 Forward 连接置为 Closed。
	// 必须在停止 engine 之前标记：gnet Stop 会拆除连接，之后状态标记已无意义。
	g.MarkForwardClosed()

	// 阶段1：停止接受新连接（所有 transport engine）
	g.transportSrv.StopEngines()

	// 阶段2：排空进行中的消息（最多2分钟）
	drainTimeout := 2 * time.Minute
	drainDone := make(chan struct{})
	panicutil.Go(g.ctx, "control.drainConnections", func() {
		defer close(drainDone)
		g.DrainConnections(drainTimeout)
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

	// 阶段3：逻辑链路（池 → 单链路 → loginserver 发现句柄）
	g.ShutdownLogic()

	// 阶段4：安全/后台工作组件（过载保护器、消息完整性校验）
	g.ShutdownSafetyWorkers()

	// 阶段5：gRPC 服务器（先断逻辑服入口，再拆数据面，避免拆除期间仍有下行推入）
	g.grpcServerMu.Lock()
	grpcSrv := g.grpcServer
	g.grpcServerMu.Unlock()
	if grpcSrv != nil {
		grpcSrv.Stop()
	}

	// 阶段6：数据面（连接检查器/管道工作池/写合并器/全部连接）
	g.ShutdownDataPath()

	g.StopStatsServer()

	tlog.Info(g.ctx, "gateway closed")
}

// ===== construction =====

// newController 用已加载并补齐默认值的配置构造网关壳：
// 先构造数据面引擎（nil fail-fast、运行时默认值、session ID 自描述均在引擎内），
// 再装配 transport/gRPC/TLS 等壳资源。
func newController(cfg *config.Config, configPath string, res *component.Resources) *Controller {
	// 网关生命周期 context：newController 创建，closeBody 取消（Close 路径）
	ctx, cancel := context.WithCancel(context.Background())

	// 业务半区引擎（cfg/res 为 nil 时在此 fail-fast panic）
	e := dataplane.NewEngine(ctx, cfg, configPath, res, BuildVersion)

	gw := &Controller{
		Engine:      e,
		ctx:         ctx,
		ctxCancel:   cancel,
		tlsConfig:   buildTLSConfig(ctx, cfg),
		logicClient: logiclink.NewLogicClient(nil),
	}
	// transport 平面服务端：注入本实例为 Runtime（编译期断言见文末）
	gw.transportSrv = transport.NewServer(ctx, gw)
	// 完整关闭流程钩子：transport 关闭握手首个触发者执行（含连接排空与 gRPC 停止）
	gw.SetShutdownHook(gw.closeBody)

	return gw
}

// 编译期校验：Controller（经 Engine 提升）实现 transport 平面消费方接口 Runtime。
var _ transport.Runtime = (*Controller)(nil)

// 编译期校验：Controller（经 Engine 提升）实现 logiclink.GatewayInterface。
var _ logiclink.GatewayInterface = (*Controller)(nil)
