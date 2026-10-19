package gateway

import (
	"net/netip"

	protoGw "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// KickService 本地执行端（单向 Kick 模型）：loginserver 封禁落库后下发
// KickAccounts/KickIP，sgate 按本地连接索引踢人并断连，无任何封禁存储。

// kickNotifyParams 踢人通知的差异化参数（维护/封禁共用推送路径）。
type kickNotifyParams struct {
	reason  string
	code    int32
	message string
}

// pushKickNotify 向客户端推送踢下线通知（CMD_KICK_NOTIFY + logic.KickNotify）。
// Send 内部走 AsyncWrite，event loop 外安全。
func (g *Gateway) pushKickNotify(conn *connection.Connection, p kickNotifyParams) {
	body, err := proto.Marshal(&protoLogic.KickNotify{
		Reason:  p.reason,
		Code:    p.code,
		Message: p.message,
	})
	if err != nil {
		return
	}
	data, err := routes.MarshalClientMessage(&protoGw.StreamData{
		Cmd:  routes.CmdKickNotify,
		Data: body,
	})
	if err != nil {
		return
	}
	_ = conn.Send(data)
}

// kickConn 单连接踢下线：踢人通知 → 逻辑服下线（清会话/推送组） → 断开。
// 断开后 OnClose 会 RemoveConnection 并从网关侧全部推送组移除该成员。
//
// 通知可靠性：优先 SendThenClose（写完成后在回调内 Close，保证客户端收到
// KickNotify 后再断连）；SendThenClose 不可用（连接已空）时回退同步 Close。
func (g *Gateway) kickConn(conn *connection.Connection, p kickNotifyParams) {
	body, err := proto.Marshal(&protoLogic.KickNotify{
		Reason:  p.reason,
		Code:    p.code,
		Message: p.message,
	})
	if err == nil {
		if data, err := routes.MarshalClientMessage(&protoGw.StreamData{
			Cmd:  routes.CmdKickNotify,
			Data: body,
		}); err == nil {
			// SendThenClose 内部已注册写完成回调，回调里 Close；
			// 此处不再重复 Close，避免通知未发出就断连。
			if sendErr := conn.SendThenClose(data); sendErr == nil {
				g.notifyLogicOffline(conn)
				return
			}
		}
	}
	// 回退：通知组装/发送失败，直接下线+断连（不丢逻辑服清理）
	g.pushKickNotify(conn, p)
	g.notifyLogicOffline(conn)
	if conn.Conn != nil {
		_ = conn.Conn.Close()
	}
}

// KickByAccounts 按登录账号踢本地连接，返回踢中数。
func (g *Gateway) KickByAccounts(accountIDs []string, reason string) int {
	p := kickNotifyParams{reason: reason, code: 403, message: "account banned"}
	kicked := 0
	for _, accountID := range accountIDs {
		connID, ok := g.connectionManager.GetAccountConnection(accountID)
		if !ok {
			continue
		}
		conn := g.connectionManager.GetConnection(connID)
		if conn == nil {
			continue
		}
		g.kickConn(conn, p)
		kicked++
	}
	return kicked
}

// KickByIP 按对端 IP 踢本地全部连接，返回踢中数。
// 目标支持两种形态：
//   - 纯 IP（如 "1.2.3.4"）→ 规范化为 /32 或 /128 前缀，精确匹配
//   - CIDR（如 "10.0.0.0/8"）→ 前缀 Contains 匹配（NAT/网段封禁）
func (g *Gateway) KickByIP(ip, reason string) int {
	if ip == "" {
		return 0
	}
	prefix, ok := parseKickPrefix(ip)
	if !ok {
		tlog.Warn(g.ctx, "kick by ip invalid target ip=%s", ip)
		return 0
	}
	p := kickNotifyParams{reason: reason, code: 403, message: "ip banned"}
	var matched []*connection.Connection
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		addr, err := netip.ParseAddr(conn.RemoteHost())
		if err != nil {
			return true
		}
		if prefix.Contains(addr) {
			matched = append(matched, conn)
		}
		return true
	})
	for _, conn := range matched {
		g.kickConn(conn, p)
	}
	tlog.Info(g.ctx, "kick by ip done ip=%s prefix=%s matched=%d reason=%s", ip, prefix, len(matched), reason)
	return len(matched)
}

// parseKickPrefix 解析踢人目标为 netip.Prefix：CIDR 原样解析；纯 IP 转 /32 或 /128。
func parseKickPrefix(s string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	bits := 32
	if addr.Is6() {
		bits = 128
	}
	return netip.PrefixFrom(addr, bits), true
}
