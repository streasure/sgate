package gateway

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// errUDPUnsupported 客户端可见的 UDP 拒绝原因（短句，回帧体积极小）。
const errUDPUnsupported = "udp transport not supported"

// handleUDPDatagram 处理单个 UDP 数据报（gnet UDP 不触发 OnOpen，每数据报独立）。
//
// udp.enabled=false（默认，#3 预留通道现状）：限速与帧校验后明确拒绝——只对能
// 解码出合法消息的包回一帧语义诚实的错误（垃圾包静默丢，防回包放大），并计数
// droppedUnsupported 供 /stats 观测。
//
// udp.enabled=true（关联通道，docs/udp-ws-design.md §5.2）：只放行 ASSOCIATE
// （验一次性 token 建关联）；其余包按源地址反查关联，未关联一律静默丢，已关联
// 更新活性后进入阶段 2 的上行接入点（seq 去重 → pipeline）。错误帧跟随请求通道，
// 不参与号段路由。
func (g *Gateway) handleUDPDatagram(c gnet.Conn, data []byte) gnet.Action {
	remoteAddr := c.RemoteAddr()

	// 安全三件套之一：源 IP 限速（防 DDoS）
	remoteIP := netutil.AddrHost(remoteAddr)
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

	// 仅对能解码出合法消息的包继续处理：垃圾包静默丢，控制回包放大倍数
	message, ok := routes.DecodeClientMessage(data[4:])
	if !ok || message.Cmd == 0 {
		routes.PutClientMessage(message)
		return gnet.None
	}

	if !g.udpEnabled() {
		// 预留通道现状（enabled=false 时行为与 #3 完全一致）
		routes.PutClientMessage(message)
		n := g.messagesDroppedUnsupported.Add(1)
		// 采样告警：UDP 高频场景逐包打日志会刷爆日志盘
		if n == 1 || n%1000 == 0 {
			tlog.Warn(g.ctx, "udp datagram rejected: transport does not carry business messages addr=%s total=%d", remoteAddr.String(), n)
		}
		_ = sendUDPFrame(c, remoteAddr, routes.NewErrorFrame("error", errUDPUnsupported, "", ""))
		return gnet.None
	}

	if message.Cmd == routes.CmdUdpAssociate {
		g.handleAssociate(c, remoteAddr, message)
		routes.PutClientMessage(message)
		return gnet.None
	}

	// 非 ASSOCIATE：按源地址反查关联。未关联的 UDP 包一律不处理（静默丢，
	// 不回错误帧：源地址未授权，回包既无处可去又构成放大面）。
	assoc, found := g.udpAssoc.lookupByAddr(remoteAddr.String())
	if !found {
		routes.PutClientMessage(message)
		return gnet.None
	}
	assoc.lastSeen.Store(time.Now().UnixMilli())

	// 阶段 2 接入点：seq 去重（assoc.seqBase 滑动窗口）→
	// pipelineTask{connectionID: assoc.connID, isUDP: true, udpRemoteAddr} → 现有 worker pool。
	// 阶段 1 尚未承载业务上行：已关联包更新活性后丢弃。
	routes.PutClientMessage(message)
	return gnet.None
}

// handleAssociate 处理关联请求：验一次性 token → 建关联 → 回 Ack。
// 全部失败路径静默丢（未知/过期/重放 token、会话已死、per-IP/总量超限、垃圾包）：
// 关联未建立时回错误帧无处可去，且构成放大面。
// ver 字段保留（阶段 1 不做版本协商）。
func (g *Gateway) handleAssociate(c gnet.Conn, remoteAddr net.Addr, message *protoGw.StreamData) {
	req := new(protoGw.UdpAssociateReq)
	if err := proto.Unmarshal(message.Data, req); err != nil || req.UdpToken == "" {
		return
	}
	now := time.Now().UnixMilli()
	connID, ok := g.udpAssoc.consumeToken(req.UdpToken, now)
	if !ok {
		return // 未知 / 已过期 / 重放（一次性，消费即失效）
	}
	if g.connectionManager.GetConnection(connID) == nil {
		return // 会话已死：token 作废
	}
	maxPerIP, maxAssoc := g.udpAssocLimits()
	assoc := g.udpAssoc.create(connID, remoteAddr, c, req.UdpToken, now, maxPerIP, maxAssoc)
	if assoc == nil {
		return // per-IP / 关联总量超限
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// 生成不了随机数则不建联（保守失败，客户端超时后重试）
		g.udpAssoc.deleteByConnID(connID)
		tlog.Warn(g.ctx, "udp associate nonce gen failed connID=%s error=%v", connID, err)
		return
	}
	ack := &protoGw.UdpAssociateAck{ServerNonce: nonce[:], SeqBase: assoc.seqBase, StateVer: assoc.stateVer.Load()}
	body, err := proto.Marshal(ack)
	if err != nil {
		g.udpAssoc.deleteByConnID(connID)
		return
	}
	data, err := routes.MarshalClientMessage(&protoGw.StreamData{Cmd: routes.CmdUdpAssociateAck, Data: body, SeqId: message.SeqId})
	if err != nil {
		g.udpAssoc.deleteByConnID(connID)
		return
	}
	// 回程 = 本次 ASSOCIATE 源地址，从监听 socket 发出（NAT 铁律 3）
	if err := sendUDPFrame(c, remoteAddr, data); err != nil {
		// Ack 丢失：关联保留，客户端重试 ASSOCIATE 因 token 已消费被拒，
		// 超时后重新登录获取新 token（阶段 1 语义，见设计 §5.4）
		tlog.Warn(g.ctx, "udp associate ack send failed connID=%s addr=%s error=%v", connID, remoteAddr.String(), err)
		return
	}
	tlog.Info(g.ctx, "udp associated connectionID=%s addr=%s", connID, remoteAddr.String())
}
