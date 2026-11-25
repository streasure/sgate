package dataplane

import (
	"fmt"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// HandleWSData 处理WebSocket数据帧，解码消息并通过消息管道转发到逻辑层。
// transport.Runtime 实现：由 transport 帧循环对 Text/Binary 载荷回调（事件循环内）。
func (g *Engine) HandleWSData(wsConn *WebSocketConnection, payload []byte) error {
	g.messagesReceived.Add(1)

	message, ok := protoGw.DecodeClientMessage(payload)
	if !ok {
		tlog.Error(g.ctx, "WebSocket message unmarshal failed")
		// 事件循环内同步写错误帧（快速路径：直接从 wsConn 获取 Connection）
		if connID := wsConn.ConnectionID(); connID != "" {
			if conn := g.connectionManager.GetConnection(connID); conn != nil {
				_ = conn.SendSync(protoGw.NewErrorFrame("error", "Invalid message format", "invalid message frame", string(payload)))
			}
		}
		return nil
	}

	if message.Cmd == 0 {
		protoGw.PutClientMessage(message)
		if connID := wsConn.ConnectionID(); connID != "" {
			if conn := g.connectionManager.GetConnection(connID); conn != nil {
				_ = conn.SendSync(protoGw.NewErrorFrame("error", "Invalid message format: missing cmd", "", ""))
			}
		}
		return nil
	}

	// 入站数据帧刷新心跳与活跃时间（浏览器客户端不发 WS ping，仅靠 ping 会被超时踢出）
	wsConn.SetLastPingTime(time.Now())
	// 确保WebSocket连接已建立
	connectionID := wsConn.ConnectionID()
	if connectionID == "" {
		// 握手未完成或首次消息：补注册为 WS 连接（建连时绑定协议身份）
		tempUserUUID := "temp_" + GenerateConnectionID()
		connectionID = g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID, true)
		wsConn.SetConnectionID(connectionID)
	}
	// 协议身份已在建连时绑定（AddConnection isWebSocket=true）
	if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
		conn.TouchActive()
	}

	if message.Cmd == protoGw.CmdLoginGate {
		// LoginGate 走捷径不经 pipeline，补齐过载/IP 限流检查（与 TCP 路径一致）
		if g.overloadProtector != nil && g.overloadProtector.IsOverloaded() {
			g.overloadProtector.RecordDrop(1)
			g.messagesDroppedOverload.Add(1)
			ackErr := g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 503, "server overload", "")
			protoGw.PutClientMessage(message)
			return ackErr
		}
		if g.rateLimiter != nil {
			if !g.rateLimiter.AllowGlobal() || !g.rateLimiter.Allow("ip", netutil.AddrHost(wsConn.Conn.RemoteAddr())) {
				g.messagesDroppedRateLimit.Add(1)
				ackErr := g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 429, "rate limited", "")
				protoGw.PutClientMessage(message)
				return ackErr
			}
		}
		req := new(protoGw.LoginGateReq)
		if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
			ackErr := g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 400, "invalid login gate request", req.ServerId)
			protoGw.PutClientMessage(message)
			return ackErr
		}
		// 切维护检查：目标 serverId（或其所属 zone）在维护中 → 拒绝新登录
		if g.isMaintenanceTarget(req.ServerId) {
			ackErr := g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 503, "server in maintenance", req.ServerId)
			protoGw.PutClientMessage(message)
			return ackErr
		}
		// 拷贝出异步持有的副本后，原解码池消息立即归还（与 TCP 失败路径同策略）。
		msgCopy := &protoGw.StreamData{
			Cmd:   message.Cmd,
			SeqId: message.SeqId,
			Data:  append([]byte(nil), message.Data...),
		}
		if !g.acquireLoginSlot() {
			ackErr := g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 429, "too many concurrent logins", req.ServerId)
			protoGw.PutClientMessage(message)
			return ackErr
		}
		protoGw.PutClientMessage(message)
		panicutil.Go(g.ctx, "dataplane.finishWSLoginGate", func() {
			defer g.releaseLoginSlot()
			g.finishWSLoginGate(wsConn, connectionID, msgCopy, req)
		})
		return nil
	}

	if message.Cmd == protoGw.CmdLogoutGate {
		action := g.handleWSLogoutGate(wsConn, connectionID, message)
		protoGw.PutClientMessage(message)
		return action
	}

	// 异步路径：投递到 worker pool
	if g.pipelineWorkerPool != nil {
		payloadCopy := getMsgBuf(len(payload))
		copy(payloadCopy, payload)
		task := pipelineTaskPool.Get().(*pipelineTaskData)
		*task = pipelineTaskData{
			conn:         wsConn.Conn,
			data:         payloadCopy,
			message:      message,
			connectionID: connectionID,
			wsConn:       wsConn,
		}
		if !g.pipelineWorkerPool.Submit(task) {
			protoGw.PutClientMessage(task.message)
			putMsgBuf(task.data)
			pipelineTaskPool.Put(task)
		}
		return nil
	}

	result := g.pipeline.Process(wsConn.Conn, payload, message, connectionID)
	protoGw.PutClientMessage(message)
	if result.Error != nil {
		if result.ProtoMsg != nil {
			protoGw.PutStreamData(result.ProtoMsg)
		}
		// 事件循环内同步写 WS 错误帧（快速路径：直接从 connectionID 获取 Connection）
		if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
			_ = conn.SendSync(protoGw.NewErrorFrame("error", result.Error.Error(), "", ""))
		}
	}
	return nil
}

// handleWSLogoutGate 处理 WS 登出：撤销 JWT → 通知逻辑服 → 断开（尽力回写 ack 后关连接）。
func (g *Engine) handleWSLogoutGate(wsConn *WebSocketConnection, connectionID string, message *protoGw.StreamData) error {
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj != nil {
		g.revokeConnJWT(connObj)
		g.notifyLogicOffline(connObj)
	}
	ackBody, _ := proto.Marshal(&protoGw.LoginGateAck{Code: 0, Message: "ok", SessionId: connectionID})
	wsConn.State.Store(int32(WSStateClosing))
	if data, err := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: protoGw.CmdLogoutGateAck, Data: ackBody, SeqId: message.SeqId}); err == nil && connObj != nil {
		// ack 可靠投递：SendThenClose（WS 路径自动封 0x82 帧）在写完成回调内 Close，
		// 避免立即 Close 丢 ack（Windows 无 outbound flush）；不可用回退同步 Close。
		if sendErr := connObj.SendThenClose(data); sendErr == nil {
			return nil
		}
	}
	if wsConn.Conn != nil {
		_ = wsConn.Conn.Close()
	}
	return nil
}

// sendWebSocketLoginAck 发送WebSocket登录确认响应（事件循环内同步写）。
func (g *Engine) sendWebSocketLoginAck(wsConn *WebSocketConnection, connectionID string, seqID int64, code int32, text, serverID string) error {
	body, err := proto.Marshal(&protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID})
	if err != nil {
		return err
	}
	data, err := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: protoGw.CmdLoginGateAck, Data: body, SeqId: seqID})
	if err != nil {
		return err
	}
	// 事件循环内同步写（快速路径：直接从 connectionID 获取 Connection）
	conn := g.connectionManager.GetConnection(connectionID)
	if conn == nil {
		return fmt.Errorf("connection not found")
	}
	return conn.SendSync(data)
}

// finishWSLoginGate 在后台协程完成 WS 登录：loginKey 校验、绑定、ack 异步回写与逻辑转发。
func (g *Engine) finishWSLoginGate(wsConn *WebSocketConnection, connectionID string, message *protoGw.StreamData, req *protoGw.LoginGateReq) {
	ack := func(code int32, text string) {
		ackMsg := &protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: req.ServerId}
		if code == 0 {
			// UDP 关联通道：登录成功随 ack 下发一次性 token（udp.enabled=false 时为空，
			// 老客户端忽略新字段，wire 兼容）。TCP 登录不发——UDP 关联挂在 WS 会话上。
			if token, port, ok := g.issueUDPToken(connectionID); ok {
				ackMsg.UdpToken = token
				ackMsg.UdpPort = int32(port)
			}
		}
		body, _ := proto.Marshal(ackMsg)
		data, _ := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: protoGw.CmdLoginGateAck, Data: body, SeqId: message.SeqId})
		// 跨协程异步写（统一写出口，framer 自动封帧）
		_ = g.sendFrameAsync(connectionID, data)
	}
	clientIP := ""
	if wsConn.Conn != nil && wsConn.Conn.RemoteAddr() != nil {
		clientIP = netutil.AddrHost(wsConn.Conn.RemoteAddr())
	}
	if !g.validateLoginKey(req.UserId, req.LoginKey, connectionID, clientIP, req.ServerId) {
		ack(401, "invalid login key")
		return
	}
	// 二次切维护检查（worker 路径，防标记在快照后置入的竞态）
	if g.isMaintenanceTarget(req.ServerId) {
		ack(503, "server in maintenance")
		return
	}
	g.connectionManager.SetConnectionServerID(connectionID, req.ServerId)
	userUUID := req.UserId
	if userUUID == "" {
		userUUID = connectionID
	}
	fullUUID := req.ServerId + ":" + userUUID

	// 关闭同用户的旧连接（重连场景），防止资源泄漏
	if oldConnID, exists := g.connectionManager.GetUserConnection(fullUUID); exists && oldConnID != connectionID {
		if oldConn := g.connectionManager.GetConnection(oldConnID); oldConn != nil {
			tlog.Info(g.ctx, "WS检测到重复登录，关闭旧连接 oldConnectionID=%s newConnectionID=%s userUUID=%s",
				oldConnID, connectionID, fullUUID)
			g.notifyLogicOffline(oldConn)
			if oldConn.Conn != nil {
				oldConn.Conn.Close()
			}
		}
	}

	g.connectionManager.UpdateConnectionUserUUID(connectionID, fullUUID)
	// 登录账号索引（KickService KickAccounts 用）
	if req.UserId != "" {
		g.connectionManager.SetConnectionAccountID(connectionID, req.UserId)
	}
	// loginKey 留存（周期 BatchValidate 安全网重校验用）
	if req.LoginKey != "" {
		g.connectionManager.SetConnectionLoginKey(connectionID, req.LoginKey)
	}

	// 先回 ack（与 TCP 路径一致），再异步转发登录给逻辑层，避免逻辑服慢导致 ack 延迟。
	ack(0, "ok")

	// 转发登录请求到逻辑层，以便注册会话
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj != nil {
		forwardMsg := &protoGw.StreamData{
			SessionId: connectionID,
			UserKey:   connObj.GetUserUUID(),
			Data:      append([]byte(nil), message.Data...),
			Cmd:       message.Cmd,
			SeqId:     message.SeqId,
		}
		g.forwardLoginAsync(req.ServerId, forwardMsg, "WS login forward to logic timed out serverID=%s")
	}
}
