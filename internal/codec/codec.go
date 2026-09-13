// codec 包为网关提供可插拔的协议编解码能力。
// 采用策略模式分别处理 TCP 和 WebSocket 的线路格式。
package codec

import (
	"context"

	"github.com/panjf2000/gnet/v2"
)

const (
	// CodecTypeTCP 表示默认的长度加数据二进制编码。
	CodecTypeTCP = "tcp"
	// CodecTypeWebSocket 表示 WebSocket 编码。
	CodecTypeWebSocket = "websocket"
)

// Codec 定义协议编解码接口。
// 实现负责处理长度前缀或 WebSocket 帧等线路格式，并返回供网关处理的 protobuf 原始字节。
type Codec interface {
	// Decode 从 gnet 连接读取并返回一个或多个已解码消息；数据不足时返回 nil、nil。
	Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error)

	// Encode 将 protobuf 原始字节封装为对应线路格式，例如增加长度前缀。
	Encode(buf []byte) []byte
}

// NewCodec 根据协议类型创建对应的编解码器。
func NewCodec(protocol string) Codec {
	switch protocol {
	case CodecTypeWebSocket:
		return NewWebSocketCodec()
	default:
		return NewTCPCodec()
	}
}
