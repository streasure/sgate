package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/streasure/sgate/internal/backend"

	"github.com/streasure/sgate/internal/connection"

	"github.com/panjf2000/gnet/v2"
	"github.com/spf13/cast"
	protoGw "github.com/streasure/protocol/gateway"
	routes "github.com/streasure/sgate/internal/routes"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/util/tlog"
)

var connContextPool = sync.Pool{
	New: func() any {
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
			tlog.Error(context.TODO(), "OnOpen panic recovered error=%v", r)
			action = gnet.Close
		}
	}()

	// 连接数限制检查（P0: 防止 OOM 和连接耗尽）
	remoteIP := getRemoteIP(c)
	if !g.connectionManager.CanAccept(remoteIP) {
		tlog.Warn(context.TODO(), "连接数限制，拒绝新连接 remoteIP=%s activeConnections=%d maxConnections=%d ipConnections=%d maxPerIP=%d",
			remoteIP,
			g.connectionManager.GetConnectionCount(),
			g.connectionManager.MaxConnections(),
			g.connectionManager.GetIPConnectionCount(remoteIP),
			g.connectionManager.MaxConnectionsPerIP())
		return nil, gnet.Close
	}

	localAddr := c.LocalAddr().String()
	isWS := false
	g.transportType.Range(func(key, value any) bool {
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
		tempUserUUID := "temp_" + connection.GenerateConnectionID()
		connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
		connCtx := GetConnContext()
		connCtx.ConnectionID = connectionID
		c.SetContext(connCtx)
	}

	g.connectionsTotal.Add(1)
	g.connectionsActive.Add(1)

	tlog.Debug(context.TODO(), "new connection localAddr=%s isWS=%v", localAddr, isWS)
	return
}

func (g *Gateway) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "OnClose panic recovered error=%v", r)
		}
	}()

	var connectionID string
	connCtx := c.Context()

	if connCtx != nil {
		if ctx, ok := connCtx.(*ConnContext); ok {
			connectionID = ctx.ConnectionID
			PutConnContext(ctx)
		} else if wsConn, ok := connCtx.(*WebSocketConnection); ok {
			connectionID = wsConn.ConnectionID()
			g.wsConnections.Delete(wsConn)
			wsConn.State.Store(int32(WSStateClosed))
		} else if id, ok := connCtx.(string); ok {
			connectionID = id
		}
	}

	if connectionID != "" {
		if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
			// P1: 记录连接生命周期指标
			duration := time.Now().UnixMilli() - conn.CreatedAt
			g.connectionDurationSum.Add(duration)
			g.connectionDurationCount.Add(1)
			g.connectionDurationTracker.Record(time.Duration(duration) * time.Millisecond)
			g.notifyLogicOffline(conn)
		}
		g.connectionManager.RemoveConnection(connectionID)
		g.connectionsActive.Add(-1)
		tlog.Debug(context.TODO(), "connection closed connectionID=%s error=%v", connectionID, err)
	}

	return
}

func (g *Gateway) OnTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "OnTraffic panic recovered error=%v", fmt.Sprintf("%v", r))
			action = gnet.Close
		}
	}()

	return g.handleNormalTraffic(c)
}

func (g *Gateway) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "handleNormalTraffic panic recovered error=%s", cast.ToString(r))
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

	maxFrameBuf := g.getProtection().MaxFrameBufSize
	if len(ctx.FrameBuf)+len(data) > maxFrameBuf {
		ctx.FrameBuf = nil
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	// 使用正常协议路径处理每个完整帧。逻辑流携带每条消息的真实客户端命令；它不定义网关私有的批处理命令。
	maxFrame := g.getProtection().MaxFrameSize
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
	// 逻辑登录命令是稳定的协议边界。将其作为内置回退保留，
	// 以便旧的动态配置在命令范围迁移后不会锁定所有新连接的会话。
	if cmd == routes.CmdLogicLoginReq {
		return true
	}
	return slices.Contains(g.getProtection().PreAuthCommands, cmd)
}

func (g *Gateway) GetLogicClient(serverID string) connection.LogicClientProvider {
	if g.logicClientPool == nil {
		return nil
	}
	return g.logicClientPool.GetClient(serverID)
}

// LookupLogicAddress 通过 serverID 在服务发现中查询逻辑服地址。
func (g *Gateway) LookupLogicAddress(serverID string) string {
	if g.logicClientPool == nil {
		return ""
	}
	return g.logicClientPool.LookupAddress(serverID)
}

func (g *Gateway) GetGatewayClient(serverID string) backend.GatewayClientProvider {
	if g.gatewayClientPool == nil {
		return nil
	}
	return g.gatewayClientPool.GetClient(serverID)
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
		tempUserUUID := "temp_" + connection.GenerateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		c.SetContext(&ConnContext{
			ConnectionID: connectionID,
			FrameBuf:     nil,
		})
	}

	message, ok := routes.DecodeClientMessage(data)
	if !ok {
		return gnet.Close
	}
	// 入站流量刷新活跃时间（否则仅靠 Send 采样会被 connIdleTimeout 误杀）
	if connObj := g.connectionManager.GetConnection(connectionID); connObj != nil {
		connObj.TouchActive()
	}
	cmd := message.Cmd
	if cmd == routes.CmdLoginGate {
		return g.handleLoginGate(c, connectionID, message)
	}
	if cmd == routes.CmdLogoutGate {
		return g.handleLogoutGate(c, connectionID, message)
	}

	// 异步路径：投递到 worker pool，event loop 立即返回
	if g.pipelineWorkerPool != nil {
		remoteIP := getRemoteIP(c)
		// 深拷贝 data，因为 FrameBuf 会被 event loop 复用
		dataCopy := append([]byte(nil), data...)
		g.pipelineWorkerPool.Submit(pipelineTaskData{
			conn:         c,
			data:         dataCopy,
			message:      message,
			connectionID: connectionID,
			remoteIP:     remoteIP,
		})
		return
	}

	// 同步路径：直接在 event loop 中处理
	result := g.pipeline.Process(c, data, message, connectionID)
	if result.Error != nil {
		errorResp := routes.NewErrorResponse("error", result.Error.Error(), "", "")
		respData := routes.MarshalClientError(errorResp)
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
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// getOrCreateBreaker 获取或创建指定 route 的熔断器。
// security.circuitBreaker.enabled=false 时返回 nil（调用方需判空）。
func (g *Gateway) getOrCreateBreaker(route string) *security.CircuitBreaker {
	if g.circuitBreakerMgr == nil {
		return nil
	}
	failureThreshold := 5
	successThreshold := 3
	timeout := 30 * time.Second
	enabled := true
	if cfg := g.cfg.Load(); cfg != nil {
		cb := cfg.Security.CircuitBreaker
		enabled = cb.Enabled
		if cb.FailureThreshold > 0 {
			failureThreshold = cb.FailureThreshold
		}
		if cb.SuccessThreshold > 0 {
			successThreshold = cb.SuccessThreshold
		}
		if cb.Timeout != "" {
			if d, err := time.ParseDuration(cb.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}
	}
	if !enabled {
		return nil
	}
	return g.circuitBreakerMgr.GetCircuitBreaker(route, failureThreshold, successThreshold, timeout)
}

func writeFrame(c gnet.Conn, data []byte) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	c.Writev([][]byte{header[:], data})
}

func writeMsgFrame(c gnet.Conn, msg *protoGw.StreamData) {
	data, _ := routes.MarshalClientMessage(msg)
	writeFrame(c, data)
}
