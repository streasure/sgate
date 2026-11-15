package transport

import (
	"fmt"
	"strings"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/dataplane"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
)

// OnOpen 新连接建立：连接数准入检查 → 判定协议分支（WS/TCP）→ 注册连接上下文。
func (s *Server) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(s.ctx, "OnOpen panic recovered error=%v", r)
			action = gnet.Close
		}
	}()

	// 连接数限制检查（P0: 防止 OOM 和连接耗尽；超限日志由业务侧记录）
	remoteIP := netutil.AddrHost(c.RemoteAddr())
	if !s.rt.CanAcceptConnection(remoteIP) {
		return nil, gnet.Close
	}

	localAddr := c.LocalAddr().String()
	isWS := false
	s.transportType.Range(func(key, value any) bool {
		port := key.(string)
		t := value.(string)
		if strings.HasSuffix(localAddr, ":"+port) && t == "websocket" {
			isWS = true
			return false
		}
		return true
	})

	if isWS {
		wsConn := dataplane.NewWebSocketConnection(c)
		// 立即注册进 connectionManager（WS 专用变体绑定协议身份）：否则未完成
		// 握手的原始 WS 连接不计入 maxConnections/maxPerIP，可被绕过打满。
		// 握手/首帧处仅在 ConnectionID 为空时补注册，此处预注册天然幂等。
		s.rt.RegisterConnection(c, wsConn)
	} else {
		connCtx := GetConnContext()
		connCtx.ConnectionID = s.rt.RegisterConnection(c, nil)
		connCtx.LogCtx = s.ctx
		c.SetContext(connCtx)
	}

	s.rt.OnConnectionOpened()

	tlog.Debug(s.ctx, "new connection localAddr=%s isWS=%v", localAddr, isWS)
	return
}

// OnClose 连接关闭：归还连接上下文（ConnContext 池），业务收尾经 Runtime 回调。
func (s *Server) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	defer panicutil.Recover(s.ctx, "transport.OnClose")

	var connectionID string
	var wsConn *dataplane.WebSocketConnection
	connCtx := c.Context()
	// OnOpen 仅在成功 SetContext 后才对 connectionsActive 计数，故以
	// connCtx != nil 作为"已计数"依据（与连接是否已注册管理器无关）。
	counted := connCtx != nil

	if connCtx != nil {
		if ctx, ok := connCtx.(*ConnContext); ok {
			connectionID = ctx.ConnectionID
			PutConnContext(ctx)
		} else if ws, ok := connCtx.(*dataplane.WebSocketConnection); ok {
			connectionID = ws.ConnectionID()
			wsConn = ws
		} else if id, ok := connCtx.(string); ok {
			connectionID = id
		}
	}

	s.rt.OnConnectionClosed(connectionID, wsConn, counted, err)

	return
}

// OnTraffic 有新数据到达：分派到 TCP / WebSocket / UDP 处理路径。
func (s *Server) OnTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(s.ctx, "OnTraffic panic recovered error=%v", fmt.Sprintf("%v", r))
			action = gnet.Close
		}
	}()

	return s.handleNormalTraffic(c)
}

func (s *Server) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(s.ctx, "handleNormalTraffic panic recovered error=%v", r)
			action = gnet.Close
		}
	}()

	data, err := c.Next(-1)
	if err != nil {
		return gnet.Close
	}

	connCtx := c.Context()

	// UDP 协议分支：无连接状态，每个数据报独立处理
	// （gnet UDP 不触发 OnOpen，首次 OnTraffic 时 Context 为 nil）
	if connCtx == nil && c.LocalAddr().Network() == "udp" {
		return s.rt.HandleUDPDatagram(c, data)
	}

	if connCtx == nil {
		return gnet.Close

	}
	if wsConn, ok := connCtx.(*dataplane.WebSocketConnection); ok {
		return s.handleWebSocketMessage(wsConn, data)
	}

	ctx, ok := connCtx.(*ConnContext)
	if !ok {
		if len(data) > 3 && data[0] == 'G' && data[1] == 'E' && data[2] == 'T' {
			// TCP 端口收到 HTTP 升级请求：提升为 WS 连接继续握手
			wsConn := dataplane.NewWebSocketConnection(c)
			s.rt.RegisterConnection(c, wsConn)
			return s.handleWebSocketMessage(wsConn, data)
		}
		return gnet.Close
	}

	// PROXY protocol v1：首包以 "PROXY " 开头时解析并取真实客户端 IP。
	// 仅 TCP 路径（WS 分支已在上方返回）；每连接只处理一次。
	if s.rt.ProxyProtocolEnabled() && !ctx.proxyHeaderParsed {
		ctx.proxyHeaderParsed = true
		if clientIP, headerLen, ok := parseProxyProtocolV1(data); ok {
			if clientIP != "" && clientIP != netutil.AddrHost(c.RemoteAddr()) {
				s.rt.UpdateConnectionIP(ctx.ConnectionID, clientIP)
				tlog.Debug(s.ctx, "proxy protocol resolved real IP conn=%s realIP=%s", ctx.ConnectionID, clientIP)
			}
			data = data[headerLen:]
			if len(data) == 0 {
				return // 头已消费完，等后续数据
			}
		}
		// 解析失败（非 PROXY 头）：按普通首包继续（不重试）
	}

	maxFrameBuf := s.rt.GetProtection().MaxFrameBufSize
	maxFrame := s.rt.GetProtection().MaxFrameSize
	if !ctx.feedFrames(data, maxFrameBuf, maxFrame, func(frame []byte) bool {
		return s.rt.HandleTCPFrame(c, ctx.ConnectionID, frame) != gnet.Close
	}) {
		return gnet.Close
	}
	return
}
