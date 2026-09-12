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

// PipelineResult carries the outcome of processing a client message through the
// shared pipeline. The caller (TCP or WebSocket handler) uses this to decide
// how to send the error response and which gnet.Action to return.
type PipelineResult struct {
	Action   gnet.Action
	Error    error
	ProtoMsg *protoGw.StreamData
}

// MessagePipeline implements the shared message processing logic for both TCP
// and WebSocket paths. It eliminates code duplication for overload check,
// authentication, security chain, filter chain, and forward-to-logic steps.
type MessagePipeline struct {
	gw *Gateway
}

// NewMessagePipeline creates a pipeline bound to the given gateway.
func NewMessagePipeline(gw *Gateway) *MessagePipeline {
	return &MessagePipeline{gw: gw}
}

// Process runs the common message processing pipeline:
//  1. Overload check
//  2. Connection/auth state
//  3. Security chain (blacklist, rate limit, WAF, circuit breaker)
//  4. Message integrity (optional)
//  5. Filter chain
//  6. Forward to LogicClient
//  7. Metrics recording
//
// Parameters:
//   - conn: the raw gnet.Conn (TCP or WS underlying)
//   - data: the raw frame payload (for WAF inspection)
//   - message: decoded StreamData
//   - connectionID: the connection identifier
//   - isPreAuth: whether this command is allowed before full authentication
//
// Returns a PipelineResult. On success, Result.ProtoMsg contains the message
// to forward. On failure, Result.Error is non-nil and the caller should send
// the error response using its protocol-specific method.
func (p *MessagePipeline) Process(conn gnet.Conn, data []byte, message *protoGw.StreamData, connectionID string) PipelineResult {
	g := p.gw

	// Stage 1: Overload check
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

	// Stage 2: Connection/auth state
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

	// Try cached LogicClient first (lock-free), fallback to pool lookup
	logicClient := connObj.GetCachedLogicClient()
	if logicClient == nil || !logicClient.IsConnected() {
		logicClient = g.GetLogicClient(connObj.GetServerID())
		if logicClient == nil {
			g.messagesDroppedNoLogicNotConnected.Add(1)
			return PipelineResult{Action: gnet.None}
		}
		connObj.SetCachedLogicClient(logicClient)
	}

	// Fast path: skip security/filter chain for authenticated connections
	// when no security components are enabled or all are nil.
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
		// Ultra-fast path: no security, no tracing, no balancing
		remoteIP = ""
		protoMsg = getStreamData()
		protoMsg.SessionId = connectionID
		protoMsg.UserKey = connObj.GetUserUUID()
		protoMsg.Data = message.Data
		protoMsg.SeqId = message.SeqId
		protoMsg.Cmd = cmd
	} else {
		// Full path with security chain
		routeKey = strconv.FormatInt(int64(cmd), 10)
		remoteIP = getRemoteIP(conn)

		// Stage 3: Security chain (blacklist -> rate limit -> WAF -> circuit breaker)
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

		// Stage 4: Message integrity check (optional)
		if g.protection.VerifyInbound {
			if err := g.messageIntegrity.ProcessMessage(message); err != nil {
				return PipelineResult{
					Action: gnet.None,
					Error:  fmt.Errorf("message integrity check failed: %w", err),
				}
			}
		}

		// Trace span for latency tracking
		if g.tracer != nil {
			traceID := obs.GenerateTraceID()
			span = g.tracer.StartSpan(traceID, "forward", "")
			g.tracer.AddAttribute(span, "cmd", routeKey)
			g.tracer.AddAttribute(span, "connectionID", connectionID)
		}

		// Stage 5: Filter chain (JWT, canary, mirror, OTel, degradation)
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

	// Stage 6: Forward to LogicClient (use cached client from Stage 2)
	var sendErr error
	if logicClient == nil {
		sendErr = ErrNotConnected
	} else {
		sendErr = logicClient.SendMessage(protoMsg)
	}

	// Stage 7: Metrics recording
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

// ProcessForWS is a thin wrapper for WebSocket callers that also handles
// user key mapping from the message. It delegates to Process for the core
// pipeline logic.
func (p *MessagePipeline) ProcessForWS(conn gnet.Conn, data []byte, message *protoGw.StreamData, connectionID string) PipelineResult {
	g := p.gw

	// WebSocket-specific: update user UUID mapping if present
	if message.UserKey != "" {
		oldUserUUID := "temp_" + connectionID
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, message.UserKey)
		tlog.Debug("received user UUID", "connectionID", connectionID, "userUUID", message.UserKey)
	}

	result := p.Process(conn, data, message, connectionID)

	// For WS, inject UserKey into the proto message if the filter chain didn't set it
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
