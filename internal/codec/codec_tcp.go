package codec

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/panjf2000/gnet/v2"
)

// 缓冲区对象池，用于降低垃圾回收压力。
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
	TCPHeaderLen = 4 // 4 字节大端长度前缀。
)

// TCPCodec 实现长度加数据格式的二进制协议编解码。
// 线路格式为：[4 字节长度][载荷]。
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

// Decode 从连接中读取并解码一个长度加数据格式的帧。
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
		// 小消息使用对象池，大消息直接分配，避免污染对象池。
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

// Encode 为原始字节增加 4 字节长度前缀。
func (c *TCPCodec) Encode(buf []byte) []byte {
	totalLen := TCPHeaderLen + len(buf)
	// 小消息使用对象池。
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
