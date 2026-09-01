// Package codec provides pluggable protocol encoding/decoding for the gateway.
// Inspired by gateserver's codec layer: Strategy Pattern for TCP/WebSocket.
package codec

import (
	"context"

	"github.com/panjf2000/gnet/v2"
)

const (
	// CodecTypeTCP is the default Length-Value binary codec.
	CodecTypeTCP = "tcp"
	// CodecTypeWebSocket is the WebSocket codec.
	CodecTypeWebSocket = "websocket"
)

// Codec defines the interface for protocol encoding/decoding.
// Implementations handle the wire format (length-prefix, WebSocket frames, etc.)
// and return raw protobuf bytes for the gateway to process.
type Codec interface {
	// Decode reads from the gnet connection and returns one or more decoded messages.
	// Returns nil, nil if not enough data is available yet.
	Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error)

	// Encode wraps raw protobuf bytes in the wire format (length prefix, etc.)
	Encode(buf []byte) []byte
}

// NewCodec creates a Codec based on the protocol type.
func NewCodec(protocol string) Codec {
	switch protocol {
	case CodecTypeWebSocket:
		return NewWebSocketCodec()
	default:
		return NewTCPCodec()
	}
}

// IPGetter is an optional capability interface for codecs that can extract
// the real client IP (e.g., from WebSocket headers like X-Forwarded-For).
type IPGetter interface {
	GetIP() string
}
