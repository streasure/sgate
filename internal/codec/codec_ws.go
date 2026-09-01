package codec

import (
	"context"

	"github.com/panjf2000/gnet/v2"
)

// WebSocketCodec handles WebSocket protocol encoding/decoding.
// Full implementation requires a WebSocket library (e.g., gobwas/ws).
// This is a stub — the actual WebSocket handling is in gateway/websocket.go.
type WebSocketCodec struct{}

func NewWebSocketCodec() *WebSocketCodec {
	return &WebSocketCodec{}
}

func (c *WebSocketCodec) Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error) {
	// TODO: Implement WebSocket frame parsing
	return nil, nil
}

func (c *WebSocketCodec) Encode(buf []byte) []byte {
	// TODO: Implement WebSocket frame encoding
	return buf
}
