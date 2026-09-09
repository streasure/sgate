package codec

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/panjf2000/gnet/v2"
)

// Buffer pools for reducing GC pressure
var (
	decodeBufPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 0, 64*1024) // 64KB
			return &buf
		},
	}
	encodeBufPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 0, 64*1024) // 64KB
			return &buf
		},
	}
)

const (
	TCPHeaderLen = 4 // 4-byte big-endian length prefix
)

// TCPCodec implements Length-Value binary protocol decoding.
// Wire format: [4-byte length][payload]
type TCPCodec struct{ maxMessageSize int }

func NewTCPCodec() *TCPCodec {
	return NewTCPCodecWithLimit(4 * 1024 * 1024)
}

func NewTCPCodecWithLimit(maxSize int) *TCPCodec {
	if maxSize <= 0 {
		maxSize = 4 * 1024 * 1024
	}
	return &TCPCodec{maxMessageSize: maxSize}
}

// Decode reads a single Length-Value frame from the connection.
func (c *TCPCodec) Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error) {
	var messages [][]byte
	for conn.InboundBuffered() >= TCPHeaderLen {
		lenData, err := conn.Peek(TCPHeaderLen)
		if err != nil {
			return nil, err
		}
		dataLen := binary.BigEndian.Uint32(lenData)
		if dataLen == 0 || uint64(dataLen) > uint64(c.maxMessageSize) {
			return nil, fmt.Errorf("TCP message size %d is outside 1..%d", dataLen, c.maxMessageSize)
		}
		msgLen := int(TCPHeaderLen + dataLen)
		if conn.InboundBuffered() < msgLen {
			break
		}
		dataWithLen, err := conn.Next(msgLen)
		if err != nil {
			return nil, err
		}
		// Use pool for small messages, direct allocation for large ones
		if dataLen <= 64*1024 {
			bufPtr := decodeBufPool.Get().(*[]byte)
			buf := (*bufPtr)[:dataLen]
			copy(buf, dataWithLen[TCPHeaderLen:])
			messages = append(messages, buf)
		} else {
			data := make([]byte, dataLen)
			copy(data, dataWithLen[TCPHeaderLen:])
			messages = append(messages, data)
		}
	}
	return messages, nil
}

// Encode wraps raw bytes with a 4-byte length prefix.
func (c *TCPCodec) Encode(buf []byte) []byte {
	totalLen := TCPHeaderLen + len(buf)
	// Use pool for small messages
	if totalLen <= 64*1024 {
		bufPtr := encodeBufPool.Get().(*[]byte)
		data := (*bufPtr)[:totalLen]
		binary.BigEndian.PutUint32(data, uint32(len(buf)))
		copy(data[TCPHeaderLen:], buf)
		return data
	}
	data := make([]byte, totalLen)
	binary.BigEndian.PutUint32(data, uint32(len(buf)))
	copy(data[TCPHeaderLen:], buf)
	return data
}
