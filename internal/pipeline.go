package gateway

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/util/tlog"
)

var streamDataPool = sync.Pool{
	New: func() interface{} {
		return &protoGw.StreamData{}
	},
}

func getStreamData() *protoGw.StreamData {
	return streamDataPool.Get().(*protoGw.StreamData)
}

func putStreamData(msg *protoGw.StreamData) {
	msg.Reset()
	streamDataPool.Put(msg)
}

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
	if !connObj.CheckAndIncrementMsgRate(g.protection.MaxMessagesPerConn) {
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
		!g.protection.VerifyInbound

	var remoteIP string
	var routeKey string
	var span *obs.TraceSpan
	var protoMsg *protoGw.StreamData
	filterOK := true

		if securityDisabled && g.tracer == nil && g.balancer == nil && g.degradation == nil {
		// 超级快速路径：无安全检查、无追踪、无负载均衡
		remoteIP = ""
		protoMsg = getStreamData()
		protoMsg.SessionId = connectionID
		protoMsg.UserKey = connObj.GetUserUUID()
		protoMsg.Data = message.Data
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
		if g.protection.VerifyInbound {
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
		sendErr = ErrNotConnected
	} else {
		sendErr = logicClient.SendMessage(protoMsg)
	}

	// 阶段7：指标记录
	if sendErr != nil {
		tlog.Warn("client message forward failed", "sessionID", connectionID, "serverID", connObj.GetServerID(), "cmd", cmd, "error", sendErr)
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
		tlog.Debug("received user UUID", "connectionID", connectionID, "userUUID", message.UserKey)
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
