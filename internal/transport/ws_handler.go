package transport

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	ws "github.com/gobwas/ws"
	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/dataplane"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
)

// wsMagicString WebSocket协议握手用的魔术字符串
const wsMagicString = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxWSHandshakeSize WebSocket 握手请求最大字节数（防半包无限缓冲）。
const maxWSHandshakeSize = 16 * 1024

func (s *Server) getMaxWSFrameSize() int {
	p := s.rt.GetProtection()
	if s == nil || p.MaxWSFrameSize <= 0 {
		return 4 * 1024 * 1024
	}
	return p.MaxWSFrameSize
}

func (s *Server) getMaxWSBufferSize() int {
	p := s.rt.GetProtection()
	if s == nil || p.MaxWSBufferSize <= 0 {
		return 4 * 1024 * 1024
	}
	return p.MaxWSBufferSize
}

// handleWebSocketHandshake 处理WebSocket协议升级握手，验证请求头并返回101响应。
func (s *Server) handleWebSocketHandshake(wsConn *dataplane.WebSocketConnection, data []byte) (action gnet.Action) {
	lines := strings.Split(string(data), "\r\n")
	if len(lines) < 2 {
		s.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	reqLine := strings.Fields(lines[0])
	if len(reqLine) != 3 {
		s.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	headers := make(map[string]string)
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ": "); ok {
			headers[strings.ToLower(name)] = value
		}
	}

	if headers["upgrade"] != "websocket" {
		s.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
		return gnet.Close
	}

	key := headers["sec-websocket-key"]
	if key == "" {
		s.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
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
		tlog.Error(s.ctx, "WebSocket handshake write failed error=%v", err)
		return gnet.Close
	}

	wsConn.State.Store(int32(dataplane.WSStateOpen))

	if wsConn.ConnectionID() == "" {
		s.rt.RegisterConnection(wsConn.Conn, wsConn)
	}

	// 反代场景：直连对端是可信代理时，从 X-Real-IP / X-Forwarded-For 解析真实
	// 客户端 IP，覆盖 RemoteHost 缓存并重键 per-IP 计数。须在首帧（pipeline 安全链
	// 首次读 RemoteHost）之前完成——握手在首帧之前，满足约束。
	if s.rt.IsTrustedProxy(netutil.AddrHost(wsConn.Conn.RemoteAddr())) {
		if realIP := resolveRealClientIP(headers); realIP != "" && realIP != netutil.AddrHost(wsConn.Conn.RemoteAddr()) {
			if s.rt.UpdateConnectionIP(wsConn.ConnectionID(), realIP) {
				tlog.Debug(s.ctx, "ws trusted proxy resolved real IP connectionID=%s realIP=%s", wsConn.ConnectionID(), realIP)
			}
		}
	}

	// 协议身份已在建连时绑定（AddConnection isWebSocket=true），无需后续设置

	tlog.Debug(s.ctx, "WebSocket handshake success connectionID=%s", wsConn.ConnectionID())

	return gnet.None
}

// resolveRealClientIP 从 WS 握手头解析真实客户端 IP：
// 优先 X-Real-IP（Nginx proxy_set_header）；否则取 X-Forwarded-For 最左项
// （客户端首个添加的 = 真实源，后续是逐级代理追加）。均无或非法返回空串。
func resolveRealClientIP(headers map[string]string) string {
	if v := strings.TrimSpace(headers["x-real-ip"]); v != "" {
		if _, err := netip.ParseAddr(v); err == nil {
			return v
		}
	}
	if xff := strings.TrimSpace(headers["x-forwarded-for"]); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(first); first != "" {
			if _, err := netip.ParseAddr(first); err == nil {
				return first
			}
		}
	}
	return ""
}

// parseWebSocketFrame 从缓冲区解析WebSocket帧，提取操作码、载荷、FIN 与 mask 位。
// 支持不同长度的载荷（7位、16位、64位长度编码）和掩码解码。
func parseWebSocketFrame(buffer []byte, maxFrameSize int) (opCode dataplane.WSOpCode, payload []byte, frameSize int, fin bool, masked bool, err error) {
	if len(buffer) < 2 {
		return 0, nil, 0, false, false, nil
	}

	fin = (buffer[0] & 0x80) != 0
	opCode = dataplane.WSOpCode(buffer[0] & 0x0F)
	masked = (buffer[1] & 0x80) != 0
	length := uint64(buffer[1] & 0x7F)

	frameSize = 2

	if length == 126 {
		if len(buffer) < 4 {
			return 0, nil, 0, false, false, nil
		}
		length = uint64(buffer[2])<<8 | uint64(buffer[3])
		frameSize += 2
	} else if length == 127 {
		if len(buffer) < 10 {
			return 0, nil, 0, false, false, nil
		}
		length = uint64(buffer[2])<<56 | uint64(buffer[3])<<48 | uint64(buffer[4])<<40 | uint64(buffer[5])<<32 |
			uint64(buffer[6])<<24 | uint64(buffer[7])<<16 | uint64(buffer[8])<<8 | uint64(buffer[9])
		frameSize += 8
	}

	if length > uint64(maxFrameSize) {
		return 0, nil, 0, false, false, fmt.Errorf("frame too large: %d bytes", length)
	}

	var mask [4]byte
	if masked {
		if len(buffer) < frameSize+4 {
			return 0, nil, 0, false, false, nil
		}
		copy(mask[:], buffer[frameSize:frameSize+4])
		frameSize += 4
	}

	if len(buffer) < frameSize+int(length) {
		return 0, nil, 0, false, false, nil
	}

	payload = buffer[frameSize : frameSize+int(length)]

	if masked {
		ws.Cipher(payload, mask, 0)
	}

	return opCode, payload, frameSize + int(length), fin, masked, nil
}

// handleWebSocketMessage 处理接收到的WebSocket数据，将其追加到缓冲区并逐帧解析处理。
func (s *Server) handleWebSocketMessage(wsConn *dataplane.WebSocketConnection, data []byte) (action gnet.Action) {
	if wsConn.State.Load() == int32(dataplane.WSStateHandshake) {
		// 半包累积：跨 packet 等待完整 \r\n\r\n 再握手
		if len(wsConn.Buffer)+len(data) > maxWSHandshakeSize {
			s.sendHTTPResponse(wsConn.Conn, 400, "Bad Request", nil)
			return gnet.Close
		}
		wsConn.Buffer = append(wsConn.Buffer, data...)
		idx := bytes.Index(wsConn.Buffer, []byte("\r\n\r\n"))
		if idx < 0 {
			return gnet.None // 等待更多数据
		}
		handshake := wsConn.Buffer[:idx+4]
		rest := wsConn.Buffer[idx+4:]
		action = s.handleWebSocketHandshake(wsConn, handshake)
		if action == gnet.Close {
			return action
		}
		// 握手成功后残留数据按帧继续处理（复用底层数组容量）
		wsConn.Buffer = append(wsConn.Buffer[:0], rest...)
		wsConn.BufOff = 0
		if len(wsConn.Buffer) == 0 {
			return gnet.None
		}
		// fall through 到帧解析
	} else {
		// 有已消费前缀时前移残留，回收底层数组头部容量（append 热路径零重分配）
		if wsConn.BufOff > 0 {
			logical := len(wsConn.Buffer) - wsConn.BufOff
			copy(wsConn.Buffer, wsConn.Buffer[wsConn.BufOff:])
			wsConn.Buffer = wsConn.Buffer[:logical]
			wsConn.BufOff = 0
		}
		if len(wsConn.Buffer)+len(data) > s.getMaxWSBufferSize() {
			tlog.Warn(s.ctx, "ws buffer overflow, closing connectionID=%s pending=%d incoming=%d maxWSBufferSize=%d",
				wsConn.ConnectionID(), len(wsConn.Buffer), len(data), s.getMaxWSBufferSize())
			wsConn.Buffer = nil
			return gnet.Close
		}
		wsConn.Buffer = append(wsConn.Buffer, data...)
	}

	for {
		opCode, payload, frameSize, fin, masked, err := parseWebSocketFrame(wsConn.Buffer[wsConn.BufOff:], s.getMaxWSFrameSize())
		if err != nil {
			return gnet.Close
		}
		if frameSize == 0 {
			return gnet.None
		}
		// RFC6455：客户端帧必须 mask
		if !masked {
			tlog.Warn(s.ctx, "unmasked client frame rejected connectionID=%s", wsConn.ConnectionID())
			return gnet.Close
		}

		// 分片重组
		if opCode == 0x0 { // continuation
			if !wsConn.Fragging {
				tlog.Warn(s.ctx, "unexpected continuation frame connectionID=%s", wsConn.ConnectionID())
				return gnet.Close
			}
			if len(wsConn.FragBuf)+len(payload) > s.getMaxWSFrameSize() {
				return gnet.Close
			}
			wsConn.FragBuf = append(wsConn.FragBuf, payload...)
			if fin {
				msg := wsConn.FragBuf
				op := wsConn.FragOp
				wsConn.FragBuf = wsConn.FragBuf[:0]
				wsConn.Fragging = false
				if err := s.processWebSocketFrame(wsConn, op, msg); err != nil {
					tlog.Error(s.ctx, "WebSocket frame process failed error=%v", err)
					return gnet.Close
				}
			}
		} else if !fin {
			// 首片分帧
			wsConn.Fragging = true
			wsConn.FragOp = opCode
			wsConn.FragBuf = append(wsConn.FragBuf[:0], payload...)
		} else {
			if err := s.processWebSocketFrame(wsConn, opCode, payload); err != nil {
				tlog.Error(s.ctx, "WebSocket frame process failed error=%v", err)
				return gnet.Close
			}
		}

		wsConn.BufOff += frameSize

		if wsConn.BufOff == len(wsConn.Buffer) {
			// 全部帧消费完：保留底层数组容量（[:0] 而非 nil），下次读事件直接复用
			wsConn.Buffer = wsConn.Buffer[:0]
			wsConn.BufOff = 0
			break
		}
	}

	return gnet.None
}

// processWebSocketFrame 根据操作码分派处理WebSocket帧（关闭、心跳、数据帧等）。
// 数据帧（Text/Binary）属业务消息，经 Runtime 回调业务平面。
func (s *Server) processWebSocketFrame(wsConn *dataplane.WebSocketConnection, opCode dataplane.WSOpCode, payload []byte) error {
	switch opCode {
	case dataplane.WSOpClose:
		return s.handleWebSocketCloseFrame(wsConn)
	case dataplane.WSOpPing:
		return s.handleWebSocketPingFrame(wsConn, payload)
	case dataplane.WSOpPong:
		s.handleWebSocketPongFrame(wsConn)
	case dataplane.WSOpText, dataplane.WSOpBinary:
		return s.rt.HandleWSData(wsConn, payload)
	default:
		tlog.Warn(s.ctx, "unknown WebSocket opcode opCode=%d", opCode)
	}
	return nil
}

// handleWebSocketCloseFrame 处理WebSocket关闭帧：回写 close 帧并通知业务侧摘除
// 会话（离线通知/拆 UDP 关联/移除管理器/wsConnections），随后关闭底层 TCP 触发
// OnClose 兜底，避免僵尸 FD。
func (s *Server) handleWebSocketCloseFrame(wsConn *dataplane.WebSocketConnection) error {
	closeFrame := []byte{0x88, 0x02, 0x03, 0xE8}
	if _, err := wsConn.Conn.Write(closeFrame); err != nil {
		tlog.Debug(s.ctx, "ws close frame write failed error=%v", err)
	}

	wsConn.State.Store(int32(dataplane.WSStateClosed))
	s.rt.OnWSClosed(wsConn)
	return nil
}

// handleWebSocketPingFrame 处理WebSocket心跳探测帧，返回心跳回复帧并更新最后活跃时间。
func (s *Server) handleWebSocketPingFrame(wsConn *dataplane.WebSocketConnection, payload []byte) error {
	frame := dataplane.EncodeWSFrame(0x8A, payload)
	if _, err := wsConn.Conn.Write(frame); err != nil {
		return err
	}
	wsConn.SetLastPingTime(time.Now())
	return nil
}

// handleWebSocketPongFrame 处理WebSocket心跳回复帧，更新最后活跃时间。
func (s *Server) handleWebSocketPongFrame(wsConn *dataplane.WebSocketConnection) {
	wsConn.SetLastPingTime(time.Now())
}

// sendHTTPResponse 发送HTTP响应，用于WebSocket握手失败时返回错误响应。
func (s *Server) sendHTTPResponse(conn gnet.Conn, statusCode int, statusText string, headers map[string]string) {
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
		tlog.Debug(s.ctx, "write HTTP response failed error=%v", err)
	}
}
