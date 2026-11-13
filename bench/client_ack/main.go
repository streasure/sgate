// client_ack 可靠推送端到端测试的客户端侧（roadmap #4 阶段 4）。
//
// 与 bench2 的区别：对下行带 SeqId 的可靠消息回 CmdMsgAck（批量确认），
// 并维护去重窗口——重传产生的重复消息只计数不重复"执行"（重复投递
// 不重复执行的客户端侧验证）。故障注入 flag 本机 TCP 丢不了包，靠客户端
// 行为模拟丢失：
//
//	-drop-ack N   每 N 条可靠消息故意丢弃首发 ACK，等服务端重传到达再补
//	              （验证重传 + 去重 + 迟到 ACK 幂等）
//	-no-ack       完全不 ACK（验证窗口满背压 / 重传达上限放弃 giveUps）
//	-ack-interval ACK 批量合并周期（调大即模拟 ACK 延迟，超过退避触发重传）
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/bench/logutil"

	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

const (
	cmdLoginGate    = protocol.CmdLoginGate
	cmdLoginGateAck = protocol.CmdLoginGateAck
	cmdPush         = 9000001
)

// sessionAck 单连接的 ACK 收集器与去重窗口。
// seen 记录全部已见 seq（本 bench 为功能测试、时长有限，不裁剪；
// 生产客户端应按水位线滑动裁剪）。
type sessionAck struct {
	conn      net.Conn
	noAck     bool
	dropEvery int // drop-ack 注入周期（0=关闭）

	mu        sync.Mutex
	seen      map[int64]struct{} // 去重窗口：已见 seq
	pending   []int64            // 待 ACK 的首见 seq
	withheld  map[int64]struct{} // drop-ack 注入：首发已收但故意不 ACK，等重传补
	uplinkSeq atomic.Int64       // ACK 帧自身上行序号（verifyInbound 要求非 0）

	unique        atomic.Int64 // 去重后的有效消息数
	dup           atomic.Int64 // 重复（重传）消息数
	withheldCount atomic.Int64 // 注入丢弃的首发 ACK 数
	caughtByRetry atomic.Int64 // 重传到达后补 ACK 的数
	ackFrames     atomic.Int64 // 发出的 ACK 帧数
	ackedSeqs     atomic.Int64 // ACK 帧里确认的 seq 总数
}

func newSessionAck(conn net.Conn, noAck bool) *sessionAck {
	return &sessionAck{
		conn:     conn,
		noAck:    noAck,
		seen:     make(map[int64]struct{}, 1024),
		withheld: make(map[int64]struct{}),
	}
}

// observe 处理一条可靠消息的 seq：去重 + 首见入待 ACK 集（或按注入策略丢弃）。
func (s *sessionAck) observe(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[seq]; ok {
		// 重复（重传）：不重复计数；若首发 ACK 被注入丢弃，此刻补 ACK。
		s.dup.Add(1)
		if _, w := s.withheld[seq]; w {
			delete(s.withheld, seq)
			s.pending = append(s.pending, seq)
			s.caughtByRetry.Add(1)
		}
		return
	}
	s.seen[seq] = struct{}{}
	s.unique.Add(1)
	// drop-ack 注入：每 N 条丢一次首发 ACK（N<=0 关闭）。
	if s.dropEvery > 0 && s.unique.Load()%int64(s.dropEvery) == 0 {
		s.withheld[seq] = struct{}{}
		s.withheldCount.Add(1)
		return
	}
	s.pending = append(s.pending, seq)
}

// flush 发送一帧批量 ACK（pending 非空时）。noAck 模式永不发送。
func (s *sessionAck) flush() {
	if s.noAck {
		return
	}
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()

	body, err := proto.Marshal(&protoLogic.MsgAck{SeqIds: batch})
	if err != nil {
		tlog.Warn(context.TODO(), "client_ack marshal MsgAck failed error=%v", err)
		return
	}
	frame := &protocol.MessageFrame{
		Cmd:   protocol.CmdMsgAck,
		SeqId: s.uplinkSeq.Add(1),
		Body:  body,
	}
	if err := sendWSBinary(s.conn, frame); err != nil {
		tlog.Warn(context.TODO(), "client_ack send ack failed count=%d error=%v", len(batch), err)
		return
	}
	s.ackFrames.Add(1)
	s.ackedSeqs.Add(int64(len(batch)))
}

func main() {
	addr := flag.String("addr", "127.0.0.1:48081", "sgate WebSocket address")
	duration := flag.Duration("duration", 30*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 1, "number of parallel connections")
	serverID := flag.String("server-id", "logic-rel", "logic server ID for login")
	logConfig := flag.String("config", "config/log/bench.yaml", "log config")
	loginBatch := flag.Int("login-batch", 100, "concurrent login batch size")
	ackInterval := flag.Duration("ack-interval", 100*time.Millisecond, "ACK batch flush interval (调大模拟 ACK 延迟)")
	ackBatch := flag.Int("ack-batch", 64, "flush ACK when pending reaches this many seqs")
	dropAck := flag.Int("drop-ack", 0, "每 N 条可靠消息丢弃首发 ACK（0=关闭），等重传补 ACK")
	noAck := flag.Bool("no-ack", false, "完全不 ACK（验证窗口满背压 / giveUps）")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	tlog.Info(context.TODO(), "client_ack started addr=%s duration=%s parallel=%d serverID=%s ackInterval=%s dropAck=%d noAck=%v",
		*addr, duration.String(), *parallel, *serverID, ackInterval.String(), *dropAck, *noAck)

	var totalRecv atomic.Int64
	var connectionsFailed atomic.Int64
	var measureStart atomic.Int64

	type connEntry struct {
		conn net.Conn
		idx  int
	}
	var conns []*connEntry
	var sessions []*sessionAck
	var connMu sync.Mutex

	batchSize := min(*loginBatch, *parallel)
	for batch := 0; batch < *parallel; batch += batchSize {
		end := min(batch+batchSize, *parallel)
		var batchReady sync.WaitGroup
		batchReady.Add(end - batch)
		for i := batch; i < end; i++ {
			idx := i
			go func() {
				defer batchReady.Done()
				c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err != nil {
					connectionsFailed.Add(1)
					return
				}
				if err := wsUpgrade(c, *addr); err != nil {
					c.Close()
					connectionsFailed.Add(1)
					return
				}
				userID := fmt.Sprintf("ack_user_%d", idx)
				loginReq := &protocol.LoginGateReq{ServerId: *serverID, UserId: userID}
				loginBody, _ := proto.Marshal(loginReq)
				if err := sendWSBinary(c, &protocol.MessageFrame{Cmd: cmdLoginGate, SeqId: 1, Body: loginBody}); err != nil {
					c.Close()
					connectionsFailed.Add(1)
					return
				}
				resp, err := readWSBinary(c)
				if err != nil || resp.Cmd != cmdLoginGateAck {
					c.Close()
					connectionsFailed.Add(1)
					return
				}
				ack := new(protocol.LoginGateAck)
				if err := proto.Unmarshal(resp.Body, ack); err != nil || ack.Code != 0 {
					tlog.Warn(context.TODO(), "client_ack login rejected client=%d code=%d message=%s", idx, ack.GetCode(), ack.GetMessage())
					c.Close()
					connectionsFailed.Add(1)
					return
				}
				sa := newSessionAck(c, *noAck)
				sa.dropEvery = *dropAck
				connMu.Lock()
				conns = append(conns, &connEntry{conn: c, idx: idx})
				sessions = append(sessions, sa)
				connMu.Unlock()
			}()
		}
		batchReady.Wait()
	}

	connected := len(conns)
	tlog.Info(context.TODO(), "client_ack all logins done connected=%d failed=%d", connected, connectionsFailed.Load())
	if connected == 0 {
		tlog.Info(context.TODO(), "client_ack completed received=0 connectionsFailed=%d", connectionsFailed.Load())
		return
	}

	deadline := time.Now().Add(*duration + 30*time.Second)
	for _, e := range conns {
		e.conn.SetReadDeadline(deadline)
	}

	measureStart.Store(time.Now().UnixNano())

	var recvWG sync.WaitGroup
	recvWG.Add(connected)
	for _, sa := range sessions {
		go func(sa *sessionAck) {
			defer recvWG.Done()
			go ackFlushLoop(sa, *ackInterval, *ackBatch)
			readLoop(sa, &totalRecv)
		}(sa)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(time.Unix(0, measureStart.Load())).Seconds()
				tlog.Info(context.TODO(), "client_ack progress elapsed=%.1f received=%d unique=%d dup=%d withheld=%d caughtByRetry=%d ackFrames=%d",
					elapsed, totalRecv.Load(), sumAtomic(sessions, func(s *sessionAck) int64 { return s.unique.Load() }),
					sumAtomic(sessions, func(s *sessionAck) int64 { return s.dup.Load() }),
					sumAtomic(sessions, func(s *sessionAck) int64 { return s.withheldCount.Load() }),
					sumAtomic(sessions, func(s *sessionAck) int64 { return s.caughtByRetry.Load() }),
					sumAtomic(sessions, func(s *sessionAck) int64 { return s.ackFrames.Load() }))
			case <-done:
				return
			}
		}
	}()

	time.Sleep(*duration)
	close(done)

	for _, e := range conns {
		e.conn.Close()
	}
	recvWG.Wait()

	elapsed := time.Since(time.Unix(0, measureStart.Load())).Seconds()
	tlog.Info(context.TODO(), "client_ack completed received=%d unique=%d dup=%d withheld=%d caughtByRetry=%d ackFrames=%d ackedSeqs=%d connectionsFailed=%d elapsed=%.1f",
		totalRecv.Load(),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.unique.Load() }),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.dup.Load() }),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.withheldCount.Load() }),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.caughtByRetry.Load() }),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.ackFrames.Load() }),
		sumAtomic(sessions, func(s *sessionAck) int64 { return s.ackedSeqs.Load() }),
		connectionsFailed.Load(), elapsed)
}

// readLoop 读取一帧并展开统计：PushBatch 按 items 展开，可靠消息
// （SeqId != 0）交 ACK 收集器去重/确认。
func readLoop(sa *sessionAck, totalRecv *atomic.Int64) {
	header := make([]byte, 2)
	ext := make([]byte, 8)
	var payload []byte
	frame := &protocol.MessageFrame{}
	for {
		if _, err := readFull(sa.conn, header); err != nil {
			return
		}
		length := uint64(header[1] & 0x7F)
		switch length {
		case 126:
			if _, err := readFull(sa.conn, ext[:2]); err != nil {
				return
			}
			length = uint64(binary.BigEndian.Uint16(ext[:2]))
		case 127:
			if _, err := readFull(sa.conn, ext); err != nil {
				return
			}
			length = binary.BigEndian.Uint64(ext)
		}
		if length == 0 || length > 4*1024*1024 {
			return
		}
		if length > uint64(cap(payload)) {
			payload = make([]byte, length)
		}
		buf := payload[:length]
		if _, err := readFull(sa.conn, buf); err != nil {
			return
		}
		if err := proto.Unmarshal(buf, frame); err != nil {
			continue
		}
		if frame.Cmd == protocol.CmdPushBatch {
			pb := &protocol.PushBatch{}
			if perr := proto.Unmarshal(frame.Body, pb); perr != nil {
				continue
			}
			for _, item := range pb.GetItems() {
				totalRecv.Add(1)
				if item.GetSeqId() != 0 {
					sa.observe(item.GetSeqId())
				}
			}
			continue
		}
		totalRecv.Add(1)
		if frame.SeqId != 0 && frame.Cmd == cmdPush {
			sa.observe(frame.SeqId)
		}
	}
}

// ackFlushLoop 周期批量 ACK；攒满 ackBatch 立即发一帧。
func ackFlushLoop(sa *sessionAck, interval time.Duration, batchSize int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		sa.mu.Lock()
		n := len(sa.pending)
		sa.mu.Unlock()
		if n >= batchSize {
			// 攒满：连续发到清空（flush 每次取走当前 pending）。
			for {
				sa.flush()
				sa.mu.Lock()
				rest := len(sa.pending)
				sa.mu.Unlock()
				if rest == 0 {
					break
				}
			}
			continue
		}
		sa.flush()
	}
}

func sumAtomic(sessions []*sessionAck, f func(*sessionAck) int64) int64 {
	var total int64
	for _, s := range sessions {
		total += f(s)
	}
	return total
}

func wsUpgrade(conn net.Conn, host string) error {
	key := base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(rand.Uint64(), 16)))
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
	mask := [4]byte{byte(rand.IntN(256)), byte(rand.IntN(256)), byte(rand.IntN(256)), byte(rand.IntN(256))}
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

func readWSBinary(conn net.Conn) (*protocol.MessageFrame, error) {
	header := make([]byte, 2)
	if _, err := readFull(conn, header); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := readFull(conn, ext); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := readFull(conn, ext); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}
	payload := make([]byte, length)
	if _, err := readFull(conn, payload); err != nil {
		return nil, err
	}
	frame := &protocol.MessageFrame{}
	if err := proto.Unmarshal(payload, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	return io.ReadFull(conn, buf)
}
