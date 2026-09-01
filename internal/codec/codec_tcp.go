package codec

import (
	"context"
	"encoding/binary"

	"github.com/panjf2000/gnet/v2"
)

const (
	TCPHeaderLen = 4 // 4-byte big-endian length prefix
)

// TCPCodec implements Length-Value binary protocol decoding.
// Wire format: [4-byte length][payload]
type TCPCodec struct{}

func NewTCPCodec() *TCPCodec {
	return &TCPCodec{}
}

// Decode reads a single Length-Value frame from the connection.
func (c *TCPCodec) Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error) {
	if conn.InboundBuffered() < TCPHeaderLen {
		return nil, nil
	}

	lenData, err := conn.Peek(TCPHeaderLen)
	if err != nil {
		return nil, err
	}

	dataLen := binary.BigEndian.Uint32(lenData)
	msgLen := int(TCPHeaderLen + dataLen)

	if conn.InboundBuffered() < msgLen {
		return nil, nil
	}

	dataWithLen, err := conn.Next(msgLen)
	if err != nil {
		return nil, err
	}

	// Copy to avoid gnet buffer reuse
	data := make([]byte, dataLen)
	copy(data, dataWithLen[TCPHeaderLen:])

	return [][]byte{data}, nil
}

// Encode wraps raw bytes with a 4-byte length prefix.
func (c *TCPCodec) Encode(buf []byte) []byte {
	data := make([]byte, TCPHeaderLen+len(buf))
	binary.BigEndian.PutUint32(data, uint32(len(buf)))
	copy(data[TCPHeaderLen:], buf)
	return data
}
