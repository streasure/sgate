package gateway

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/backend"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
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

	// 阶段1：过载检查
	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		return PipelineResult{
			Action: gnet.None,
			Error:  fmt.Errorf("server overload"),
		}
	}

	cmd := message.Cmd
	if cmd == 0 {
		return PipelineResult{
			Action: gnet.None,
			Error:  fmt.Errorf("missing cmd"),
		}
	}

	// 阶段2：连接/认证状态检查
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj == nil || connObj.GetServerID() == "" {
		return PipelineResult{Action: gnet.Close}
	}
	if !connObj.IsAuthenticated() && !g.isPreAuthCommand(cmd) {
		g.messagesDroppedAuth.Add(1)
		return PipelineResult{
			Action: gnet.Close,
			Error:  fmt.Errorf("connection not authenticated"),
		}
	}

	// 阶段2.5：连接级流控
	if !connObj.CheckAndIncrementMsgRate(protection.MaxMessagesPerConn) {
		g.messagesDroppedRateLimit.Add(1)
		return PipelineResult{
			Action: gnet.None,
			Error:  fmt.Errorf("connection rate limit exceeded"),
		}
	}

	// 优先使用缓存的LogicClient（无锁），未命中则回退到连接池查找
	logicClient := connObj.GetCachedLogicClient()
	if logicClient == nil || !logicClient.IsConnected() {
		logicClient = g.GetLogicClient(connObj.GetServerID())
		if logicClient == nil {
			g.messagesDroppedNoLogicNotConnected.Add(1)
			return PipelineResult{Action: gnet.None}
		}
		connObj.SetCachedLogicClient(logicClient)
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
		protoMsg.Data = append([]byte(nil), message.Data...)
		protoMsg.SeqId = message.SeqId
		protoMsg.Cmd = cmd
	} else {
		// 完整路径，包含安全链处理
		routeKey = strconv.FormatInt(int64(cmd), 10)
		remoteIP = getRemoteIP(conn)

		// 阶段3：安全链（黑名单 -> 限流 -> WAF -> 熔断器）
		if g.whitelistBlacklist != nil {
			if g.whitelistBlacklist.IsInBlacklist(remoteIP) {
				g.messagesDroppedBlacklist.Add(1)
				return PipelineResult{Action: gnet.None}
			}
			whitelist := g.whitelistBlacklist.GetWhitelist()
			if len(whitelist) > 0 && !g.whitelistBlacklist.IsInWhitelist(remoteIP) {
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
				return PipelineResult{Action: gnet.None}
			}
		}
		if g.circuitBreakerMgr != nil {
			breaker := g.getOrCreateBreaker(routeKey)
			if !breaker.Allow() {
				g.messagesDroppedCircuit.Add(1)
				return PipelineResult{Action: gnet.None}
			}
		}

		// 阶段4：消息完整性检查（可选）
		if protection.VerifyInbound {
			if err := g.messageIntegrity.ProcessMessage(message); err != nil {
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
		protoMsg, filterOK = g.applyForwardFilters(conn, message.Data, connectionID, cmd)
		if !filterOK {
			if span != nil && g.tracer != nil {
				g.tracer.EndSpan(span)
			}
			return PipelineResult{Action: gnet.None}
		}
		if protoMsg == nil {
			protoMsg = &protoGw.StreamData{
				SessionId: connectionID,
				UserKey:   connObj.GetUserUUID(),
				ClientIp:  remoteIP,
				Data:      append([]byte(nil), message.Data...),
				SeqId:     message.SeqId,
			}
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
		}
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
		tlog.Warn(context.TODO(), "client message forward failed sessionID=%s serverID=%s cmd=%d error=%v", connectionID, connObj.GetServerID(), cmd, sendErr)
		g.messagesDroppedFull.Add(1)
		if g.circuitBreakerMgr != nil {
			g.getOrCreateBreaker(routeKey).RecordFailure()
		}
		if g.balancer != nil {
			g.balancer.RecordFailure(routeKey)
		}
		if g.degradation != nil {
			g.degradation.RecordResult(routeKey, true)
		}
	} else {
		g.messagesForwarded.Add(1)
		if g.circuitBreakerMgr != nil {
			g.getOrCreateBreaker(routeKey).RecordSuccess()
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
	remoteIP     string
}

// wsPipelineTaskData WebSocket pipeline 任务数据。
type wsPipelineTaskData struct {
	wsConn       *WebSocketConnection
	payload      []byte
	message      *protoGw.StreamData
	connectionID string
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
func NewPipelineWorkerPool(gw *Gateway, cfg config.PipelineConfig) *PipelineWorkerPool {
	shards := cfg.WorkerShards
	if shards <= 0 {
		shards = runtime.GOMAXPROCS(0) * 4
	}
	queueSize := cfg.WorkerQueueSize
	if queueSize <= 0 {
		queueSize = 65536
	}

	p := &PipelineWorkerPool{
		workers: make([]pipelineWorker, shards),
		shards:  shards,
		gw:      gw,
	}

	for i := 0; i < shards; i++ {
		p.workers[i].taskCh = make(chan pipelineTask, queueSize/shards)
		p.wg.Add(1)
		go p.runWorker(i)
	}

	tlog.Info(context.TODO(), "pipeline worker pool started shards=%d queueSize=%d", shards, queueSize)
	return p
}

// runWorker 是单个 worker goroutine 的主循环。
func (p *PipelineWorkerPool) runWorker(shard int) {
	defer p.wg.Done()
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
	result := p.gw.pipeline.Process(task.conn, task.data, task.message, task.connectionID)
	if result.Error != nil {
		errorResp := routes.NewErrorResponse("error", result.Error.Error(), "", "")
		respData, _ := proto.Marshal(errorResp)
		writeFrame(task.conn, respData)
	}
}

// processWSTask 在 worker goroutine 中执行 WebSocket pipeline 处理。
func (p *PipelineWorkerPool) processWSTask(task *wsPipelineTaskData) {
	result := p.gw.pipeline.ProcessForWS(task.wsConn.Conn, task.payload, task.message, task.connectionID)
	if result.Error != nil {
		errorResp := routes.NewErrorResponse("error", result.Error.Error(), "", "")
		responseData := routes.MarshalClientError(errorResp)
		p.gw.sendWebSocketMessage(task.wsConn, WSOpBinary, responseData)
	}
}

// Submit 将 TCP pipeline 任务提交到对应分片的 worker。
func (p *PipelineWorkerPool) Submit(task pipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{task: &task}:
		return true
	default:
		p.dropped.Add(1)
		tlog.Warn(context.TODO(), "pipeline worker queue full, task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// SubmitWS 将 WebSocket pipeline 任务提交到对应分片的 worker。
func (p *PipelineWorkerPool) SubmitWS(task wsPipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{wsTask: &task}:
		return true
	default:
		p.dropped.Add(1)
		tlog.Warn(context.TODO(), "pipeline worker queue full, WS task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// getShard 根据 connectionID 计算分片索引。
func (p *PipelineWorkerPool) getShard(connectionID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(connectionID))
	return h.Sum32() % uint32(p.shards)
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
