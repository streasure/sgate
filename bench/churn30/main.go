// Package main churn30 混合负载长压测：
//   - 常驻已登录用户不定时随机突发大量协议
//   - 每隔 wave-interval 一批新用户连接登录，登录后立即大量发协议，
//     存活 wave-life 后断开（制造连接churn）
//
// 结果写入 -out JSONL 文件（后台进程 stdout 不可回收）。
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

const (
	cmdLoginGate    int32 = 1000001
	cmdLoginGateAck int32 = 1000002
	cmdHeartbeatReq int32 = 1100010
)

type session struct {
	conn    net.Conn
	id      string
	stop    chan struct{}
	closed  atomic.Bool
	writeMu sync.Mutex
}

func (s *session) sendFrame(frame *protocol.MessageFrame) error {
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.conn.Write(append(header, data...))
	return err
}

func readTCPFrame(conn net.Conn) (*protocol.MessageFrame, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	dataLen := binary.BigEndian.Uint32(header)
	if dataLen == 0 || dataLen > 4*1024*1024 {
		return nil, fmt.Errorf("invalid frame length %d", dataLen)
	}
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	frame := &protocol.MessageFrame{}
	if err := proto.Unmarshal(data, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// login 依次拨号+LoginGate 登录，成功返回 session。
func login(addr, serverID, userID string) (*session, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	loginReq := &protocol.LoginGateReq{ServerId: serverID, UserId: userID, LoginKey: ""}
	body, _ := proto.Marshal(loginReq)
	data, _ := proto.Marshal(&protocol.MessageFrame{Cmd: cmdLoginGate, SeqId: 1, Body: body})
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(header, data...)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send login: %w", err)
	}
	resp, err := readTCPFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read ack: %w", err)
	}
	if resp.Cmd != cmdLoginGateAck {
		conn.Close()
		return nil, fmt.Errorf("unexpected cmd %d", resp.Cmd)
	}
	ack := new(protocol.LoginGateAck)
	if err := proto.Unmarshal(resp.Body, ack); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decode ack: %w", err)
	}
	if ack.Code != 0 {
		conn.Close()
		return nil, fmt.Errorf("rejected code=%d msg=%s", ack.Code, ack.Message)
	}
	conn.SetDeadline(time.Time{})
	return &session{conn: conn, id: userID, stop: make(chan struct{})}, nil
}

type counters struct {
	sent       atomic.Int64
	recv       atomic.Int64
	loginOK    atomic.Int64
	loginFail  atomic.Int64
	writeErr   atomic.Int64
	readClosed atomic.Int64
	wavesDone  atomic.Int64
	churned    atomic.Int64
}

type report struct {
	Ts          time.Time `json:"ts"`
	ElapsedSec  float64   `json:"elapsed_s"`
	SentDelta   int64     `json:"sent_delta"`
	Rate        float64   `json:"rate"`
	SentTotal   int64     `json:"sent_total"`
	RecvTotal   int64     `json:"recv_total"`
	ActiveConns int       `json:"active_conns"`
	LoginOK     int64     `json:"login_ok"`
	LoginFail   int64     `json:"login_fail"`
	WavesDone   int64     `json:"waves_done"`
	Churned     int64     `json:"churned"`
	WriteErr    int64     `json:"write_err"`
	ReadClosed  int64     `json:"read_closed"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:48080", "sgate TCP address")
	duration := flag.Duration("duration", 30*time.Minute, "total duration")
	base := flag.Int("base", 150, "persistent logged-in users")
	wave := flag.Int("wave", 50, "new users per wave")
	waveInterval := flag.Duration("wave-interval", time.Minute, "interval between login waves")
	waveLife := flag.Duration("wave-life", 5*time.Minute, "how long wave conns stay before churn-close")
	serverID := flag.String("server-id", "logic1-tcp", "logic server instance ID")
	out := flag.String("out", "logs/churn30_result.jsonl", "JSONL result file")
	flag.Parse()

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create out file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	var c counters
	var active atomic.Int64
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	start := time.Now()
	deadline := start.Add(*duration)
	stopAll := make(chan struct{})

	// 启动会话：读协程 + 突发发送协程。
	startSession := func(s *session, initialBurst bool) {
		active.Add(1)
		// reader：drain 响应；读错误（服务端踢/EOF）关闭会话。
		go func() {
			for {
				s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
				_, err := readTCPFrame(s.conn)
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						continue // 无响应正常，继续等
					}
					// 仅服务端主动断开计数；本端主动关闭时 closed 已为 true。
					if s.closed.CompareAndSwap(false, true) {
						close(s.stop)
						s.conn.Close()
						c.readClosed.Add(1)
					}
					return
				}
				c.recv.Add(1)
			}
		}()
		// sender：随机突发
		go func() {
			seq := int64(2)
			sendBatch := func(n int) bool {
				body := []byte("bench")
				for range n {
					select {
					case <-s.stop:
						return false
					case <-stopAll:
						return false
					default:
					}
					err := s.sendFrame(&protocol.MessageFrame{Cmd: cmdHeartbeatReq, SeqId: seq, Body: body})
					if err != nil {
						// 仅本端首次失败计数（服务端踢人经由写失败暴露）；主动关闭不算。
						if s.closed.CompareAndSwap(false, true) {
							close(s.stop)
							s.conn.Close()
							c.writeErr.Add(1)
						}
						return false
					}
					c.sent.Add(1)
					seq++
				}
				return true
			}
			if initialBurst {
				// 新用户登录后立即大量发协议
				if !sendBatch(100 + rng.Intn(200)) {
					active.Add(-1)
					return
				}
			}
			for {
				select {
				case <-s.stop:
					active.Add(-1)
					return
				case <-stopAll:
					active.Add(-1)
					return
				case <-time.After(time.Duration(200+rng.Intn(1800)) * time.Millisecond):
				}
				n := 10 + rng.Intn(70)
				if rng.Intn(10) == 0 {
					n = 200 + rng.Intn(300) // 10% 概率大突发
				}
				if !sendBatch(n) {
					active.Add(-1)
					return
				}
			}
		}()
	}

	// 阶段1：常驻用户登录
	fmt.Printf("churn30: addr=%s duration=%s base=%d wave=%d waveInterval=%s waveLife=%s\n",
		*addr, *duration, *base, *wave, *waveInterval, *waveLife)
	baseSessions := make([]*session, 0, *base)
	for i := range *base {
		s, err := login(*addr, *serverID, fmt.Sprintf("churn_base_%d", i))
		if err != nil {
			c.loginFail.Add(1)
			fmt.Fprintf(os.Stderr, "base login %d: %v\n", i, err)
			continue
		}
		c.loginOK.Add(1)
		baseSessions = append(baseSessions, s)
		startSession(s, true)
		if i%20 == 19 {
			time.Sleep(100 * time.Millisecond) // 避免登录风暴
		}
	}
	fmt.Printf("base logged in: %d (fail %d)\n", len(baseSessions), c.loginFail.Load())

	// 波次管理
	type waveConn struct {
		s      *session
		bornAt time.Time
	}
	var mu sync.Mutex
	waves := make([]waveConn, 0)
	waveSeq := 0

	go func() {
		waveTick := time.NewTicker(*waveInterval)
		defer waveTick.Stop()
		for {
			select {
			case <-stopAll:
				return
			case <-waveTick.C:
				waveSeq++
				myWave := waveSeq
				for i := range *wave {
					userID := fmt.Sprintf("churn_wave%d_%d", myWave, i)
					s, err := login(*addr, *serverID, userID)
					if err != nil {
						c.loginFail.Add(1)
						continue
					}
					c.loginOK.Add(1)
					mu.Lock()
					waves = append(waves, waveConn{s: s, bornAt: time.Now()})
					mu.Unlock()
					startSession(s, true)
					time.Sleep(20 * time.Millisecond)
				}
				c.wavesDone.Add(1)
			}
		}
	}()

	// 波次到期churn关闭
	go func() {
		lifeTick := time.NewTicker(10 * time.Second)
		defer lifeTick.Stop()
		for {
			select {
			case <-stopAll:
				return
			case <-lifeTick.C:
				mu.Lock()
				kept := waves[:0]
				for _, w := range waves {
					if time.Since(w.bornAt) > *waveLife {
						if w.s.closed.CompareAndSwap(false, true) {
							close(w.s.stop)
							w.s.conn.Close()
						}
						c.churned.Add(1)
						continue
					}
					kept = append(kept, w)
				}
				waves = kept
				mu.Unlock()
			}
		}
	}()

	// 报告
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		var lastSent int64
		for {
			select {
			case <-stopAll:
				return
			case now := <-tick.C:
				sent := c.sent.Load()
				el := now.Sub(start).Seconds()
				r := report{
					Ts: now, ElapsedSec: el,
					SentDelta: sent - lastSent, Rate: float64(sent-lastSent) / 10,
					SentTotal: sent, RecvTotal: c.recv.Load(),
					ActiveConns: int(active.Load()),
					LoginOK:     c.loginOK.Load(), LoginFail: c.loginFail.Load(),
					WavesDone: c.wavesDone.Load(), Churned: c.churned.Load(),
					WriteErr: c.writeErr.Load(), ReadClosed: c.readClosed.Load(),
				}
				lastSent = sent
				enc.Encode(r)
				f.Sync()
				fmt.Printf("[%6.0fs] rate=%.0f active=%d loginOK=%d waves=%d churn=%d wrErr=%d rdClose=%d\n",
					el, r.Rate, r.ActiveConns, r.LoginOK, r.WavesDone, r.Churned, r.WriteErr, r.ReadClosed)
			}
		}
	}()

	// 主循环直到 duration
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}

	// 收尾：停发送，关闭全部会话
	close(stopAll)
	mu.Lock()
	for _, w := range waves {
		if w.s.closed.CompareAndSwap(false, true) {
			close(w.s.stop)
			w.s.conn.Close()
		}
	}
	mu.Unlock()
	for _, s := range baseSessions {
		if s.closed.CompareAndSwap(false, true) {
			close(s.stop)
			s.conn.Close()
		}
	}
	time.Sleep(2 * time.Second)

	final := map[string]any{
		"ts": time.Now(), "elapsed_s": time.Since(start).Seconds(),
		"sent_total": c.sent.Load(), "recv_total": c.recv.Load(),
		"login_ok": c.loginOK.Load(), "login_fail": c.loginFail.Load(),
		"waves_done": c.wavesDone.Load(), "churned": c.churned.Load(),
		"write_err": c.writeErr.Load(), "read_closed": c.readClosed.Load(),
		"final_active": int(active.Load()),
		"avg_rate":     float64(c.sent.Load()) / time.Since(start).Seconds(),
	}
	enc.Encode(final)
	f.Sync()
	fmt.Printf("FINAL %+v\n", final)
}
