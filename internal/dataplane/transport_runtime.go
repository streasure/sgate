package dataplane

import (
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/util/tlog"
)

// 编译期断言：Gateway 实现 transport 平面的消费方接口 Runtime。

// CanAcceptConnection 实现 transport.Runtime：连接数准入检查（连接数/单 IP 上限）。
func (g *Engine) CanAcceptConnection(remoteIP string) bool {
	if !g.connectionManager.CanAccept(remoteIP) {
		tlog.Warn(g.ctx, "连接数限制，拒绝新连接 remoteIP=%s activeConnections=%d maxConnections=%d ipConnections=%d maxPerIP=%d",
			remoteIP,
			g.connectionManager.GetConnectionCount(),
			g.connectionManager.MaxConnections(),
			g.connectionManager.GetIPConnectionCount(remoteIP),
			g.connectionManager.MaxConnectionsPerIP())
		return false
	}
	return true
}

// RegisterConnection 实现 transport.Runtime：注册新连接并返回连接 ID。
// wsConn 非 nil 表示 WebSocket 连接：绑定协议身份（isWebSocket=true）、
// 写入 c.Context 并登记 wsConnections 集合。
func (g *Engine) RegisterConnection(c gnet.Conn, wsConn *WebSocketConnection) string {
	tempUserUUID := "temp_" + GenerateConnectionID()
	connectionID := g.connectionManager.AddConnection(c, tempUserUUID, wsConn != nil)
	if wsConn != nil {
		wsConn.SetConnectionID(connectionID)
		c.SetContext(wsConn)
		g.wsConnections.Store(wsConn, true)
	}
	return connectionID
}

// OnConnectionOpened 实现 transport.Runtime：连接建立计数。
func (g *Engine) OnConnectionOpened() {
	g.connectionsTotal.Add(1)
	g.connectionsActive.Add(1)
}

// OnConnectionClosed 实现 transport.Runtime：连接关闭收尾。
// wsConn 非 nil 时先从 WS 连接集合摘除并置关闭态；随后记录生命周期指标、
// 通知逻辑服离线、拆 UDP 关联、移除管理器条目；counted 递减活跃计数。
func (g *Engine) OnConnectionClosed(connectionID string, wsConn *WebSocketConnection, counted bool, err error) {
	if wsConn != nil {
		g.wsConnections.Delete(wsConn)
		wsConn.State.Store(int32(WSStateClosed))
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
		g.releaseUDPAssoc(connectionID) // 会话是根：连接关闭即拆 UDP 关联（幂等）
		g.connectionManager.RemoveConnection(connectionID)
		tlog.Debug(g.ctx, "connection closed connectionID=%s error=%v", connectionID, err)
	}
	if counted {
		g.connectionsActive.Add(-1)
	}
}

// OnWSClosed 实现 transport.Runtime：WebSocket 关闭帧的业务摘除。
// 先通知逻辑服离线再移除：OnClose 走到时 conn 已不在 manager，
// 不显式通知会漏发离线（notifyLogicOffline 幂等，重复调用安全）。
func (g *Engine) OnWSClosed(wsConn *WebSocketConnection) {
	if connID := wsConn.ConnectionID(); connID != "" {
		if connObj := g.connectionManager.GetConnection(connID); connObj != nil {
			g.notifyLogicOffline(connObj)
		}
		g.releaseUDPAssoc(connID) // 会话是根：WS 关闭帧也拆 UDP 关联（幂等，OnClose 兜底）
		g.connectionManager.RemoveConnection(connID)
	}
	g.wsConnections.Delete(wsConn)
	// 关闭底层 TCP，触发 OnClose 清理（H1：原先仅清 map 不关 socket → FD 泄漏）
	if wsConn.Conn != nil {
		_ = wsConn.Conn.Close()
	}
}

// ProxyProtocolEnabled 实现 transport.Runtime：首包 PROXY protocol 解析开关（热读配置）。
func (g *Engine) ProxyProtocolEnabled() bool {
	cfg := g.cfg.Load()
	return cfg != nil && cfg.Security.ProxyProtocol
}

// UpdateConnectionIP 实现 transport.Runtime：更新连接真实客户端 IP（反代场景）。
func (g *Engine) UpdateConnectionIP(connectionID, ip string) bool {
	return g.connectionManager.UpdateConnectionIP(connectionID, ip)
}

// HandleUDPDatagram 实现 transport.Runtime：gnet OnTraffic 的 UDP 数据报分支。
func (g *Engine) HandleUDPDatagram(c gnet.Conn, data []byte) gnet.Action {
	return g.handleUDPDatagram(c, data)
}

// Shutdown 实现 transport.Runtime：关闭流程主体（由 transport 关闭握手的
// 首个触发者调用：主动 Close 或 OnShutdown 首触发）。
// 组合方（gateway 壳）经 SetShutdownHook 注入完整生产关闭流程（含连接排空
// 与 gRPC 停止）；未注入时（测试/嵌入场景）回退业务半区收尾 Close。
func (g *Engine) Shutdown() {
	if g.shutdownHook != nil {
		g.shutdownHook()
		return
	}
	g.Close()
}
