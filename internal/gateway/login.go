package gateway

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	protoLogin "github.com/streasure/protocol/loginserver"
	routes "github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/retry"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

func (g *Gateway) setLoginServerDiscovery(discovery *uetcd.Component) {
	if discovery == nil {
		return
	}
	discovery.OnServiceChange(g.handleLoginServerChange)
	for fullKey, address := range discovery.ServiceSet() {
		instanceID := fullKey[strings.LastIndex(fullKey, "/")+1:]
		g.handleLoginServerChange(uetcd.ServiceEvent{
			Type:       uetcd.EventRegister,
			ServiceID:  discovery.ServiceID(),
			InstanceID: instanceID,
			Address:    address,
		})
	}
}

func (g *Gateway) handleLoginServerChange(event uetcd.ServiceEvent) {
	if event.Type == uetcd.EventDeregister {
		return
	}
	if _, err := netip.ParseAddrPort(event.Address); err != nil {
		tlog.Warn(g.ctx, "invalid loginserver address instanceID=%s address=%s error=%v", event.InstanceID, event.Address, err)
		return
	}
	g.loginServerMu.Lock()
	defer g.loginServerMu.Unlock()
	if g.loginServerAddr == event.Address {
		return
	}
	conn, err := grpc.NewClient(event.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		tlog.Error(g.ctx, "connect loginserver failed address=%s error=%v", event.Address, err)
		return
	}
	oldConn := g.loginServerConn
	g.loginServerConn = conn
	g.loginServerClient = protoLogin.NewLoginServiceClient(conn)
	g.loginServerAddr = event.Address
	if oldConn != nil {
		_ = oldConn.Close()
	}
	tlog.Info(g.ctx, "loginserver connected instanceID=%s address=%s", event.InstanceID, event.Address)
}

// loginValidationEnabled 读取热更新后的登录校验开关。
func (g *Gateway) loginValidationEnabled() bool {
	if cfg := g.cfg.Load(); cfg != nil {
		return cfg.LoginValidation.Enabled
	}
	if cfg := config.Get(); cfg != nil {
		return cfg.LoginValidation.Enabled
	}
	return false
}

// validateLoginKey 校验 LoginGate 的 loginKey。
// loginValidation.enabled=false：永远放行（压测/免登）。
// loginValidation.enabled=true：必须走 loginserver；空 loginKey 也不能绕过；
// 无 loginserver 连接或 gRPC 失败时一律拒绝（fail-closed）。
// 会话上下文（sgate_id/conn_id/client_ip）随请求携带，loginserver 校验通过后写三键会话缓存。
func (g *Gateway) validateLoginKey(userID, loginKey, connectionID, clientIP string) bool {
	if !g.loginValidationEnabled() {
		return true
	}
	g.loginServerMu.RLock()
	client := g.loginServerClient
	g.loginServerMu.RUnlock()
	if client == nil {
		tlog.Warn(g.ctx, "login validation enabled but loginserver not connected accountId=%s", userID)
		return false
	}
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer cancel()
	ack, err := client.ValidateLoginToken(ctx, &protoLogin.ValidateLoginTokenReq{
		AccountId:  userID,
		LoginToken: loginKey,
		SgateId:    g.GetServerID(),
		ConnId:     connectionID,
		ClientIp:   clientIP,
	})
	if err != nil {
		tlog.Warn(g.ctx, "validate login token failed accountId=%s error=%v", userID, err)
		return false
	}
	return ack.Valid
}

// removeSessionAsync 连接关闭时异步通知 loginserver 清理会话缓存（Session Unbind）。
// best-effort：loginValidation 未启用或无 loginserver 连接时直接跳过；
// 失败仅 debug 日志（TTL 24h 兜底，无需重试）。
func (g *Gateway) removeSessionAsync(conn *connection.Connection) {
	if !g.loginValidationEnabled() {
		return
	}
	g.loginServerMu.RLock()
	client := g.loginServerClient
	g.loginServerMu.RUnlock()
	if client == nil {
		return
	}
	// 未登录连接（无 accountID）无会话缓存，跳过
	if conn.GetAccountID() == "" {
		return
	}
	sgateID := g.GetServerID()
	connID := conn.ID()
	panicutil.Go(g.ctx, "gateway.removeSessionAsync", func() {
		ctx, cancel := context.WithTimeout(g.ctx, 2*time.Second)
		defer cancel()
		if _, err := client.RemoveSession(ctx, &protoLogin.RemoveSessionReq{
			SgateId: sgateID,
			ConnId:  connID,
		}); err != nil {
			tlog.Debug(g.ctx, "remove session best-effort failed sgate=%s conn=%s err=%v", sgateID, connID, err)
		}
	})
}

func (g *Gateway) handleLoginGate(c gnet.Conn, connectionID string, message *protoGw.StreamData) gnet.Action {
	// LoginGate 走捷径不经 pipeline，此处补齐过载/IP 限流检查。
	// 成功路径由 finishLoginGate 异步持有 message（不归还解码池）；
	// 所有同步拒绝路径回写 ack 后立即归还，避免解码池对象泄漏到 GC。
	if g.overloadProtector != nil && g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		ack := &protoGw.LoginGateAck{Code: 503, Message: "server overload", SessionId: connectionID}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		routes.PutClientMessage(message)
		return gnet.None
	}
	if g.rateLimiter != nil {
		if !g.rateLimiter.AllowGlobal() || !g.rateLimiter.Allow("ip", netutil.AddrHost(c.RemoteAddr())) {
			g.messagesDroppedRateLimit.Add(1)
			ack := &protoGw.LoginGateAck{Code: 429, Message: "rate limited", SessionId: connectionID}
			body, _ := proto.Marshal(ack)
			writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
			routes.PutClientMessage(message)
			return gnet.None
		}
	}

	req := new(protoGw.LoginGateReq)
	if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
		writeAck := func(code int32, text, serverID string) {
			ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID}
			body, _ := proto.Marshal(ack)
			writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		}
		writeAck(400, "invalid login gate request", req.ServerId)
		routes.PutClientMessage(message)
		return gnet.None
	}

	// 切维护检查：目标 serverId（或其所属 zone）在维护中 → 拒绝新登录
	if g.isMaintenanceTarget(req.ServerId) {
		ack := &protoGw.LoginGateAck{Code: 503, Message: "server in maintenance", SessionId: connectionID, ServerId: req.ServerId}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		routes.PutClientMessage(message)
		return gnet.Close
	}

	// 并发登录上限：满则快速拒绝，防无限起 finishLoginGate 协程
	if !g.acquireLoginSlot() {
		ack := &protoGw.LoginGateAck{Code: 429, Message: "too many concurrent logins", SessionId: connectionID, ServerId: req.ServerId}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		routes.PutClientMessage(message)
		return gnet.None
	}

	// 校验与绑定在 worker 中执行，避免同步 gRPC 阻塞 event loop。
	// 成功后在 worker 中回写 LoginGateAck（writeFrame 在 worker 中改用 AsyncWrite）。
	panicutil.Go(g.ctx, "gateway.finishLoginGate", func() {
		defer g.releaseLoginSlot()
		g.finishLoginGate(c, connectionID, message, req)
	})
	return gnet.None
}

// finishLoginGate 在后台协程完成 loginKey 校验、连接绑定与 ack 回写。
func (g *Gateway) finishLoginGate(c gnet.Conn, connectionID string, message *protoGw.StreamData, req *protoGw.LoginGateReq) {
	writeAck := func(code int32, text, serverID string) {
		ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID}
		body, _ := proto.Marshal(ack)
		data, _ := routes.MarshalClientMessage(&protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		writeFrameAsync(c, data)
	}
	if !g.validateLoginKey(req.UserId, req.LoginKey, connectionID, netutil.AddrHost(c.RemoteAddr())) {
		writeAck(401, "invalid login key", req.ServerId)
		return
	}
	// 二次切维护检查（worker 路径，防标记在快照后置入的竞态）
	if g.isMaintenanceTarget(req.ServerId) {
		writeAck(503, "server in maintenance", req.ServerId)
		return
	}
	g.connectionManager.SetConnectionServerID(connectionID, req.ServerId)
	userUUID := req.UserId
	if userUUID == "" {
		userUUID = connectionID
	}
	fullUUID := req.ServerId + ":" + userUUID

	// P1: 主动关闭同用户的旧连接（重连场景），防止资源泄漏
	if oldConnID, exists := g.connectionManager.GetUserConnection(fullUUID); exists && oldConnID != connectionID {
		if oldConn := g.connectionManager.GetConnection(oldConnID); oldConn != nil {
			tlog.Info(g.ctx, "检测到重复登录，关闭旧连接 oldConnectionID=%s newConnectionID=%s userUUID=%s",
				oldConnID,
				connectionID,
				fullUUID)
			g.notifyLogicOffline(oldConn)
			if oldConn.Conn != nil {
				oldConn.Conn.Close()
			}
		}
	}

	// 保持选中的逻辑服在网关侧的身份标识中，以防止逻辑分片之间的会话索引冲突。
	g.connectionManager.UpdateConnectionUserUUID(connectionID, fullUUID)
	// 登录账号索引（KickService KickAccounts 用；userConnections 键是 fullUUID 而非裸账号）
	if req.UserId != "" {
		g.connectionManager.SetConnectionAccountID(connectionID, req.UserId)
	}
	// loginKey 留存（周期 BatchValidate 安全网重校验用）
	if req.LoginKey != "" {
		g.connectionManager.SetConnectionLoginKey(connectionID, req.LoginKey)
	}
	writeAck(0, "ok", req.ServerId)

	// 异步转发登录 StreamData 给逻辑服。
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj != nil {
		forwardMsg := &protoGw.StreamData{
			SessionId: connectionID,
			UserKey:   connObj.GetUserUUID(),
			Data:      append([]byte(nil), message.Data...),
			Cmd:       message.Cmd,
			SeqId:     message.SeqId,
		}
		g.forwardLoginAsync(req.ServerId, forwardMsg, "login forward to logic timed out serverID=%s")
	}
}

// errLogicClientUnavailable 逻辑服客户端尚未就绪（重试中间态）。
var errLogicClientUnavailable = errors.New("logic client unavailable")

// forwardLoginAsync 异步转发登录消息到逻辑服：最多 20 次、间隔 100ms，
// 全部失败时按 warnFormat（唯一参数 serverID）打点。TCP/WS 登录共用。
func (g *Gateway) forwardLoginAsync(serverID string, msg *protoGw.StreamData, warnFormat string) {
	panicutil.Go(g.ctx, "gateway.forwardLoginAsync", func() {
		err := retry.Do(g.ctx, 20, 100*time.Millisecond, func() error {
			lc := g.GetLogicClient(serverID)
			if lc == nil {
				return errLogicClientUnavailable
			}
			return lc.SendMessage(msg)
		})
		if err != nil {
			tlog.Warn(g.ctx, warnFormat, serverID)
		}
	})
}

func (g *Gateway) notifyLogicOffline(conn *connection.Connection) {
	// 幂等标记 = 已送达（非已尝试）：显式路径与 OnClose 可能都触发，
	// 已成功发送过的连接直接跳过；发送失败不置位，后续路径可重试。
	if conn.OfflineNotified() {
		return
	}
	serverID := conn.GetServerID()
	if serverID == "" {
		return
	}
	client := g.GetLogicClient(serverID)
	if client == nil {
		return
	}
	ntf := &protoGw.UserOfflineNtf{SessionId: conn.ID(), UserKey: conn.GetUserUUID(), ServerId: serverID, OfflineTime: time.Now().UnixMilli()}
	body, _ := proto.Marshal(ntf)
	data := &protoGw.StreamData{SessionId: conn.ID(), UserKey: conn.GetUserUUID(), Cmd: routes.CmdUserOffline, Data: body}
	// 调用方在 gnet 事件循环/空闲检查协程上：SendMessage 队列满时可能阻塞
	// （QueuePolicyBlock 无超时），同步发送会拖死事件循环乃至 StopConnectionChecker。
	// 仅网络入队挪到后台协程；成功后才置幂等标记，失败留给 OnClose 重试。
	panicutil.Go(g.ctx, "gateway.notifyLogicOffline", func() {
		if err := client.SendMessage(data); err != nil {
			tlog.Warn(g.ctx, "offline notify enqueue failed, left for retry connectionID=%s serverID=%s error=%v",
				conn.ID(), serverID, err)
			return
		}
		conn.MarkOfflineNotified()
	})
}
