package gateway

import (
	"encoding/binary"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
)

// handleUDPDatagram 处理单个 UDP 数据报（无连接状态）。
// UDP 规则：
// - 简化版：所有数据报直接投递 pipeline，无登录校验（登录校验在 pipeline 内完成）
// - 每个数据报 = 完整帧（4 字节长度头 + 消息体）
// - 安检不豁免（全走 pipeline）
// - UDP 无持久会话，每个数据报独立处理
func (g *Gateway) handleUDPDatagram(c gnet.Conn, data []byte) gnet.Action {
	remoteAddr := c.RemoteAddr().String()

	// 安全三件套之一：源 IP 限速（防 DDoS）
	remoteIP := netutil.AddrHost(c.RemoteAddr())
	if g.rateLimiter != nil {
		if !g.rateLimiter.AllowGlobal() || !g.rateLimiter.Allow("ip", remoteIP) {
			return gnet.None // 静默丢弃
		}
	}

	// 帧格式校验：最小 4 字节长度头
	if len(data) < 4 {
		return gnet.None
	}

	frameLen := binary.BigEndian.Uint32(data[:4])
	if int(frameLen) != len(data)-4 {
		return gnet.None
	}

	maxFrame := g.getProtection().MaxFrameSize
	if frameLen == 0 || frameLen > uint32(maxFrame) {
		return gnet.None
	}

	frame := data[4:]

	// 解析消息头
	message, ok := routes.DecodeClientMessage(frame)
	if !ok {
		tlog.Debug(g.ctx, "udp decode failed addr=%s", remoteAddr)
		return gnet.None
	}
	if message.Cmd == 0 {
		return gnet.None
	}

	// UDP 简化处理：直接投递到 pipeline（无 connectionID，pipeline 内部判断 UDP 场景）
	// 用 remoteAddr 作为虚拟 connectionID（UDP 无状态会话）
	dataCopy := getMsgBuf(len(frame))
	copy(dataCopy, frame)

	task := pipelineTaskPool.Get().(*pipelineTaskData)
	*task = pipelineTaskData{
		conn:          c,
		data:          dataCopy,
		message:       message,
		connectionID:  "udp_" + remoteAddr, // UDP 虚拟 connectionID
		wsConn:        nil,
		isUDP:         true,
		udpRemoteAddr: remoteAddr,
	}

	if g.pipelineWorkerPool == nil {
		// 同步路径（测试环境）：暂不支持 UDP
		tlog.Warn(g.ctx, "udp sync path not supported")
		routes.PutClientMessage(task.message)
		putMsgBuf(task.data)
		pipelineTaskPool.Put(task)
		return gnet.None
	}

	if !g.pipelineWorkerPool.Submit(task) {
		routes.PutClientMessage(task.message)
		putMsgBuf(task.data)
		pipelineTaskPool.Put(task)
		return gnet.None
	}

	return gnet.None
}
