package gateway

import (
	"encoding/binary"
	"net"

	"github.com/panjf2000/gnet/v2"
)

// sendUDPFrame UDP 下行唯一出口：4 字节长度头 + 消息体（与 TCP/WS 帧一致），
// 经监听 socket 主动发往 targetAddr（NAT 铁律：回程必须从监听 socket 发出，
// ASSOCIATE 时捕获的 conn 其 fd/pc 属于监听 socket，release 不关闭）。
//
// 所有 UDP 下行必须收敛到本函数：gnet 文档标注 SendTo "not concurrency-safe"，
// 但 unix/windows 两平台实现均为单次系统调用（sendto / PacketConn.WriteTo）、
// 不读写 conn 内部状态；收敛单点即可保证行为可推理，勿新增其他发送路径。
// 未关联/目标失效时 SendTo 返回错误，调用方按"路断回落/丢弃"处理。
func sendUDPFrame(conn gnet.Conn, targetAddr net.Addr, data []byte) error {
	if conn == nil || targetAddr == nil {
		return nil
	}

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	_, err := conn.SendTo(frame, targetAddr)
	return err
}
