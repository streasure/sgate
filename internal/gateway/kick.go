package gateway

import (
	"context"

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
func (g *Gateway) kickConn(conn *connection.Connection, p kickNotifyParams) {
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
func (g *Gateway) KickByIP(ip, reason string) int {
	if ip == "" {
		return 0
	}
	p := kickNotifyParams{reason: reason, code: 403, message: "ip banned"}
	var matched []*connection.Connection
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		if conn.RemoteHost() == ip {
			matched = append(matched, conn)
		}
		return true
	})
	for _, conn := range matched {
		g.kickConn(conn, p)
	}
	tlog.Info(context.TODO(), "kick by ip done ip=%s matched=%d reason=%s", ip, len(matched), reason)
	return len(matched)
}
