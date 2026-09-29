package gateway

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	protoLogin "github.com/streasure/protocol/loginserver"
	routes "github.com/streasure/sgate/internal/routes"
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
		tlog.Warn(context.TODO(), "invalid loginserver address instanceID=%s address=%s error=%v", event.InstanceID, event.Address, err)
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
		tlog.Error(context.TODO(), "connect loginserver failed address=%s error=%v", event.Address, err)
		return
	}
	oldConn := g.loginServerConn
	g.loginServerConn = conn
	g.loginServerClient = protoLogin.NewLoginServiceClient(conn)
	g.loginServerAddr = event.Address
	if oldConn != nil {
		_ = oldConn.Close()
	}
	tlog.Info(context.TODO(), "loginserver connected instanceID=%s address=%s", event.InstanceID, event.Address)
}

// loginValidationEnabled 读取热更新后的登录校验开关。
func (g *Gateway) loginValidationEnabled() bool {
	if cfg, ok := g.cfg.Load().(*config.Config); ok && cfg != nil {
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
func (g *Gateway) validateLoginKey(userID, loginKey string) bool {
	if !g.loginValidationEnabled() {
		return true
	}
	g.loginServerMu.RLock()
	client := g.loginServerClient
	g.loginServerMu.RUnlock()
	if client == nil {
		tlog.Warn(context.TODO(), "login validation enabled but loginserver not connected accountId=%s", userID)
		return false
	}
	ctx, cancel := context.WithTimeout(context.TODO(), 3*time.Second)
	defer cancel()
	ack, err := client.ValidateLoginToken(ctx, &protoLogin.ValidateLoginTokenReq{AccountId: userID, LoginToken: loginKey})
	if err != nil {
		tlog.Warn(context.TODO(), "validate login token failed accountId=%s error=%v", userID, err)
		return false
	}
	return ack.Valid
}

func (g *Gateway) handleLoginGate(c gnet.Conn, connectionID string, message *protoGw.StreamData) gnet.Action {
	// LoginGate 走捷径不经 pipeline，此处补齐过载/IP 限流检查
	if g.overloadProtector != nil && g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		ack := &protoGw.LoginGateAck{Code: 503, Message: "server overload", SessionId: connectionID}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		return gnet.None
	}
	if g.rateLimiter != nil && !g.rateLimiter.Allow("ip", getRemoteIP(c)) {
		g.messagesDroppedRateLimit.Add(1)
		ack := &protoGw.LoginGateAck{Code: 429, Message: "rate limited", SessionId: connectionID}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		return gnet.None
	}

	req := new(protoGw.LoginGateReq)
	if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
		writeAck := func(code int32, text, serverID string) {
			ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID}
			body, _ := proto.Marshal(ack)
			writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		}
		writeAck(400, "invalid login gate request", req.ServerId)
		return gnet.None
	}

	// 登录前封禁检查（内存 BanStore；TODO 迁 MySQL）
	if g.rejectIfBanned(connectionID, req.UserId, func(code int32, text string) {
		ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: req.ServerId}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
	}) {
		return gnet.Close
	}

	// 并发登录上限：满则快速拒绝，防无限起 finishLoginGate 协程
	if !g.acquireLoginSlot() {
		ack := &protoGw.LoginGateAck{Code: 429, Message: "too many concurrent logins", SessionId: connectionID, ServerId: req.ServerId}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		return gnet.None
	}

	// 校验与绑定在 worker 中执行，避免同步 gRPC 阻塞 event loop。
	// 成功后在 worker 中回写 LoginGateAck（writeFrame 在 worker 中改用 AsyncWrite）。
	go func() {
		defer g.releaseLoginSlot()
		g.finishLoginGate(c, connectionID, message, req)
	}()
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
	if !g.validateLoginKey(req.UserId, req.LoginKey) {
		writeAck(401, "invalid login key", req.ServerId)
		return
	}
	// 二次封禁检查（worker 路径，防竞态补写）
	if g.rejectIfBanned(connectionID, req.UserId, func(code int32, text string) {
		writeAck(code, text, req.ServerId)
	}) {
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
			tlog.Info(context.TODO(), "检测到重复登录，关闭旧连接 oldConnectionID=%s newConnectionID=%s userUUID=%s",
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
		go func() {
			for range 20 {
				if lc := g.GetLogicClient(req.ServerId); lc != nil {
					if err := lc.SendMessage(forwardMsg); err == nil {
						return
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			tlog.Warn(context.TODO(), "login forward to logic timed out serverID=%s", req.ServerId)
		}()
	}
}

func (g *Gateway) notifyLogicOffline(conn *connection.Connection) {
	// 幂等：显式路径与 OnClose 可能都触发，仅首个调用者发送
	if !conn.ClaimOfflineNotify() {
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
	_ = client.SendMessage(&protoGw.StreamData{SessionId: conn.ID(), UserKey: conn.GetUserUUID(), Cmd: routes.CmdUserOffline, Data: body})
}
