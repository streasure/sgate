package gateway

import (
	"encoding/binary"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
)

// errUDPUnsupported 客户端可见的 UDP 拒绝原因（短句，回帧体积极小）。
const errUDPUnsupported = "udp transport not supported"

// handleUDPDatagram 处理单个 UDP 数据报。
//
// UDP 当前定位是「预留通道」，不承载业务：gnet UDP 不触发 OnOpen（每个数据报
// 新建 conn 对象），ConnectionManager 中没有、也无法有对应连接，投进 pipeline
// 只会在阶段 2 以 unknown connection 失败 —— 一次必然失败的完整往返。因此在
// 限速与帧校验之后直接明确拒绝：只对能解码出合法消息的包回一帧语义诚实的错误
// （垃圾包静默丢，控制回包放大），并计数 droppedUnsupported 供 /stats 观测。
//
// 承载业务的规划方案是「WS 会话 + UDP 关联通道」：客户端 WS 登录后用本地 UDP
// socket 向本端口发携带一次性 token 的关联包，网关建立 connID ↔ 公网地址 关联表，
// 后续 UDP 包按源地址反查真实 connID 复用现有 pipeline 全链路（鉴权/限流/过滤器
// 一律不变），回程走 sendUDPFrame，关联生命周期挂在 WS 会话上。
// 详见 docs/architecture.md「UDP 边界与 WS+UDP 关联方案」。
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

	// 仅对能解码出合法消息的包回错误：垃圾包静默丢，控制回包放大倍数
	message, ok := routes.DecodeClientMessage(data[4:])
	if !ok || message.Cmd == 0 {
		routes.PutClientMessage(message)
		return gnet.None
	}
	routes.PutClientMessage(message)

	n := g.messagesDroppedUnsupported.Add(1)
	// 采样告警：UDP 高频场景逐包打日志会刷爆日志盘
	if n == 1 || n%1000 == 0 {
		tlog.Warn(g.ctx, "udp datagram rejected: transport does not carry business messages addr=%s total=%d", remoteAddr, n)
	}
	_ = sendUDPFrame(c, remoteAddr, routes.NewErrorFrame("error", errUDPUnsupported, "", ""))
	return gnet.None
}
