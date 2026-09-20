package internal

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ws "github.com/gobwas/ws"
	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// WSOpCode 表示WebSocket操作码
type WSOpCode byte

const (
	WSOpText   WSOpCode = 0x1 // 文本帧
	WSOpBinary WSOpCode = 0x2 // 二进制帧
	WSOpClose  WSOpCode = 0x8 // 关闭帧
	WSOpPing   WSOpCode = 0x9 // 心跳探测帧
	WSOpPong   WSOpCode = 0xA // 心跳回复帧
)

// wsMagicString WebSocket协议握手用的魔术字符串
const wsMagicString = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func (g *Gateway) getMaxWSFrameSize() int {
	if g == nil || g.protection.MaxWSFrameSize <= 0 {
		return 4 * 1024 * 1024
	}
	return g.protection.MaxWSFrameSize
}

func (g *Gateway) getMaxWSBufferSize() int {
	if g == nil || g.protection.MaxWSBufferSize <= 0 {
		return 4 * 1024 * 1024
	}
	return g.protection.MaxWSBufferSize
}

// WebSocketConnection 表示WebSocket连接，包含缓冲区和状态信息。
// 不使用 sync.Pool — 指针作为 sync.Map key 时，pool 复用会导致陈旧 entry。
type WebSocketConnection struct {
	Conn         gnet.Conn
	State        atomic.Int32
	Buffer       []byte
	ConnectionID string
	LastPingTime time.Time
}

// WebSocket连接状态常量
const (
	WSStateHandshake = iota // 握手中
	WSStateOpen             // 已打开
	WSStateClosing          // 关闭中
	WSStateClosed           // 已关闭
)

// NewWebSocketConnection 创建 WebSocket 连接并初始化状态。
func NewWebSocketConnection(conn gnet.Conn) *WebSocketConnection {
	return &WebSocketConnection{
		Conn:         conn,
		Buffer:       make([]byte, 0, 4096),
		LastPingTime: time.Now(),
	}
}

// handleWebSocketHandshake 处理WebSocket协议升级握手，验证请求头并返回101响应。
func (g *Gateway) handleWebSocketHandshake(wsConn *WebSocketConnection, data []byte) (action gnet.Action) {
	lines := strings.Split(string(data), "\r\n")
	if len(lines) < 2 {
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) != 3 {
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	headers := make(map[string]string)
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) == 2 {
			headers[strings.ToLower(parts[0])] = parts[1]
		}
	}

	if headers["upgrade"] != "websocket" {
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	key := headers["sec-websocket-key"]
	if key == "" {
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	combined := key + wsMagicString
	hash := sha1.Sum([]byte(combined))
	accept := base64.StdEncoding.EncodeToString(hash[:])

	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	buf.WriteString("\r\n")

	if _, err := wsConn.Conn.Write(buf.Bytes()); err != nil {
		tlog.Error(context.Background(), "WebSocket handshake write failed error=%v", err)
		return gnet.Close
	}

	wsConn.State.Store(int32(WSStateOpen))

	if wsConn.ConnectionID == "" {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID)
		wsConn.ConnectionID = connectionID
	}

	if conn := g.connectionManager.GetConnection(wsConn.ConnectionID); conn != nil && !conn.IsWebSocket() {
		conn.SetWS(true)
	}

	tlog.Debug(context.Background(), "WebSocket handshake success connectionID=%s", wsConn.ConnectionID)

	return gnet.None
}

// parseWebSocketFrame 从缓冲区解析WebSocket帧，提取操作码和载荷数据。
// 支持不同长度的载荷（7位、16位、64位长度编码）和掩码解码。
func parseWebSocketFrame(buffer []byte, maxFrameSize int) (opCode WSOpCode, payload []byte, frameSize int, err error) {
	if len(buffer) < 2 {
		return 0, nil, 0, nil
	}

	opCode = WSOpCode(buffer[0] & 0x0F)
	masked := (buffer[1] & 0x80) != 0
	length := uint64(buffer[1] & 0x7F)

	frameSize = 2

	if length == 126 {
		if len(buffer) < 4 {
			return 0, nil, 0, nil
		}
		length = uint64(buffer[2])<<8 | uint64(buffer[3])
		frameSize += 2
	} else if length == 127 {
		if len(buffer) < 10 {
			return 0, nil, 0, nil
		}
		length = uint64(buffer[2])<<56 | uint64(buffer[3])<<48 | uint64(buffer[4])<<40 | uint64(buffer[5])<<32 |
			uint64(buffer[6])<<24 | uint64(buffer[7])<<16 | uint64(buffer[8])<<8 | uint64(buffer[9])
		frameSize += 8
	}

	if length > uint64(maxFrameSize) {
		return 0, nil, 0, fmt.Errorf("frame too large: %d bytes", length)
	}

	var mask [4]byte
	if masked {
		if len(buffer) < frameSize+4 {
			return 0, nil, 0, nil
		}
		copy(mask[:], buffer[frameSize:frameSize+4])
		frameSize += 4
	}

	if len(buffer) < frameSize+int(length) {
		return 0, nil, 0, nil
	}

	payload = buffer[frameSize : frameSize+int(length)]

	if masked {
		ws.Cipher(payload, mask, 0)
	}

	return opCode, payload, frameSize + int(length), nil
}

// handleWebSocketMessage 处理接收到的WebSocket数据，将其追加到缓冲区并逐帧解析处理。
func (g *Gateway) handleWebSocketMessage(wsConn *WebSocketConnection, data []byte) (action gnet.Action) {
	if wsConn.State.Load() == int32(WSStateHandshake) {
		return g.handleWebSocketHandshake(wsConn, data)
	}

	if len(wsConn.Buffer)+len(data) > g.getMaxWSBufferSize() {
		wsConn.Buffer = nil
		return gnet.Close
	}
	wsConn.Buffer = append(wsConn.Buffer, data...)

	for {
		opCode, payload, frameSize, err := parseWebSocketFrame(wsConn.Buffer, g.getMaxWSFrameSize())
		if err != nil || frameSize == 0 {
			return gnet.None
		}

		if err := g.processWebSocketFrame(wsConn, opCode, payload); err != nil {
			tlog.Error(context.Background(), "WebSocket frame process failed error=%v", err)
			return gnet.Close
		}

		// 剩余数据较小时复制到紧凑 buffer，避免大数组长期驻留内存
		remaining := len(wsConn.Buffer) - frameSize
		if remaining > 0 && remaining < cap(wsConn.Buffer)/4 {
			newBuf := make([]byte, remaining)
			copy(newBuf, wsConn.Buffer[frameSize:])
			wsConn.Buffer = newBuf
		} else {
			wsConn.Buffer = wsConn.Buffer[frameSize:]
		}

		if len(wsConn.Buffer) == 0 {
			break
		}
	}

	return gnet.None
}

// processWebSocketFrame 根据操作码分派处理WebSocket帧（关闭、心跳、数据帧等）。
func (g *Gateway) processWebSocketFrame(wsConn *WebSocketConnection, opCode WSOpCode, payload []byte) error {
	switch opCode {
	case WSOpClose:
		return g.handleWebSocketCloseFrame(wsConn)
	case WSOpPing:
		return g.handleWebSocketPingFrame(wsConn, payload)
	case WSOpPong:
		g.handleWebSocketPongFrame(wsConn)
	case WSOpText, WSOpBinary:
		return g.handleWebSocketDataFrame(wsConn, payload)
	default:
		tlog.Warn(context.Background(), "unknown WebSocket opcode opCode=%d", opCode)
	}
	return nil
}

// handleWebSocketDataFrame 处理WebSocket数据帧，解码消息并通过消息管道转发到逻辑层。
func (g *Gateway) handleWebSocketDataFrame(wsConn *WebSocketConnection, payload []byte) error {
	g.messagesReceived.Add(1)

	message, ok := decodeClientMessage(payload)
	if !ok {
		tlog.Error(context.Background(), "WebSocket message unmarshal failed")
		errorMsg := newErrorResponse("error", "Invalid message format", "invalid message frame", string(payload))
		responseData := marshalClientError(errorMsg)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}

	if message.Cmd == 0 {
		errorMsg := newErrorResponse("error", "Invalid message format: missing cmd", "", "")
		responseData := marshalClientError(errorMsg)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}

	// 确保WebSocket连接已建立
	connectionID := wsConn.ConnectionID
	if connectionID == "" {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID)
		wsConn.ConnectionID = connectionID
	}
	if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
		conn.SetWS(true)
	}

	if message.Cmd == gateway.CmdLoginGate {
		req := new(protoGw.LoginGateReq)
		if err := proto.Unmarshal(message.Data, req); err != nil || req.ServerId == "" {
			return g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 400, "invalid login gate request", req.ServerId)
		}
		if !g.validateLoginKey(req.UserId, req.LoginKey) {
			return g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 401, "invalid login key", req.ServerId)
		}
		g.connectionManager.SetConnectionServerID(connectionID, req.ServerId)
		userUUID := req.UserId
		if userUUID == "" {
			userUUID = connectionID
		}
		g.connectionManager.UpdateConnectionUserUUID(connectionID, req.ServerId+":"+userUUID)

		// 转发登录请求到逻辑层，以便注册会话
		connObj := g.connectionManager.GetConnection(connectionID)
		if connObj != nil {
			if lc := g.GetLogicClient(req.ServerId); lc != nil {
				_ = lc.SendMessage(&protoGw.StreamData{
					SessionId: connectionID,
					UserKey:   connObj.GetUserUUID(),
					Data:      append([]byte(nil), message.Data...),
					Cmd:       message.Cmd,
					SeqId:     message.SeqId,
				})
			}
		}

		return g.sendWebSocketLoginAck(wsConn, connectionID, message.SeqId, 0, "ok", req.ServerId)
	}

	result := g.pipeline.ProcessForWS(wsConn.Conn, payload, message, connectionID)
	if result.Error != nil {
		errorResp := newErrorResponse("error", result.Error.Error(), "", "")
		responseData := marshalClientError(errorResp)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}
	return nil
}

// sendWebSocketLoginAck 发送WebSocket登录确认响应。
func (g *Gateway) sendWebSocketLoginAck(wsConn *WebSocketConnection, connectionID string, seqID int64, code int32, text, serverID string) error {
	body, err := proto.Marshal(&protoGw.LoginGateAck{Code: code, Message: text, SessionId: connectionID, ServerId: serverID})
	if err != nil {
		return err
	}
	data, err := marshalClientMessage(&protoGw.StreamData{Cmd: gateway.CmdLoginGateAck, Data: body, SeqId: seqID})
	if err != nil {
		return err
	}
	return g.sendWebSocketMessage(wsConn, WSOpBinary, data)
}

// handleWebSocketCloseFrame 处理WebSocket关闭帧，发送关闭响应并清理连接资源。
func (g *Gateway) handleWebSocketCloseFrame(wsConn *WebSocketConnection) error {
	closeFrame := []byte{0x88, 0x02, 0x03, 0xE8}
	if _, err := wsConn.Conn.Write(closeFrame); err != nil {
		return err
	}

	wsConn.State.Store(int32(WSStateClosed))
	if wsConn.ConnectionID != "" {
		g.connectionManager.RemoveConnection(wsConn.ConnectionID)
	}
	g.wsConnections.Delete(wsConn)
	return nil
}

// handleWebSocketPingFrame 处理WebSocket心跳探测帧，返回心跳回复帧并更新最后活跃时间。
func (g *Gateway) handleWebSocketPingFrame(wsConn *WebSocketConnection, payload []byte) error {
	frame := encodeWSFrame(0x8A, payload)
	if _, err := wsConn.Conn.Write(frame); err != nil {
		return err
	}
	wsConn.LastPingTime = time.Now()
	return nil
}

// handleWebSocketPongFrame 处理WebSocket心跳回复帧，更新最后活跃时间。
func (g *Gateway) handleWebSocketPongFrame(wsConn *WebSocketConnection) {
	wsConn.LastPingTime = time.Now()
}

// encodeWSFrame 构造 WebSocket 帧（服务端发送，FIN=1, 无 mask）
func encodeWSFrame(opCode byte, payload []byte) []byte {
	n := len(payload)
	switch {
	case n < 126:
		frame := make([]byte, 0, 2+n)
		frame = append(frame, opCode, byte(n))
		return append(frame, payload...)
	case n <= 65535:
		frame := make([]byte, 0, 4+n)
		frame = append(frame, opCode, 126, byte(n>>8), byte(n))
		return append(frame, payload...)
	default:
		frame := make([]byte, 0, 10+n)
		frame = append(frame, opCode, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		frame = append(frame, b[:]...)
		return append(frame, payload...)
	}
}

// sendWebSocketMessage 向WebSocket连接发送指定操作码的消息帧，自动处理不同长度载荷的帧头编码。
func (g *Gateway) sendWebSocketMessage(wsConn *WebSocketConnection, opCode WSOpCode, payload []byte) error {
	if wsConn.State.Load() != int32(WSStateOpen) {
		return fmt.Errorf("websocket connection not open")
	}
	frame := encodeWSFrame(byte(opCode|0x80), payload)
	_, err := wsConn.Conn.Write(frame)
	return err
}

// sendHTTPResponse 发送HTTP响应，用于WebSocket握手失败时返回错误响应。
func (g *Gateway) sendHTTPResponse(conn gnet.Conn, statusCode int, statusText string, headers map[string]string) {
	var buf bytes.Buffer

	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(statusCode))
	buf.WriteString(" ")
	buf.WriteString(statusText)
	buf.WriteString("\r\n")

	if headers == nil {
		headers = make(map[string]string)
	}

	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "text/plain"
	}
	if _, ok := headers["Connection"]; !ok {
		headers["Connection"] = "close"
	}

	for key, value := range headers {
		buf.WriteString(key)
		buf.WriteString(": ")
		buf.WriteString(value)
		buf.WriteString("\r\n")
	}

	buf.WriteString("\r\n")

	if _, err := conn.Write(buf.Bytes()); err != nil {
		tlog.Debug(context.Background(), "write HTTP response failed error=%v", err)
	}
}
