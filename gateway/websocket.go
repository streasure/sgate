package gateway

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
	tlog "github.com/streasure/treasure-slog"
)

// WebSocketConnectionPool WebSocket连接对象池
var wsConnectionPool = sync.Pool{
	New: func() interface{} {
		return &WebSocketConnection{
			Buffer: make([]byte, 0, 4096),
		}
	},
}

// WebSocketConnection WebSocket连接结构
type WebSocketConnection struct {
	Conn         gnet.Conn
	State        int32 // 使用原子操作保护
	Buffer       []byte
	ConnectionID string
	LastPingTime time.Time
	BufPool      *sync.Pool // 缓冲区池
}

// WebSocket状态常量
const (
	WSStateHandshake = iota
	WSStateOpen
	WSStateClosing
	WSStateClosed
)

// NewWebSocketConnection 创建新的WebSocket连接
func NewWebSocketConnection(conn gnet.Conn) *WebSocketConnection {
	wsConn := wsConnectionPool.Get().(*WebSocketConnection)
	wsConn.Conn = conn
	atomic.StoreInt32(&wsConn.State, int32(WSStateHandshake))
	wsConn.Buffer = wsConn.Buffer[:0] // 重置缓冲区
	wsConn.ConnectionID = ""
	wsConn.LastPingTime = time.Now()
	return wsConn
}

// HandleWebSocket 处理WebSocket连接
func (g *Gateway) HandleWebSocket(c gnet.Conn, data []byte) (action gnet.Action) {
	// 从连接上下文获取WebSocket连接
	var wsConn *WebSocketConnection
	connCtx := c.Context()

	if connCtx != nil {
		if ws, ok := connCtx.(*WebSocketConnection); ok {
			wsConn = ws
		} else if id, ok := connCtx.(string); ok {
			// 已存在连接ID，创建WebSocket连接
			wsConn = NewWebSocketConnection(c)
			wsConn.ConnectionID = id
			// 保存WebSocketConnection到连接上下文
			c.SetContext(wsConn)
			// 添加到WebSocket连接集合
			g.wsConnections.Store(wsConn, true)
		}
	}

	if wsConn == nil {
		// 创建新的WebSocket连接
		wsConn = NewWebSocketConnection(c)
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
		wsConn.ConnectionID = connectionID
		// 保存WebSocketConnection到连接上下文
		c.SetContext(wsConn)
		// 添加到WebSocket连接集合
		g.wsConnections.Store(wsConn, true)
	}

	switch atomic.LoadInt32(&wsConn.State) {
	case int32(WSStateHandshake):
		// 处理握手
		return g.handleWebSocketHandshake(wsConn, data)
	case int32(WSStateOpen):
		// 处理消息
		return g.handleWebSocketMessage(wsConn, data)
	case int32(WSStateClosing):
		// 处理关闭
		return gnet.Close
	case int32(WSStateClosed):
		// 连接已关闭
		return gnet.Close
	default:
		return gnet.Close
	}
}

// handleWebSocketHandshake 处理WebSocket握手
func (g *Gateway) handleWebSocketHandshake(wsConn *WebSocketConnection, data []byte) (action gnet.Action) {
	// 解析HTTP请求
	lines := strings.Split(string(data), "\r\n")
	if len(lines) < 2 {
		tlog.Error("解析WebSocket握手请求失败: 无效的HTTP请求")
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	// 解析请求行
	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) != 3 {
		tlog.Error("解析WebSocket握手请求失败: 无效的请求行")
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	// 解析头部
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

	// 检查是否是WebSocket握手请求
	if headers["upgrade"] != "websocket" {
		tlog.Error("不是WebSocket握手请求")
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	// 生成Sec-WebSocket-Accept头
	key := headers["sec-websocket-key"]
	if key == "" {
		tlog.Error("WebSocket握手请求缺少Sec-WebSocket-Key头部")
		g.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	accept := calculateWebSocketAccept(key)

	// 构建响应
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	buf.WriteString("\r\n")

	// 发送响应
	if _, err := wsConn.Conn.Write(buf.Bytes()); err != nil {
		tlog.Error("发送WebSocket握手响应失败", "error", err)
		return gnet.Close
	}

	// 更新状态
	atomic.StoreInt32(&wsConn.State, int32(WSStateOpen))
	tlog.Debug("WebSocket握手成功")

	return gnet.None
}

// calculateWebSocketAccept 计算WebSocket Accept值
func calculateWebSocketAccept(key string) string {
	// WebSocket握手密钥计算
	const magicString = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	combined := key + magicString
	hash := sha1.Sum([]byte(combined))
	return base64.StdEncoding.EncodeToString(hash[:])
}

// parseWebSocketFrame 解析WebSocket帧
func parseWebSocketFrame(buffer []byte) (opCode ws.OpCode, payload []byte, frameSize int, err error) {
	// 尝试解析帧
	if len(buffer) < 2 {
		// 帧不完整，等待更多数据
		return 0, nil, 0, nil
	}

	// 解析基本头部
	opCode = ws.OpCode(buffer[0] & 0x0F)
	masked := (buffer[1] & 0x80) != 0
	length := uint64(buffer[1] & 0x7F)

	// 计算帧大小
	frameSize = 2 // 基本头部

	// 处理扩展长度
	if length == 126 {
		if len(buffer) < 4 {
			// 帧不完整，等待更多数据
			return 0, nil, 0, nil
		}
		length = uint64(buffer[2])<<8 | uint64(buffer[3])
		frameSize += 2
	} else if length == 127 {
		if len(buffer) < 10 {
			// 帧不完整，等待更多数据
			return 0, nil, 0, nil
		}
		length = uint64(buffer[2])<<56 | uint64(buffer[3])<<48 | uint64(buffer[4])<<40 | uint64(buffer[5])<<32 |
			uint64(buffer[6])<<24 | uint64(buffer[7])<<16 | uint64(buffer[8])<<8 | uint64(buffer[9])
		frameSize += 8
	}

	// 处理掩码
	var mask []byte
	if masked {
		if len(buffer) < frameSize+4 {
			// 帧不完整，等待更多数据
			return 0, nil, 0, nil
		}
		mask = buffer[frameSize : frameSize+4]
		frameSize += 4
	}

	// 处理payload
	if len(buffer) < frameSize+int(length) {
		// 帧不完整，等待更多数据
		return 0, nil, 0, nil
	}

	payload = buffer[frameSize : frameSize+int(length)]

	// 解码掩码
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}

	return opCode, payload, frameSize + int(length), nil
}

// handleWebSocketMessage 处理WebSocket消息
func (g *Gateway) handleWebSocketMessage(wsConn *WebSocketConnection, data []byte) (action gnet.Action) {
	// 将数据添加到缓冲区
	wsConn.Buffer = append(wsConn.Buffer, data...)

	for {
		// 解析帧
		opCode, payload, frameSize, err := parseWebSocketFrame(wsConn.Buffer)
		if err != nil || frameSize == 0 {
			// 帧不完整，等待更多数据
			return gnet.None
		}

		// 处理帧
		if err := g.processWebSocketFrame(wsConn, opCode, payload); err != nil {
			tlog.Error("处理WebSocket帧失败", "error", err)
			return gnet.Close
		}

		// 从缓冲区中移除已处理的数据
		wsConn.Buffer = wsConn.Buffer[frameSize:]

		// 如果缓冲区为空，退出循环
		if len(wsConn.Buffer) == 0 {
			break
		}
	}

	return gnet.None
}

// processWebSocketFrame 处理WebSocket帧
func (g *Gateway) processWebSocketFrame(wsConn *WebSocketConnection, opCode ws.OpCode, payload []byte) error {
	switch opCode {
	case ws.OpClose:
		// 处理关闭帧
		return g.handleWebSocketCloseFrame(wsConn)
	case ws.OpPing:
		// 处理ping帧
		return g.handleWebSocketPingFrame(wsConn, payload)
	case ws.OpPong:
		// 处理pong帧
		g.handleWebSocketPongFrame(wsConn)
	case ws.OpText, ws.OpBinary:
		// 处理数据帧
		return g.handleWebSocketDataFrame(wsConn, payload)
	default:
		// 未知操作码
		tlog.Warn("未知的WebSocket操作码", "opCode", opCode)
	}
	return nil
}



// handleWebSocketDataFrame 处理数据帧
func (g *Gateway) handleWebSocketDataFrame(wsConn *WebSocketConnection, payload []byte) error {
	// 解析消息内容
	message := &protobuf.Message{}
	if err := proto.Unmarshal(payload, message); err != nil {
		tlog.Error("解析消息失败", "error", err)
		// 发送错误响应
		errorMsg := NewErrorMessage("error", "Invalid message format", err.Error(), string(payload))
		responseData, _ := proto.Marshal(errorMsg)
		// 发送响应
		return g.sendWebSocketMessage(wsConn, ws.OpBinary, responseData)
	}

	// 处理消息
	route := message.Route
	if route == "" {
		tlog.Warn("消息格式错误，缺少route字段")
		errorMsg := NewErrorMessage("error", "Invalid message format: missing route", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		// 发送响应
		return g.sendWebSocketMessage(wsConn, ws.OpBinary, responseData)
	}

	// 获取payload
	payloadData := message.Payload

	// 获取连接ID
	connectionID := wsConn.ConnectionID

	if connectionID == "" {
		// 生成新的连接ID
		// 生成临时用户UUID，在收到第一条消息时会更新为实际的用户UUID
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID)
		wsConn.ConnectionID = connectionID
	}

	// 如果用户UUID存在，更新连接的用户映射
	if message.UserUuid != "" {
		// 生成临时用户UUID（如果是新连接）
		oldUserUUID := "temp_" + connectionID
		// 更新连接的用户映射
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, message.UserUuid)
		tlog.Debug("收到用户UUID", "connectionID", connectionID, "userUUID", message.UserUuid)
	}

	// 从对象池获取消息对象
	msg := GetMessage()
	msg.ConnectionID = connectionID
	msg.Route = route
	msg.Payload = payloadData
	msg.Conn = wsConn.Conn

	select {
	case g.messagePool <- msg:
		// 消息已加入队列
	default:
		// 消息队列已满，直接处理
		g.handleMessage(msg)
	}

	return nil
}

// handleWebSocketCloseFrame 处理关闭帧
func (g *Gateway) handleWebSocketCloseFrame(wsConn *WebSocketConnection) error {
	// 发送关闭响应
	closeFrame := []byte{0x88, 0x02, 0x03, 0xE8} // 1000 status code
	if _, err := wsConn.Conn.Write(closeFrame); err != nil {
		return err
	}

	// 更新状态
	atomic.StoreInt32(&wsConn.State, int32(WSStateClosed))
	// 从连接管理器中移除
	if wsConn.ConnectionID != "" {
		g.connectionManager.RemoveConnection(wsConn.ConnectionID)
	}
	// 从WebSocket连接集合中移除
	g.wsConnections.Delete(wsConn)
	// 归还连接对象到对象池
	wsConnectionPool.Put(wsConn)
	return nil
}

// handleWebSocketPingFrame 处理ping帧
func (g *Gateway) handleWebSocketPingFrame(wsConn *WebSocketConnection, payload []byte) error {
	// 发送pong响应
	pongFrame := append([]byte{0x8A, byte(len(payload))}, payload...)
	if _, err := wsConn.Conn.Write(pongFrame); err != nil {
		return err
	}

	// 更新心跳时间
	wsConn.LastPingTime = time.Now()
	return nil
}

// handleWebSocketPongFrame 处理pong帧
func (g *Gateway) handleWebSocketPongFrame(wsConn *WebSocketConnection) {
	// 更新心跳时间
	wsConn.LastPingTime = time.Now()
}

// sendWebSocketMessage 发送WebSocket消息
func (g *Gateway) sendWebSocketMessage(wsConn *WebSocketConnection, opCode ws.OpCode, payload []byte) error {
	// 检查连接状态
	if atomic.LoadInt32(&wsConn.State) != int32(WSStateOpen) {
		return fmt.Errorf("websocket connection not open")
	}

	// 创建帧
	frame := []byte{byte(opCode | 0x80), byte(len(payload))}
	frame = append(frame, payload...)
	// 发送帧
	if _, err := wsConn.Conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// sendHTTPResponse 发送HTTP响应
func (g *Gateway) sendHTTPResponse(conn gnet.Conn, statusCode int, statusText string, headers map[string]string) {
	var buf bytes.Buffer

	// 写入状态行
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(statusCode))
	buf.WriteString(" ")
	buf.WriteString(statusText)
	buf.WriteString("\r\n")

	// 写入头部
	if headers == nil {
		headers = make(map[string]string)
	}

	// 添加默认头部
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

	// 写入空行
	buf.WriteString("\r\n")

	// 发送响应
	conn.Write(buf.Bytes())
}

// init 初始化随机数生成器
func init() {
	rand.Seed(time.Now().UnixNano())
}
