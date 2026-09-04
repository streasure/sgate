package codec

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/panjf2000/gnet/v2"
)

const maxWebSocketMessageSize = 4 * 1024 * 1024

var ErrWebSocketClose = errors.New("websocket close frame received")

// WebSocketCodec handles an RFC 6455 upgrade and binary WebSocket messages.
// A codec belongs to exactly one client connection.
type WebSocketCodec struct {
	upgraded       bool
	ip             string
	fragment       []byte
	fragmentOpcode byte
	maxMessageSize int
}

func NewWebSocketCodec() *WebSocketCodec {
	return &WebSocketCodec{maxMessageSize: maxWebSocketMessageSize}
}

func NewWebSocketCodecWithLimit(maxSize int) *WebSocketCodec {
	if maxSize <= 0 || maxSize > maxWebSocketMessageSize {
		maxSize = maxWebSocketMessageSize
	}
	return &WebSocketCodec{maxMessageSize: maxSize}
}

func (c *WebSocketCodec) Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error) {
	if !c.upgraded {
		ok, err := c.upgrade(conn)
		if err != nil || !ok {
			return nil, err
		}
	}

	var messages [][]byte
	for {
		if conn.InboundBuffered() < 2 {
			return messages, nil
		}
		data, err := conn.Peek(-1)
		if err != nil {
			return nil, err
		}
		message, complete, err := c.readFrame(conn, data)
		if err != nil {
			return nil, err
		}
		if !complete {
			return messages, nil
		}
		if message != nil {
			messages = append(messages, message)
		}
	}
}

func (c *WebSocketCodec) Encode(buf []byte) []byte {
	frame := make([]byte, 0, 10+len(buf))
	frame = append(frame, 0x82)
	switch n := len(buf); {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 0xffff:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(n))
		frame = append(frame, length[:]...)
	}
	return append(frame, buf...)
}

func (c *WebSocketCodec) GetIP() string { return c.ip }

func (c *WebSocketCodec) upgrade(conn gnet.Conn) (bool, error) {
	data, err := conn.Peek(-1)
	if err != nil {
		return false, err
	}
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		if len(data) > 16*1024 {
			return false, errors.New("websocket handshake headers too large")
		}
		return false, nil
	}
	headerLen := end + 4
	request := data[:headerLen]
	if len(request) > 16*1024 {
		return false, errors.New("websocket handshake headers too large")
	}
	lines := strings.Split(string(request[:len(request)-4]), "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "GET ") {
		return false, errors.New("invalid websocket request")
	}
	headers := make(map[string]string, len(lines)-1)
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return false, errors.New("invalid websocket header")
		}
		headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") ||
		!headerToken(headers["connection"], "upgrade") ||
		headers["sec-websocket-version"] != "13" || headers["sec-websocket-key"] == "" {
		return false, errors.New("invalid websocket upgrade request")
	}
	if ip := firstValidIP(headers["x-forwarded-for"]); ip != "" {
		c.ip = ip
	} else if ip := net.ParseIP(headers["x-real-ip"]); ip != nil {
		c.ip = ip.String()
	}
	hash := sha1.Sum([]byte(headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(hash[:]) + "\r\n\r\n"
	if _, err := conn.Discard(headerLen); err != nil {
		return false, err
	}
	conn.AsyncWrite([]byte(response), nil)
	c.upgraded = true
	return true, nil
}

func (c *WebSocketCodec) readFrame(conn gnet.Conn, data []byte) ([]byte, bool, error) {
	first, second := data[0], data[1]
	if first&0x70 != 0 {
		return nil, false, errors.New("websocket extensions are unsupported")
	}
	fin, opcode := first&0x80 != 0, first&0x0f
	if second&0x80 == 0 {
		return nil, false, errors.New("client websocket frame is not masked")
	}
	length := uint64(second & 0x7f)
	headerLen := 2
	switch length {
	case 126:
		if len(data) < 4 {
			return nil, false, nil
		}
		length, headerLen = uint64(binary.BigEndian.Uint16(data[2:4])), 4
	case 127:
		if len(data) < 10 {
			return nil, false, nil
		}
		length, headerLen = binary.BigEndian.Uint64(data[2:10]), 10
		if length&(uint64(1)<<63) != 0 {
			return nil, false, errors.New("invalid websocket frame length")
		}
	}
	maxSize := c.maxMessageSize
	if maxSize <= 0 {
		maxSize = maxWebSocketMessageSize
	}
	if length > uint64(maxSize) || length > uint64(int(^uint(0)>>1)) {
		return nil, false, fmt.Errorf("websocket message exceeds %d bytes", maxSize)
	}
	if len(data) < headerLen+4+int(length) {
		return nil, false, nil
	}
	if opcode >= 0x8 && (!fin || length > 125) {
		return nil, false, errors.New("invalid websocket control frame")
	}
	mask := data[headerLen : headerLen+4]
	payload := make([]byte, int(length))
	for i, value := range data[headerLen+4 : headerLen+4+int(length)] {
		payload[i] = value ^ mask[i&3]
	}
	if _, err := conn.Discard(headerLen + 4 + int(length)); err != nil {
		return nil, false, err
	}

	switch opcode {
	case 0x8:
		conn.AsyncWrite(append([]byte{0x88, byte(len(payload))}, payload...), nil)
		return nil, true, ErrWebSocketClose
	case 0x9:
		conn.AsyncWrite(append([]byte{0x8a, byte(len(payload))}, payload...), nil)
		return nil, true, nil
	case 0xa:
		return nil, true, nil
	case 0x0:
		if c.fragmentOpcode == 0 {
			return nil, false, errors.New("unexpected websocket continuation frame")
		}
		c.fragment = append(c.fragment, payload...)
		if len(c.fragment) > maxSize {
			return nil, false, errors.New("websocket fragmented message too large")
		}
		if !fin {
			return nil, true, nil
		}
		message, opcode := c.fragment, c.fragmentOpcode
		c.fragment, c.fragmentOpcode = nil, 0
		if opcode != 0x2 {
			return nil, false, errors.New("text websocket messages are unsupported")
		}
		return message, true, nil
	case 0x1, 0x2:
		if c.fragmentOpcode != 0 {
			return nil, false, errors.New("interleaved websocket data frames")
		}
		if fin {
			if opcode != 0x2 {
				return nil, false, errors.New("text websocket messages are unsupported")
			}
			return payload, true, nil
		}
		c.fragment, c.fragmentOpcode = payload, opcode
		return nil, true, nil
	default:
		return nil, false, errors.New("unsupported websocket opcode")
	}
}

func headerToken(value, token string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}

func firstValidIP(value string) string {
	for _, item := range strings.Split(value, ",") {
		if ip := net.ParseIP(strings.TrimSpace(item)); ip != nil {
			return ip.String()
		}
	}
	return ""
}
