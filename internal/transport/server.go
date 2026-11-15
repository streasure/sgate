package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
)

// Server 是 transport 平面的 gnet 事件处理器与引擎容器：
// 持有各监听端口的 gnet engine 与「端口 → 传输类型」表，实现 gnet.EventHandler；
// 连接注册、准入计数、消息管道等业务动作经消费方定义的 Runtime 接口回掉业务平面。
// 由业务侧构造（gateway 组合时注入 Runtime 实现），关闭流程由业务侧触发。
type Server struct {
	ctx context.Context
	rt  Runtime

	enginesMu sync.Mutex
	engines   []gnet.Engine // 所有 transport engine，关闭时逐个 Stop

	// transportType 端口 → 传输类型（tcp/websocket/udp），OnOpen 判定协议分支用。
	transportType sync.Map

	// 关闭握手：首个触发者执行 rt.Shutdown，其余等待完成。
	// 不能用 sync.Once：gnet 的 Stop 需等事件循环回调 OnShutdown 返回后才置
	// inShutdown；OnShutdown 若重入 Once.Do 将与持有 Once 的 Stop 轮询互锁
	// （优雅退出挂死）。故用 CAS：首触发者独立执行关闭，本回调立即返回。
	closing   atomic.Bool
	closeDone chan struct{} // 关闭流程完成信号（关闭等待者挂在此）
}

// NewServer 创建 transport 服务端：ctx 为业务生命周期 context，rt 为业务平面实现。
func NewServer(ctx context.Context, rt Runtime) *Server {
	return &Server{
		ctx:       ctx,
		rt:        rt,
		closeDone: make(chan struct{}),
	}
}

// setTransportType 设置监听端口对应的传输类型。
func (s *Server) setTransportType(port string, transportType string) {
	s.transportType.Store(port, transportType)
}

// Start 按配置启动全部传输监听器（协议插件化：tcp/websocket/udp）。
// 每个监听器在独立 goroutine 中阻塞于 gnet.Run，直至引擎停止。
func (s *Server) Start(cfg *config.Config) {
	for _, tr := range cfg.Transports {
		port := tr.Port
		transportType := tr.Type
		s.setTransportType(fmt.Sprintf("%d", port), transportType)

		// 协议插件化：从注册表查找协议实现（tcp/websocket/udp）
		// 配置 type 为空时默认 tcp（历史兼容）
		protoName := transportType
		if protoName == "" {
			protoName = "tcp"
		}
		proto := Get(protoName)
		if proto == nil {
			tlog.Error(s.ctx, "unknown protocol type=%s port=%d (no plugin registered)", protoName, port)
			continue
		}

		tlog.Info(s.ctx, "starting gateway transport port=%d type=%s", port, protoName)
		addr := fmt.Sprintf(":%d", port) // 仅用于日志标识
		panicutil.Go(s.ctx, fmt.Sprintf("transport.listen addr=%s", addr), func() {
			if err := proto.Start(s, tr, cfg); err != nil {
				tlog.Error(s.ctx, "gateway transport stopped addr=%s error=%v", addr, err)
			}
		})
	}
}

// EngineCount 返回已注册且尚未摘除的 engine 数量（OnBoot 注册 / OnShutdown 摘除）。
func (s *Server) EngineCount() int {
	s.enginesMu.Lock()
	defer s.enginesMu.Unlock()
	return len(s.engines)
}

// StopEngines 停止全部已注册 engine（业务关闭流程阶段1：停止接受新连接）。
func (s *Server) StopEngines() {
	s.enginesMu.Lock()
	engines := append([]gnet.Engine(nil), s.engines...)
	s.enginesMu.Unlock()
	for i := range engines {
		_ = engines[i].Stop(context.Background())
	}
}

// RequestClose 请求业务关闭流程：首个调用者同步执行 rt.Shutdown，其余等待完成。
// 不能用 sync.Once：gnet 的 Stop 需等事件循环回调 OnShutdown 返回后才置 inShutdown，
// 若 OnShutdown 重入 Once.Do，将与持有 Once 的 Stop 轮询互锁（优雅退出挂死）。
func (s *Server) RequestClose() {
	if !s.closing.CompareAndSwap(false, true) {
		<-s.closeDone
		return
	}
	defer close(s.closeDone)
	s.rt.Shutdown()
}

// CloseDone 返回关闭流程完成信号（首个关闭触发者执行完毕时关闭）。
func (s *Server) CloseDone() <-chan struct{} {
	return s.closeDone
}

// OnBoot 在 gnet 引擎启动时被回调：登记 engine 供关闭流程逐个 Stop。
func (s *Server) OnBoot(engine gnet.Engine) (action gnet.Action) {
	s.enginesMu.Lock()
	s.engines = append(s.engines, engine)
	s.enginesMu.Unlock()
	return
}

// OnShutdown 在 gnet engine 关闭流程中被回调（事件循环 goroutine 上执行）。
// 必须先摘除该 engine（RequestClose 会对所有 engine 调 Stop，与正在回调的 engine 互等死锁），
// 且绝不能在此等待关闭流程完成：RequestClose 可能正持有关闭状态并阻塞在 engines[i].Stop
// 的 inShutdown 轮询上（gnet 需等本回调返回才置位），等待将形成互锁。
func (s *Server) OnShutdown(engine gnet.Engine) {
	s.enginesMu.Lock()
	for i, e := range s.engines {
		if e == engine {
			s.engines = append(s.engines[:i], s.engines[i+1:]...)
			break
		}
	}
	s.enginesMu.Unlock()

	if s.closing.CompareAndSwap(false, true) {
		// 首触发（engine 自行关闭、无人调用过 RequestClose）：在独立协程执行，
		// 本回调立即返回，让 gnet 完成 inShutdown 置位。
		panicutil.Go(s.ctx, "transport.closeAsync", func() {
			defer close(s.closeDone)
			s.rt.Shutdown()
		})
	}
}

// OnTick 每秒回调（gnet WithTicker(true)）：驱动业务侧消息速率采样与指标日志。
func (s *Server) OnTick() (delay time.Duration, action gnet.Action) {
	s.rt.OnTick()
	return 1 * time.Second, gnet.None
}
