package gateway

import (
	"encoding/binary"
	"errors"
	"net"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// errNoUDPAssoc SendUDP 失败原因（调用方 connection.SendByCmd 据此回落 WS）。
var errNoUDPAssoc = errors.New("no udp association")

// SendUDP 实现 connection.UDPDownlink：把一条下行消息发到会话关联的客户端地址。
// 按会话反查关联 → 新鲜度条件（§8.5）→ sendUDPFrame。
//
// 新鲜度条件：now - lastSeen ≤ idleTimeout/2 才走 UDP。关联仍在表里但已 stale
// （NAT 重绑/客户端 UDP 静默）时 SendTo 会成功投递到死地址——错误回不了头，
// 形成静默黑洞；直接返回错误让 SendByCmd 回落 WS，把黑洞窗口从 idleTimeout
// 砍半到 idleTimeout/2（2 次保活丢失即回落），阈值复用同一配置零新增。
//
// 返回错误一律表示"这条路不能用"，SendByCmd 回落 WS，消息不丢。
func (g *Gateway) SendUDP(sessionID string, frame []byte) error {
	assoc, found := g.udpAssoc.lookupByConnID(sessionID)
	if !found || assoc.udpConn == nil || assoc.publicAddr == nil {
		return errNoUDPAssoc
	}
	if idle := g.udpIdleDuration(); time.Now().UnixMilli()-assoc.lastSeen.Load() > idle.Milliseconds()/2 {
		return errNoUDPAssoc // stale：不尝试 SendTo（系统调用会成功，错误回不了头）
	}
	return sendUDPFrame(assoc.udpConn, assoc.publicAddr, frame)
}

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
