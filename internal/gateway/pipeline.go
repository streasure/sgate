package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/backend"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/hashutil"
	"github.com/streasure/util/tlog"
)

// PipelineResult 携带通过共享消息管道处理客户端消息的结果。
// 调用方（TCP或WebSocket处理器）使用此结构决定如何发送错误响应以及返回哪个gnet.Action。
type PipelineResult struct {
	Action   gnet.Action
	Error    error
	ProtoMsg *protoGw.StreamData
}

// MessagePipeline 实现TCP和WebSocket共享的消息处理逻辑。
// 消除了过载检查、认证、安全链、过滤器链和转发到逻辑层等步骤的代码重复。
type MessagePipeline struct {
	gw *Gateway
}

// NewMessagePipeline 创建绑定到指定网关的消息管道。
func NewMessagePipeline(gw *Gateway) *MessagePipeline {
	return &MessagePipeline{gw: gw}
}

// 管道拒绝路径的哨兵错误：热路径上避免 fmt.Errorf 的每消息分配。
var (
	errPipelineMissingCmd = errors.New("missing cmd")
	errPipelineNoConn     = errors.New("unknown connection")
	errPipelineUnbound    = errors.New("connection not bound (login pending)")
	errPipelineUnauthed   = errors.New("connection not authenticated")
	errPipelineRateLimit  = errors.New("connection rate limit exceeded")
)

// loginBindGrace 新连接的登录绑定宽限期：绑定在后台协程完成
// （loginKey 校验为同步 HTTP），窗口内到达的非 preAuth 消息只丢弃不断连。
// 为包级变量便于测试覆盖。
var loginBindGrace = 5 * time.Second

// routeKeyCache 命令号到路由键字符串的驻留缓存（熔断/限流/降级按键读取）。
var routeKeyCache sync.Map

// routeKeyFor 返回 cmd 的驻留路由键：首次 strconv 分配一次，之后零分配。
func routeKeyFor(cmd int32) string {
	if v, ok := routeKeyCache.Load(cmd); ok {
		return v.(string)
	}
	key := strconv.Itoa(int(cmd))
	actual, _ := routeKeyCache.LoadOrStore(cmd, key)
	return actual.(string)
}

// Process 运行通用消息处理管道：
//  1. 过载检查
//  2. 连接/认证状态检查
//  3. 安全链（黑名单、限流、WAF、熔断器）
//  4. 消息完整性检查（可选）
//  5. 过滤器链
//  6. 转发到逻辑客户端
//  7. 指标记录
//
// 参数：
//   - conn：原始gnet.Conn连接（TCP或WebSocket底层连接）
//   - data：原始帧负载（用于WAF检测）
//   - message：解码后的 StreamData 消息。
//   - connectionID：连接标识符
//
// 返回PipelineResult。成功时，Result.ProtoMsg包含要转发的消息；
// 失败时，Result.Error非空，调用方应使用协议特定的方法发送错误响应。
func (p *MessagePipeline) Process(conn gnet.Conn, data []byte, message *protoGw.StreamData, connectionID string) PipelineResult {
	g := p.gw
	protection := g.getProtection()

	// 阶段1：过载检查。过载丢弃静默（与黑名单/安全限流/登录宽限一致）：
	// 此时逐条回错误帧会占用正被挤爆的事件循环与写带宽，等于放大负载；
	// 可见性由 messagesDroppedOverload / overloadProtector 计数提供。
	// 登录路径例外：LoginGate 必须回 503 ack（见 login.go）。
	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		return PipelineResult{Action: gnet.None}
	}

	cmd := message.Cmd
	if cmd == 0 {
		return PipelineResult{
			Action: gnet.None,
			Error:  errPipelineMissingCmd,
		}
	}

	// 阶段2：连接/认证状态检查
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj == nil {
		g.messagesDroppedAuth.Add(1)
		return PipelineResult{Action: gnet.Close, Error: errPipelineNoConn}
	}
	// preAuth 命令（如 LoginGate）在 serverID 绑定完成前也放行，否则 preAuthCommands 配置永远无效。
	// 未绑定时 connObj 仍可用于 rate 控制与转发（LoginGate 处理器内部完成绑定）。
	unbound := connObj.GetServerID() == ""
	if unbound && !g.isPreAuthCommand(cmd) {
		g.messagesDroppedAuth.Add(1)
		// 登录宽限期内（绑定在后台协程完成，含 loginKey HTTP 校验）：
		// 抢跑的非 preAuth 消息只丢弃不断连，避免「不等 ack 先发心跳」的客户端被误杀。
		if time.Since(time.UnixMilli(connObj.CreatedAt)) < loginBindGrace {
			return PipelineResult{Action: gnet.None}
		}
		return PipelineResult{
			Action: gnet.Close,
			Error:  errPipelineUnbound,
		}
	}
	if !unbound && !connObj.IsAuthenticated() && !g.isPreAuthCommand(cmd) {
		g.messagesDroppedAuth.Add(1)
		// 同上：serverID 已绑定但 userUUID 尚未更新的短暂窗口按宽限期处理。
		if time.Since(time.UnixMilli(connObj.CreatedAt)) < loginBindGrace {
			return PipelineResult{Action: gnet.None}
		}
		return PipelineResult{
			Action: gnet.Close,
			Error:  errPipelineUnauthed,
		}
	}

	// 阶段2.5：连接级流控
	if !connObj.CheckAndIncrementMsgRate(protection.MaxMessagesPerConn) {
		g.messagesDroppedRateLimit.Add(1)
		return PipelineResult{
			Action: gnet.None,
			Error:  errPipelineRateLimit,
		}
	}

	// 优先使用缓存的LogicClient（无锁），未命中则回退到连接池查找。
	// 未绑定（preAuth 阶段，如 LoginGate）时 logicClient 为 nil，
	// 由阶段6返回 ErrNotConnected 前需允许处理器本地处理——
	// 实际上 LoginGate 由 handler 直接处理、不进入转发，此分支仅对绑定后消息生效。
	var logicClient connection.LogicClientProvider
	boundServerID := connObj.GetServerID()
	if boundServerID != "" {
		logicClient = connObj.GetCachedLogicClient()
		if logicClient == nil || !logicClient.IsConnected() {
			logicClient = g.GetLogicClient(boundServerID)
			if logicClient == nil {
				g.messagesDroppedNoLogicNotConnected.Add(1)
				return PipelineResult{Action: gnet.None}
			}
			connObj.SetCachedLogicClient(logicClient)
		}
	}

	// 快速路径：当没有启用安全组件或全部为nil时，
	// 跳过已认证连接的安全/过滤器链检查
	securityDisabled := g.whitelistBlacklist == nil &&
		g.rateLimiter == nil &&
		g.waf == nil &&
		g.circuitBreakerMgr == nil &&
		!protection.VerifyInbound

	var remoteIP string
	var routeKey string
	var span *obs.TraceSpan
	var protoMsg *protoGw.StreamData
	filterOK := true

	if securityDisabled && g.tracer == nil && g.balancer == nil && g.degradation == nil {
		// 超级快速路径：无安全检查、无追踪、无负载均衡
		remoteIP = ""
		protoMsg = backend.GetStreamData()
		protoMsg.SessionId = connectionID
		protoMsg.UserKey = connObj.GetUserUUID()
		// 复用池化消息的 Data 缓冲容量（PutStreamData 保留容量），热路径零分配。
		protoMsg.Data = append(protoMsg.Data[:0], message.Data...)
		protoMsg.SeqId = message.SeqId
		protoMsg.Cmd = cmd
	} else {
		// 完整路径，包含安全链处理
		routeKey = routeKeyFor(cmd)
		remoteIP = connObj.RemoteHost()

		// 阶段3：安全链（黑名单 -> 限流 -> WAF -> 熔断器）
		if g.whitelistBlacklist != nil {
			if g.whitelistBlacklist.IsInBlacklist(remoteIP) {
				g.messagesDroppedBlacklist.Add(1)
				return PipelineResult{Action: gnet.None}
			}
			if !g.whitelistBlacklist.WhitelistEmpty() && !g.whitelistBlacklist.IsInWhitelist(remoteIP) {
				g.messagesDroppedBlacklist.Add(1)
				return PipelineResult{Action: gnet.None}
			}
		}
		if g.rateLimiter != nil {
			if !g.rateLimiter.Allow("ip", remoteIP) {
				g.messagesDroppedRateLimit.Add(1)
				return PipelineResult{Action: gnet.None}
			}
			if !g.rateLimiter.Allow("route", routeKey) {
				g.messagesDroppedRateLimit.Add(1)
				return PipelineResult{Action: gnet.None}
			}
		}
		if g.waf != nil {
			if !g.waf.Inspect(data) {
				g.messagesDroppedWAF.Add(1)
				// blockAction=log 仅记录（已由 WAF 内部告警）并放行；drop 才断连
				if g.waf.ShouldBlock() {
					return PipelineResult{Action: gnet.Close}
				}
				return PipelineResult{Action: gnet.None}
			}
		}
		if g.circuitBreakerMgr != nil {
			if breaker := g.getOrCreateBreaker(routeKey); breaker != nil && !breaker.Allow() {
				g.messagesDroppedCircuit.Add(1)
				return PipelineResult{Action: gnet.None}
			}
		}

		// 阶段4：消息完整性检查（可选）
		if protection.VerifyInbound {
			if err := g.messageIntegrity.ProcessMessage(connectionID, message); err != nil {
				g.messagesDroppedIntegrity.Add(1)
				return PipelineResult{
					Action: gnet.None,
					Error:  fmt.Errorf("message integrity check failed: %w", err),
				}
			}
		}

		// 创建追踪Span用于延迟跟踪
		if g.tracer != nil {
			traceID := obs.GenerateTraceID()
			span = g.tracer.StartSpan(traceID, "forward", "")
			g.tracer.AddAttribute(span, "cmd", routeKey)
			g.tracer.AddAttribute(span, "connectionID", connectionID)
		}

		// 阶段5：过滤器链（JWT、金丝雀、镜像、OpenTelemetry、降级）
		protoMsg, filterOK = g.applyForwardFilters(conn, message.Data, connectionID, cmd, message.SeqId)
		if !filterOK {
			if span != nil && g.tracer != nil {
				g.tracer.EndSpan(span)
			}
			return PipelineResult{Action: gnet.None}
		}
		if protoMsg == nil {
			protoMsg = backend.GetStreamData()
			protoMsg.SessionId = connectionID
			protoMsg.UserKey = connObj.GetUserUUID()
			protoMsg.ClientIp = remoteIP
			protoMsg.Data = append(protoMsg.Data[:0], message.Data...)
			protoMsg.SeqId = message.SeqId
			if cmd > 0 {
				protoMsg.Cmd = cmd
			}
		} else {
			if protoMsg.UserKey == "" {
				protoMsg.UserKey = connObj.GetUserUUID()
			}
			if protoMsg.ClientIp == "" {
				protoMsg.ClientIp = remoteIP
			}
			if cmd > 0 && protoMsg.Cmd == 0 {
				protoMsg.Cmd = cmd
			}
			if protoMsg.SeqId == 0 && message.SeqId != 0 {
				protoMsg.SeqId = message.SeqId
			}
		}
	}

	// 阶段5.5：切断 Data 别名。过滤器链可能让 protoMsg.Data 直接引用
	// message.Data（入站解码池化缓冲）：入站消息在处理结束后归还解码池，
	// 转发消息要到 gRPC 序列化后才归还，二者生命周期不同，必须复制。
	if len(protoMsg.Data) > 0 && len(message.Data) > 0 &&
		&protoMsg.Data[0] == &message.Data[0] {
		aliased := make([]byte, len(protoMsg.Data))
		copy(aliased, protoMsg.Data)
		protoMsg.Data = aliased
	}

	// 阶段6：转发到LogicClient（使用阶段2缓存的客户端）
	var sendErr error
	if logicClient == nil {
		sendErr = backend.ErrNotConnected
	} else {
		sendErr = logicClient.SendMessage(protoMsg)
	}

	// 阶段7：指标记录
	if sendErr != nil {
		g.messagesFailed.Add(1)
		tlog.Warn(context.TODO(), "client message forward failed sessionID=%s serverID=%s cmd=%d error=%v", connectionID, connObj.GetServerID(), cmd, sendErr)
		g.messagesDroppedFull.Add(1)
		if g.circuitBreakerMgr != nil {
			if breaker := g.getOrCreateBreaker(routeKey); breaker != nil {
				breaker.RecordFailure()
			}
		}
		if g.balancer != nil {
			g.balancer.RecordFailure(routeKey)
		}
		if g.degradation != nil {
			g.degradation.RecordResult(routeKey, true)
		}
	} else {
		g.messagesForwarded.Add(1)
		g.messagesProcessed.Add(1)
		if g.circuitBreakerMgr != nil {
			if breaker := g.getOrCreateBreaker(routeKey); breaker != nil {
				breaker.RecordSuccess()
			}
		}
		if g.balancer != nil {
			g.balancer.RecordSuccess(routeKey)
		}
		if g.degradation != nil {
			g.degradation.RecordResult(routeKey, false)
		}
	}

	if span != nil && g.tracer != nil {
		g.tracer.EndSpan(span)
		if g.latencyTracker != nil {
			g.latencyTracker.Record(span.Duration)
		}
	}

	if sendErr != nil {
		return PipelineResult{
			Action:   gnet.None,
			Error:    fmt.Errorf("forward to logic failed: %w", sendErr),
			ProtoMsg: protoMsg,
		}
	}

	return PipelineResult{Action: gnet.None, ProtoMsg: protoMsg}
}

// ProcessForWS 是WebSocket调用者的轻量级封装，
// 同时处理消息中的用户标识映射。核心管道逻辑委托给Process处理。
func (p *MessagePipeline) ProcessForWS(conn gnet.Conn, data []byte, message *protoGw.StreamData, connectionID string) PipelineResult {
	g := p.gw

	// WebSocket特定：如果存在用户UUID则更新映射
	if message.UserKey != "" {
		oldUserUUID := "temp_" + connectionID
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, message.UserKey)
		tlog.Debug(context.TODO(), "received user UUID connectionID=%s userUUID=%s", connectionID, message.UserKey)
	}

	result := p.Process(conn, data, message, connectionID)

	// 对于WebSocket，如果过滤器链未设置UserKey，则注入到协议消息中
	if result.ProtoMsg != nil {
		if result.ProtoMsg.UserKey == "" && message.UserKey != "" {
			result.ProtoMsg.UserKey = message.UserKey
		}
		if result.ProtoMsg.SeqId == 0 && message.SeqId != 0 {
			result.ProtoMsg.SeqId = message.SeqId
		}
	}

	return result
}

// ===== 异步 worker pool（合并自 pipeline_worker.go）=====

// pipelineTask 是提交给 worker pool 的异步任务（TCP 或 WS）。
type pipelineTask struct {
	task   *pipelineTaskData
	wsTask *wsPipelineTaskData
}

// pipelineTaskData TCP pipeline 任务数据。
type pipelineTaskData struct {
	conn         gnet.Conn
	data         []byte
	message      *protoGw.StreamData
	connectionID string
}

// wsPipelineTaskData WebSocket pipeline 任务数据。
type wsPipelineTaskData struct {
	wsConn       *WebSocketConnection
	payload      []byte
	message      *protoGw.StreamData
	connectionID string
}

// 池化：pipeline 任务结构与入站帧缓冲（热路径每消息一任务/一帧拷贝）。
var (
	pipelineTaskPool   = sync.Pool{New: func() any { return &pipelineTaskData{} }}
	wsPipelineTaskPool = sync.Pool{New: func() any { return &wsPipelineTaskData{} }}
	msgBufPool         = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
)

// getMsgBuf 返回长度为 n 的入站帧缓冲；池中对象容量不足时退化为新分配。
func getMsgBuf(n int) []byte {
	if v := msgBufPool.Get(); v != nil {
		b := *(v.(*[]byte))
		if cap(b) >= n {
			return b[:n]
		}
	}
	return make([]byte, n)
}

// putMsgBuf 归还入站帧缓冲；超大缓冲（>1MB）不入池，交还 GC 一次性回收。
func putMsgBuf(b []byte) {
	if cap(b) == 0 || cap(b) > 1<<20 {
		return
	}
	v := b[:0]
	msgBufPool.Put(&v)
}

// pipelineWorker 是单个 worker goroutine，处理一个分片内的所有连接。
type pipelineWorker struct {
	taskCh chan pipelineTask
}

// PipelineWorkerPool 是分片的异步 pipeline 工作池。
// 按 connectionID 哈希分片，保证同一连接的消息严格有序。
type PipelineWorkerPool struct {
	workers   []pipelineWorker
	shards    int
	gw        *Gateway
	submitted atomic.Int64
	dropped   atomic.Int64
	wg        sync.WaitGroup
}

// NewPipelineWorkerPool 创建并启动异步 pipeline 工作池。
// shardCount 应为 2 的幂以优化哈希取模。
// cfg.WorkerQueueSize 为全池总任务数，启动时按分片数均分（每分片至少 1，
// 防止 queueSize < shards 时退化为无缓冲通道导致 Submit 全量丢弃）。
func NewPipelineWorkerPool(gw *Gateway, cfg config.PipelineConfig) *PipelineWorkerPool {
	shards := cfg.WorkerShards
	if shards <= 0 {
		shards = runtime.GOMAXPROCS(0) * 4
	}
	queueSize := cfg.WorkerQueueSize
	if queueSize <= 0 {
		queueSize = config.DefaultPipelineWorkerQueueSize
	}

	p := &PipelineWorkerPool{
		workers: make([]pipelineWorker, shards),
		shards:  shards,
		gw:      gw,
	}

	perShard := max(queueSize/shards, 1)
	for i := range shards {
		p.workers[i].taskCh = make(chan pipelineTask, perShard)
		p.wg.Go(func() { p.runWorker(i) })
	}

	tlog.Info(context.TODO(), "pipeline worker pool started shards=%d queueSize=%d perShard=%d", shards, queueSize, perShard)
	return p
}

// runWorker 是单个 worker goroutine 的主循环。
func (p *PipelineWorkerPool) runWorker(shard int) {
	for t := range p.workers[shard].taskCh {
		if t.wsTask != nil {
			p.processWSTask(t.wsTask)
		} else {
			p.processTask(t.task)
		}
	}
}

// processTask 在 worker goroutine 中执行 TCP pipeline 处理。
func (p *PipelineWorkerPool) processTask(task *pipelineTaskData) {
	defer func() {
		routes.PutClientMessage(task.message)
		putMsgBuf(task.data)
		pipelineTaskPool.Put(task)
	}()
	result := p.gw.pipeline.Process(task.conn, task.data, task.message, task.connectionID)
	if result.Error != nil {
		if result.ProtoMsg != nil {
			backend.PutStreamData(result.ProtoMsg)
		}
		writeFrameAsync(task.conn, routes.NewErrorFrame("error", result.Error.Error(), "", ""))
	}
	applyAsyncResult(task.conn, nil, result)
}

// processWSTask 在 worker goroutine 中执行 WebSocket pipeline 处理。
func (p *PipelineWorkerPool) processWSTask(task *wsPipelineTaskData) {
	defer func() {
		routes.PutClientMessage(task.message)
		putMsgBuf(task.payload)
		wsPipelineTaskPool.Put(task)
	}()
	result := p.gw.pipeline.ProcessForWS(task.wsConn.Conn, task.payload, task.message, task.connectionID)
	if result.Error != nil {
		if result.ProtoMsg != nil {
			backend.PutStreamData(result.ProtoMsg)
		}
		p.gw.sendWebSocketMessageAsync(task.wsConn, WSOpBinary, routes.NewErrorFrame("error", result.Error.Error(), "", ""))
	}
	applyAsyncResult(task.wsConn.Conn, task.wsConn, result)
}

// applyAsyncResult 在 worker 中执行 pipeline 返回的 Action（如 Close）。
// gnet.Conn.Close 是并发安全的；不得在 worker 中使用非 Async 的 Write。
func applyAsyncResult(conn gnet.Conn, wsConn *WebSocketConnection, result PipelineResult) {
	if result.Action != gnet.Close {
		return
	}
	if wsConn != nil {
		wsConn.State.Store(int32(WSStateClosing))
	}
	_ = conn.Close()
}

// writeFrameAsync 从 worker goroutine 安全写 TCP 帧（gnet 要求跨协程用 AsyncWrite）。
// 必须与 writeFrame 一致：先写 4 字节大端长度前缀，否则客户端按前缀读长度会得到垃圾值。
func writeFrameAsync(conn gnet.Conn, data []byte) {
	if conn == nil {
		return
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	buf := make([]byte, 4+len(data))
	copy(buf, header[:])
	copy(buf[4:], data)
	_ = conn.AsyncWrite(buf, noopAsyncCallback)
}

// noopAsyncCallback AsyncWrite 完成回调，忽略错误。
func noopAsyncCallback(_ gnet.Conn, _ error) error { return nil }

// Submit 将 TCP pipeline 任务提交到对应分片的 worker。
// 提交失败（队列满）时返回 false，调用方负责归还任务与其缓冲。
func (p *PipelineWorkerPool) Submit(task *pipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{task: task}:
		return true
	default:
		p.dropped.Add(1)
		if p.gw != nil {
			p.gw.messagesDroppedFull.Add(1)
		}
		tlog.Warn(context.TODO(), "pipeline worker queue full, task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// SubmitWS 将 WebSocket pipeline 任务提交到对应分片的 worker。
// 提交失败（队列满）时返回 false，调用方负责归还任务与其缓冲。
func (p *PipelineWorkerPool) SubmitWS(task *wsPipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{wsTask: task}:
		return true
	default:
		p.dropped.Add(1)
		if p.gw != nil {
			p.gw.messagesDroppedFull.Add(1)
		}
		tlog.Warn(context.TODO(), "pipeline worker queue full, WS task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// getShard 根据 connectionID 计算分片索引。
func (p *PipelineWorkerPool) getShard(connectionID string) uint32 {
	return hashutil.FNV1a32(connectionID) % uint32(p.shards)
}

// Stats 返回工作池统计信息。
func (p *PipelineWorkerPool) Stats() (submitted, dropped int64, queueDepth int64) {
	submitted = p.submitted.Load()
	dropped = p.dropped.Load()
	var depth int64
	for i := range p.workers {
		depth += int64(len(p.workers[i].taskCh))
	}
	return submitted, dropped, depth
}

// Stop 停止工作池，等待所有 worker 退出。
func (p *PipelineWorkerPool) Stop() {
	for i := range p.workers {
		close(p.workers[i].taskCh)
	}
	p.wg.Wait()
}
