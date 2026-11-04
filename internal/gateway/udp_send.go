package gateway

import (
	"encoding/binary"

	"github.com/panjf2000/gnet/v2"
)

// sendUDPFrame UDP 专用发送（跨协程安全，AsyncWrite 模式）。
// gnet v2.9.7 的 UDP 响应通过 AsyncWrite 发送，自动回复到数据报的源地址。
// 注意：remoteAddr 参数暂时保留但不使用（gnet UDP AsyncWrite 自动处理目标地址）。
func sendUDPFrame(conn gnet.Conn, remoteAddr string, data []byte) error {
	if conn == nil {
		return nil
	}

	// 封帧：4 字节长度头 + 消息体（与 TCP/WS 保持一致）
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	// UDP AsyncWrite：gnet v2.9.7 自动回复到当前数据报的源地址
	return conn.AsyncWrite(frame, nil)
}
