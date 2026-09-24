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
	conn, err := grpc.Dial(event.Address,
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
	req := new(protoGw.LoginGateReq)
	writeAck := func(code int32, text, serverID string) {
		ack := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID}
		body, _ := proto.Marshal(ack)
		writeMsgFrame(c, &protoGw.StreamData{Cmd: routes.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
	}
	if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
		writeAck(400, "invalid login gate request", req.ServerId)
		return gnet.None
	}
	if !g.validateLoginKey(req.UserId, req.LoginKey) {
		writeAck(401, "invalid login key", req.ServerId)
		return gnet.None
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

	// 异步转发登录 StreamData 给逻辑服（不阻塞 gnet 事件循环）。
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
			for i := 0; i < 20; i++ {
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

	return gnet.None
}

func (g *Gateway) notifyLogicOffline(conn *connection.Connection) {
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
