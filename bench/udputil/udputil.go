// Package udputil 是 bench/udp_*（UDP 阶段 4 压测）共用的客户端工具：
// WS 登录取一次性 udp_token → UDP ASSOCIATE 建联 → 4 字节长度头数据报收发。
//
// wire 帧一律经协议仓编解码（MarshalClientMessage / DecodeClientMessage），
// 本包只负责压测流程编排与统计口径，不重新实现帧格式（AGENTS.md：帧格式
// 权威在 github.com/streasure/protocol/gateway，改动必须走协议仓）。
package udputil

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

const (
	// MaxDatagram 单数据报上限，与网关 udp.maxDatagram 默认值一致。
	// 上行超限会被网关静默丢弃（MTU 安全值，避免 IP 分片）。
	MaxDatagram = 1400
	// ReadBufSize 单条数据报读缓冲（4 字节头 + MessageFrame + 解码余量）。
	ReadBufSize = 2048
	// udpReadBuffer 客户端 socket 收发缓冲：过小会在客户端侧丢包，
	// 与网关处理能力无关，污染对账口径。
	udpReadBuffer = 4 << 20
)

// ErrBadDatagram 数据报长度头不符或 MessageFrame 解码失败（脏包/半包）。
var ErrBadDatagram = errors.New("udputil: bad datagram")

// ---------------------------------------------------------------------------
// 数据报编解码
// ---------------------------------------------------------------------------

// Datagram 组装一条上行数据报：4 字节大端长度头 + MessageFrame。
func Datagram(cmd int32, seq int64, body []byte) ([]byte, error) {
	return AppendDatagram(nil, cmd, seq, body)
}

// AppendDatagram 复用 dst 组装数据报（压测热路径逐条分配会掩盖网关耗时）。
func AppendDatagram(dst []byte, cmd int32, seq int64, body []byte) ([]byte, error) {
	frame, err := protocol.MarshalClientMessage(&protocol.StreamData{Cmd: cmd, SeqId: seq, Data: body})
	if err != nil {
		return dst, err
	}
	n := 4 + len(frame)
	if cap(dst) < n {
		dst = make([]byte, n)
	} else {
		dst = dst[:n]
	}
	binary.BigEndian.PutUint32(dst[:4], uint32(len(frame)))
	copy(dst[4:], frame)
	return dst, nil
}

// EncodeAction 组装一条 UdpAction 上行数据报。
// seq 同时写入 MessageFrame.seq_id（网关去重判据）与 UdpAction.seq（业务序号）。
func EncodeAction(dst []byte, seq int64, payload []byte) ([]byte, error) {
	body, err := proto.Marshal(&protocol.UdpAction{Seq: seq, Payload: payload})
	if err != nil {
		return dst, err
	}
	return AppendDatagram(dst, protocol.CmdUdpAction, seq, body)
}

// EncodeState 组装一条下行 UdpState。
// snapshot 前 8 字节 = 发送时刻 UnixNano（压测时延口径，逻辑侧与客户端约定）。
func EncodeState(stateVer int64, sentNano int64) ([]byte, error) {
	snap := make([]byte, 8)
	binary.BigEndian.PutUint64(snap, uint64(sentNano))
	return proto.Marshal(&protocol.UdpState{StateVer: stateVer, Snapshot: snap})
}

// DecodeState 解析下行 UdpState，返回状态版本与内嵌发送时刻（UnixNano）。
func DecodeState(body []byte) (stateVer int64, sentNano int64, err error) {
	st := new(protocol.UdpState)
	if err := proto.Unmarshal(body, st); err != nil {
		return 0, 0, fmt.Errorf("decode udp state: %w", err)
	}
	snap := st.GetSnapshot()
	if len(snap) < 8 {
		return st.GetStateVer(), 0, fmt.Errorf("snapshot too short: %d", len(snap))
	}
	return st.GetStateVer(), int64(binary.BigEndian.Uint64(snap[:8])), nil
}

// ReadDatagram 从 UDP socket 读一条数据报并解码。
// 返回 ErrBadDatagram 表示脏包（调用方可跳过继续读），其余错误为 I/O 错误。
func ReadDatagram(udp *net.UDPConn, buf []byte) (cmd int32, seq int64, body []byte, err error) {
	n, _, err := udp.ReadFromUDP(buf)
	if err != nil {
		return 0, 0, nil, err
	}
	return decodeDatagram(buf[:n])
}

func decodeDatagram(b []byte) (int32, int64, []byte, error) {
	if len(b) < 4 {
		return 0, 0, nil, fmt.Errorf("%w: len=%d", ErrBadDatagram, len(b))
	}
	if int(binary.BigEndian.Uint32(b[:4])) != len(b)-4 {
		return 0, 0, nil, fmt.Errorf("%w: header=%d len=%d", ErrBadDatagram,
			binary.BigEndian.Uint32(b[:4]), len(b))
	}
	msg, ok := protocol.DecodeClientMessage(b[4:])
	if !ok {
		return 0, 0, nil, fmt.Errorf("%w: undecodable message frame", ErrBadDatagram)
	}
	defer protocol.PutClientMessage(msg)
	// body 需在归还池化信封前拷出（信封与 Data 缓冲会被复用）。
	out := make([]byte, len(msg.Data))
	copy(out, msg.Data)
	return msg.Cmd, msg.SeqId, out, nil
}

// ---------------------------------------------------------------------------
// 会话建立
// ---------------------------------------------------------------------------

// WSLogin 拨号 + WebSocket 升级 + LoginGate 登录，返回连接与登录 ack。
// ack 携带一次性 udp_token 与 udp_port（网关 udp.enabled=false 时二者为空，
// 调用方据此判定 UDP 通道是否开启）。token 30s 有效且一次性，失败须重新登录。
func WSLogin(addr, serverID, userID string, timeout time.Duration) (net.Conn, *protocol.LoginGateAck, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	fail := func(err error) (net.Conn, *protocol.LoginGateAck, error) {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := wsUpgrade(conn, addr); err != nil {
		return fail(fmt.Errorf("ws upgrade: %w", err))
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fail(err)
	}
	body, err := proto.Marshal(&protocol.LoginGateReq{ServerId: serverID, UserId: userID})
	if err != nil {
		return fail(err)
	}
	if err := sendWSBinary(conn, &protocol.MessageFrame{Cmd: protocol.CmdLoginGate, SeqId: 1, Body: body}); err != nil {
		return fail(fmt.Errorf("send login: %w", err))
	}
	resp, err := ReadWSFrame(conn)
	if err != nil {
		return fail(fmt.Errorf("read login ack: %w", err))
	}
	if resp.Cmd != protocol.CmdLoginGateAck {
		return fail(fmt.Errorf("unexpected login resp cmd=%d", resp.Cmd))
	}
	ack := new(protocol.LoginGateAck)
	if err := proto.Unmarshal(resp.Body, ack); err != nil {
		return fail(fmt.Errorf("decode login ack: %w", err))
	}
	if ack.Code != 0 {
		return fail(fmt.Errorf("login rejected code=%d message=%s", ack.Code, ack.GetMessage()))
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return conn, ack, nil
}

// OpenUDP 打开压测用 UDP socket（通配地址，由 OS 选源地址与端口），
// 并放大收发缓冲，避免客户端侧缓冲过小造成与网关无关的丢包。
func OpenUDP() (*net.UDPConn, error) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("listen udp4: %w", err)
	}
	_ = pc.SetReadBuffer(udpReadBuffer)
	_ = pc.SetWriteBuffer(udpReadBuffer)
	return pc, nil
}

// ResolveGW 解析网关 UDP 监听地址。
func ResolveGW(host string, port int) (*net.UDPAddr, error) {
	if port <= 0 {
		return nil, fmt.Errorf("invalid udp port %d", port)
	}
	return net.ResolveUDPAddr("udp4", net.JoinHostPort(host, strconv.Itoa(port)))
}

// Associate 用一次性 token 建立 UDP 关联并等待 ASSOCIATE Ack。
// 返回 Ack（seqBase = 上行 seq 起号，stateVer = 当前下行状态版本）与建联耗时。
// 失败语义与网关一致：token 已消费即作废，须重新登录取新 token 重试。
func Associate(udp *net.UDPConn, gw *net.UDPAddr, token string, timeout time.Duration) (*protocol.UdpAssociateAck, time.Duration, error) {
	if token == "" {
		return nil, 0, errors.New("empty udp token (gateway udp.enabled=false or no udp transport?)")
	}
	if udp == nil || gw == nil {
		return nil, 0, errors.New("udp socket / gateway addr not prepared")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, 0, fmt.Errorf("gen nonce: %w", err)
	}
	body, err := proto.Marshal(&protocol.UdpAssociateReq{UdpToken: token, Nonce: nonce[:], Ver: 1})
	if err != nil {
		return nil, 0, err
	}
	dg, err := Datagram(protocol.CmdUdpAssociate, 1, body)
	if err != nil {
		return nil, 0, err
	}
	if len(dg) > MaxDatagram {
		return nil, 0, fmt.Errorf("associate datagram %d > %d", len(dg), MaxDatagram)
	}

	start := time.Now()
	if err := udp.SetReadDeadline(start.Add(timeout)); err != nil {
		return nil, 0, err
	}
	defer func() { _ = udp.SetReadDeadline(time.Time{}) }()

	if _, err := udp.WriteToUDP(dg, gw); err != nil {
		return nil, time.Since(start), fmt.Errorf("send associate: %w", err)
	}

	buf := make([]byte, ReadBufSize)
	for {
		cmd, _, ackBody, err := ReadDatagram(udp, buf)
		if err != nil {
			if errors.Is(err, ErrBadDatagram) {
				continue // 脏包不阻塞关联等待
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil, time.Since(start), fmt.Errorf("associate ack timeout after %s", timeout)
			}
			return nil, time.Since(start), err
		}
		if cmd != protocol.CmdUdpAssociateAck {
			continue // 关联阶段只认 Ack
		}
		ack := new(protocol.UdpAssociateAck)
		if err := proto.Unmarshal(ackBody, ack); err != nil {
			return nil, time.Since(start), fmt.Errorf("decode associate ack: %w", err)
		}
		return ack, time.Since(start), nil
	}
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// Stats 网关 /stats 的对账字段（internal/dataplane/monitor.go statsPayload 子集）。
type Stats struct {
	Received             int64 `json:"received"`
	Forwarded            int64 `json:"forwarded"`
	Failed               int64 `json:"failed"`
	DroppedTotal         int64 `json:"droppedTotal"`
	DroppedRateLimit     int64 `json:"droppedRateLimit"`
	DroppedAuth          int64 `json:"droppedAuth"`
	DroppedNoLogic       int64 `json:"droppedNoLogicNotConnected"`
	DroppedUnsupported   int64 `json:"droppedUnsupported"`
	PushedToClient       int64 `json:"pushedToClient"`
	UDPUnassociatedDrops int64 `json:"udpUnassociatedDrops"`
	UDPSweptWSSurviving  int64 `json:"udpSweptWsSurviving"`
}

// FetchStats 拉取网关 /stats 快照；url 为空返回 (nil, nil)。
func FetchStats(url string, timeout time.Duration) (*Stats, error) {
	if url == "" {
		return nil, nil
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch stats http status=%d", resp.StatusCode)
	}
	s := new(Stats)
	if err := json.NewDecoder(resp.Body).Decode(s); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}
	return s, nil
}

// LogicCounters logic 侧计数器快照（bench/logic_udp 的 /counters 端点）。
type LogicCounters struct {
	Logins            int64 `json:"logins"`
	UdpActionReceived int64 `json:"udpActionReceived"`
	Pushed            int64 `json:"pushed"`
	PushFailed        int64 `json:"pushFailed"`
	StateVer          int64 `json:"stateVer"`
	Members           int64 `json:"members"`
}

// FetchLogicCounters 拉取 logic 侧计数器；url 为空返回 (nil, nil)。
func FetchLogicCounters(url string, timeout time.Duration) (*LogicCounters, error) {
	if url == "" {
		return nil, nil
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch logic counters: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch logic counters http status=%d", resp.StatusCode)
	}
	c := new(LogicCounters)
	if err := json.NewDecoder(resp.Body).Decode(c); err != nil {
		return nil, fmt.Errorf("decode logic counters: %w", err)
	}
	return c, nil
}

// Delta 返回 b - a（快照取样点 a 在前、b 在后）。
func Delta(a, b *Stats) *Stats {
	if a == nil || b == nil {
		return nil
	}
	return &Stats{
		Received:             b.Received - a.Received,
		Forwarded:            b.Forwarded - a.Forwarded,
		Failed:               b.Failed - a.Failed,
		DroppedTotal:         b.DroppedTotal - a.DroppedTotal,
		DroppedRateLimit:     b.DroppedRateLimit - a.DroppedRateLimit,
		DroppedAuth:          b.DroppedAuth - a.DroppedAuth,
		DroppedNoLogic:       b.DroppedNoLogic - a.DroppedNoLogic,
		DroppedUnsupported:   b.DroppedUnsupported - a.DroppedUnsupported,
		PushedToClient:       b.PushedToClient - a.PushedToClient,
		UDPUnassociatedDrops: b.UDPUnassociatedDrops - a.UDPUnassociatedDrops,
		UDPSweptWSSurviving:  b.UDPSweptWSSurviving - a.UDPSweptWSSurviving,
	}
}

// Percentile 最近秩法百分位，输入必须升序；p ∈ [0,1]，空切片返回 0。
func Percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Micros 纳秒 → 微秒浮点（日志展示口径，P50 常 <1µs）。
func Micros(ns int64) float64 { return float64(ns) / 1e3 }

// AssocStats 关联压测汇总：QPS = 成功建联数 / 并发爬坡窗口（窗口为 0 时
// 无法度量，如实报 0），外加样本的最小 / 平均耗时（ns，输入须升序）。
func AssocStats(sorted []int64, windowSec float64) (qps float64, minNs int64, avgNs int64) {
	if len(sorted) == 0 {
		return 0, 0, 0
	}
	if windowSec > 0 {
		qps = float64(len(sorted)) / windowSec
	}
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return qps, sorted[0], sum / int64(len(sorted))
}

// ---------------------------------------------------------------------------
// WebSocket 握手与帧（与 bench1_ws/bench2_ws 同口径）
// ---------------------------------------------------------------------------

func wsUpgrade(conn net.Conn, host string) error {
	key := base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(mrand.Uint64(), 16)))
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	respBuf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for {
		n, err := conn.Read(tmp)
		if err != nil {
			return err
		}
		respBuf = append(respBuf, tmp[:n]...)
		if len(respBuf) >= 4 && string(respBuf[len(respBuf)-4:]) == "\r\n\r\n" {
			break
		}
		if len(respBuf) > 16*1024 {
			return fmt.Errorf("websocket handshake response too large")
		}
	}
	if !strings.HasPrefix(string(respBuf), "HTTP/1.1 101") {
		return fmt.Errorf("upgrade failed: %s", strings.Split(string(respBuf), "\r\n")[0])
	}
	return nil
}

func sendWSBinary(conn net.Conn, frame *protocol.MessageFrame) error {
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	mask := [4]byte{byte(mrand.IntN(256)), byte(mrand.IntN(256)), byte(mrand.IntN(256)), byte(mrand.IntN(256))}
	var wsFrame []byte
	n := len(data)
	if n < 126 {
		wsFrame = make([]byte, 0, 2+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|byte(n))
	} else if n <= 65535 {
		wsFrame = make([]byte, 0, 4+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|126, byte(n>>8), byte(n))
	} else {
		wsFrame = make([]byte, 0, 10+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|127)
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(n))
		wsFrame = append(wsFrame, lenBuf[:]...)
	}
	wsFrame = append(wsFrame, mask[:]...)
	masked := make([]byte, n)
	copy(masked, data)
	for i := range masked {
		masked[i] ^= mask[i%4]
	}
	wsFrame = append(wsFrame, masked...)
	_, err = conn.Write(wsFrame)
	return err
}

// ReadWSFrame 读一帧服务端下行（未掩码），返回 MessageFrame。
func ReadWSFrame(conn net.Conn) (*protocol.MessageFrame, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(conn, ext); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(conn, ext); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}
	if length == 0 || length > 4*1024*1024 {
		return nil, fmt.Errorf("invalid ws frame length=%d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	frame := new(protocol.MessageFrame)
	if err := proto.Unmarshal(payload, frame); err != nil {
		return nil, fmt.Errorf("decode ws frame: %w", err)
	}
	return frame, nil
}
