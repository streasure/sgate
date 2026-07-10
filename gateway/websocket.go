package gateway

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

type WSOpCode byte

const (
	WSOpText   WSOpCode = 0x1
	WSOpBinary WSOpCode = 0x2
	WSOpClose  WSOpCode = 0x8
	WSOpPing   WSOpCode = 0x9
	WSOpPong   WSOpCode = 0xA
)

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

var wsConnectionPool = sync.Pool{
	New: func() interface{} {
		return &WebSocketConnection{
			Buffer: make([]byte, 0, 4096),
		}
	},
}

type WebSocketConnection struct {
	Conn         gnet.Conn
	State        int32
	Buffer       []byte
	ConnectionID string
	LastPingTime time.Time
}

const (
	WSStateHandshake = iota
	WSStateOpen
	WSStateClosing
	WSStateClosed
)

func NewWebSocketConnection(conn gnet.Conn) *WebSocketConnection {
	wsConn := wsConnectionPool.Get().(*WebSocketConnection)
	wsConn.Conn = conn
	atomic.StoreInt32(&wsConn.State, int32(WSStateHandshake))
	wsConn.Buffer = wsConn.Buffer[:0]
	wsConn.ConnectionID = ""
	wsConn.LastPingTime = time.Now()
	return wsConn
}

func (g *Gateway) HandleWebSocket(c gnet.Conn, data []byte) (action gnet.Action) {
	var wsConn *WebSocketConnection
	connCtx := c.Context()

	if connCtx != nil {
		if wsc, ok := connCtx.(*WebSocketConnection); ok {
			wsConn = wsc
		} else if id, ok := connCtx.(string); ok {
			wsConn = NewWebSocketConnection(c)
			wsConn.ConnectionID = id
			c.SetContext(wsConn)
			g.wsConnections.Store(wsConn, true)
		}
	}

	if wsConn == nil {
		wsConn = NewWebSocketConnection(c)
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
		wsConn.ConnectionID = connectionID
		c.SetContext(wsConn)
		g.wsConnections.Store(wsConn, true)
	}

	switch atomic.LoadInt32(&wsConn.State) {
	case int32(WSStateHandshake):
		return g.handleWebSocketHandshake(wsConn, data)
	case int32(WSStateOpen):
		return g.handleWebSocketMessage(wsConn, data)
	case int32(WSStateClosing), int32(WSStateClosed):
		return gnet.Close
	default:
		return gnet.Close
	}
}

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

	accept := calculateWebSocketAccept(key)

	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	buf.WriteString("\r\n")

	if _, err := wsConn.Conn.Write(buf.Bytes()); err != nil {
		tlog.Error("WebSocket handshake write failed", "error", err)
		return gnet.Close
	}

	atomic.StoreInt32(&wsConn.State, int32(WSStateOpen))

	if wsConn.ConnectionID == "" {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID)
		wsConn.ConnectionID = connectionID
	}

	if conn := g.connectionManager.GetConnection(wsConn.ConnectionID); conn != nil && !conn.IsWS {
		conn.IsWS = true
	}

	tlog.Debug("WebSocket handshake success", "connectionID", wsConn.ConnectionID)

	return gnet.None
}

func calculateWebSocketAccept(key string) string {
	combined := key + wsMagicString
	hash := sha1.Sum([]byte(combined))
	return base64.StdEncoding.EncodeToString(hash[:])
}

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

	var mask []byte
	if masked {
		if len(buffer) < frameSize+4 {
			return 0, nil, 0, nil
		}
		mask = buffer[frameSize : frameSize+4]
		frameSize += 4
	}

	if len(buffer) < frameSize+int(length) {
		return 0, nil, 0, nil
	}

	payload = buffer[frameSize : frameSize+int(length)]

	if masked && len(mask) == 4 {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}

	return opCode, payload, frameSize + int(length), nil
}

func (g *Gateway) handleWebSocketMessage(wsConn *WebSocketConnection, data []byte) (action gnet.Action) {
	if atomic.LoadInt32(&wsConn.State) == int32(WSStateHandshake) {
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
			tlog.Error("WebSocket frame process failed", "error", err)
			return gnet.Close
		}

		wsConn.Buffer = wsConn.Buffer[frameSize:]

		if len(wsConn.Buffer) == 0 {
			break
		}
	}

	return gnet.None
}

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
		tlog.Warn("unknown WebSocket opcode", "opCode", opCode)
	}
	return nil
}

func (g *Gateway) handleWebSocketDataFrame(wsConn *WebSocketConnection, payload []byte) error {
	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		return nil
	}

	message := &protobuf.Message{}
	if err := proto.Unmarshal(payload, message); err != nil {
		tlog.Error("WebSocket message unmarshal failed", "error", err)
		errorMsg := NewErrorMessage("error", "Invalid message format", err.Error(), string(payload))
		responseData, _ := proto.Marshal(errorMsg)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}

	route := message.Route
	if route == "" {
		errorMsg := NewErrorMessage("error", "Invalid message format: missing route", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}

	connectionID := wsConn.ConnectionID

	if connectionID == "" {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(wsConn.Conn, tempUserUUID)
		wsConn.ConnectionID = connectionID
	}

	if conn := g.connectionManager.GetConnection(connectionID); conn != nil && !conn.IsWS {
		conn.IsWS = true
	}

	if message.UserUuid != "" {
		oldUserUUID := "temp_" + connectionID
		g.connectionManager.UpdateUserConnection(connectionID, oldUserUUID, message.UserUuid)
		tlog.Debug("received user UUID", "connectionID", connectionID, "userUUID", message.UserUuid)
	}

	if route == protobuf.RouteHandshake {
		g.handleHandshake(wsConn.Conn, connectionID, message)
		return nil
	}

	logicClient := g.getLogicClient()
	if logicClient != nil {
		protoMsg := logicProtoMsgPool.Get().(*protobuf.Message)
		protoMsg.ConnectionId = connectionID
		protoMsg.Route = route
		protoMsg.Data = message.Data
		protoMsg.Cmd = message.Cmd

		if err := logicClient.SendMessage(protoMsg); err != nil {
			errorMsg := NewErrorMessage("error", "Failed to send message to logic server", err.Error(), "")
			responseData, _ := proto.Marshal(errorMsg)
			return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
		}
		resetLogicProtoMsg(protoMsg)
		logicProtoMsgPool.Put(protoMsg)
	} else {
		errorMsg := NewErrorMessage("error", "Logic server not connected", "", "")
		responseData, _ := proto.Marshal(errorMsg)
		return g.sendWebSocketMessage(wsConn, WSOpBinary, responseData)
	}

	return nil
}

func (g *Gateway) handleWebSocketCloseFrame(wsConn *WebSocketConnection) error {
	closeFrame := []byte{0x88, 0x02, 0x03, 0xE8}
	if _, err := wsConn.Conn.Write(closeFrame); err != nil {
		return err
	}

	atomic.StoreInt32(&wsConn.State, int32(WSStateClosed))
	if wsConn.ConnectionID != "" {
		g.connectionManager.RemoveConnection(wsConn.ConnectionID)
	}
	g.wsConnections.Delete(wsConn)
	wsConnectionPool.Put(wsConn)
	return nil
}

func (g *Gateway) handleWebSocketPingFrame(wsConn *WebSocketConnection, payload []byte) error {
	var pongFrame []byte
	payloadLen := len(payload)
	if payloadLen < 126 {
		pongFrame = make([]byte, 0, 2+payloadLen)
		pongFrame = append(pongFrame, 0x8A, byte(payloadLen))
		pongFrame = append(pongFrame, payload...)
	} else {
		pongFrame = make([]byte, 0, 4+payloadLen)
		pongFrame = append(pongFrame, 0x8A, 126)
		pongFrame = append(pongFrame, byte(payloadLen>>8), byte(payloadLen))
		pongFrame = append(pongFrame, payload...)
	}
	if _, err := wsConn.Conn.Write(pongFrame); err != nil {
		return err
	}
	wsConn.LastPingTime = time.Now()
	return nil
}

func (g *Gateway) handleWebSocketPongFrame(wsConn *WebSocketConnection) {
	wsConn.LastPingTime = time.Now()
}

func (g *Gateway) sendWebSocketMessage(wsConn *WebSocketConnection, opCode WSOpCode, payload []byte) error {
	if atomic.LoadInt32(&wsConn.State) != int32(WSStateOpen) {
		return fmt.Errorf("websocket connection not open")
	}

	var frame []byte
	payloadLen := len(payload)

	if payloadLen < 126 {
		frame = make([]byte, 0, 2+payloadLen)
		frame = append(frame, byte(opCode|0x80), byte(payloadLen))
	} else if payloadLen <= 65535 {
		frame = make([]byte, 0, 4+payloadLen)
		frame = append(frame, byte(opCode|0x80), 126)
		frame = append(frame, byte(payloadLen>>8), byte(payloadLen))
	} else {
		frame = make([]byte, 0, 10+payloadLen)
		frame = append(frame, byte(opCode|0x80), 127)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(uint64(payloadLen)>>(uint(i)*8)))
		}
	}
	frame = append(frame, payload...)

	if _, err := wsConn.Conn.Write(frame); err != nil {
		return err
	}
	return nil
}

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

	conn.Write(buf.Bytes())
}
