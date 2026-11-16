// main UDP 阶段 4 下行压测：stateVer 推送时延分位数（UDP vs WS 回落对照）。
//
// 流程：WS 登录取一次性 udp_token → ASSOCIATE 建联 → 周期发 UdpAction 保活
// （关联 lastSeen 新鲜度阈值 = idleTimeout/2 = 30s，静默超时下行会回落 WS）
// → 接收 logic 推送的 CmdUdpState，按 snapshot 内嵌的发送时刻算端到端时延。
//
// 前置：
//  1. 网关：set CONF=config\bench\udp-on.yaml & start.bat
//  2. logic：.\logic_udp.exe -push-interval 10ms（按会话推送 CmdUdpState）
//
// 时延口径：logic 在 UdpState.snapshot 前 8 字节写入 UnixNano（udputil.EncodeState），
// 客户端收到即算 now - ts，覆盖 logic → 网关 gRPC → UDP 下行全链路。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/bench/udputil"
	"github.com/streasure/util/tlog"
)

// session 一条已完成 WS 登录 + UDP 关联的压测会话。
type session struct {
	ws     net.Conn
	udp    *net.UDPConn
	gw     *net.UDPAddr
	connID string
	token  string
	port   int
	seq    int64   // 保活上行 seq 游标（单发送者串行取号）
	lats   []int64 // UDP 下行时延（ns），仅本会话读协程写
	wsLats []int64 // 回落 WS 的下行时延（ns），仅本会话 WS 读协程写
}

// maxClockSkew 时延越界阈值：负值或超过该值视为坏样本（时钟/陈旧快照）。
const maxClockSkew = 5 * time.Second

func main() {
	addr := flag.String("addr", "127.0.0.1:48081", "sgate WebSocket address")
	duration := flag.Duration("duration", 30*time.Second, "downlink measurement duration")
	parallel := flag.Int("parallel", 100, "number of sessions (one WS conn + one UDP assoc each)")
	serverID := flag.String("server-id", "logic-udp", "logic service ID bound at login")
	logConfig := flag.String("config", "config/log/bench.yaml", "log config")
	loginBatch := flag.Int("login-batch", 100, "concurrent login batch size")
	assocTimeout := flag.Duration("assoc-timeout", 5*time.Second, "ASSOCIATE ack timeout")
	keepalive := flag.Duration("keepalive", time.Second, "UdpAction keepalive interval (must be < idleTimeout/2)")
	statsAddr := flag.String("stats-addr", "http://127.0.0.1:8081/stats", "gateway /stats endpoint (empty = skip)")
	logicAddr := flag.String("logic-counters", "http://127.0.0.1:8091/counters", "logic_udp /counters endpoint (empty = skip)")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	ctx := context.TODO()
	tlog.Info(ctx, "udp_downlink started addr=%s duration=%s parallel=%d serverID=%s keepalive=%s",
		*addr, duration.String(), *parallel, *serverID, keepalive.String())

	if *parallel <= 0 {
		tlog.Info(ctx, "udp_downlink completed parallel=0 (nothing to do)")
		return
	}
	if *keepalive <= 0 {
		tlog.Error(ctx, "udp_downlink keepalive must be > 0 (association would go stale)")
		return
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		tlog.Error(ctx, "udp_downlink bad addr=%s error=%v", *addr, err)
		return
	}

	var (
		loginFailed atomic.Int64
		assocOK     atomic.Int64
		assocFail   atomic.Int64
		udpState    atomic.Int64
		udpError    atomic.Int64
		udpOther    atomic.Int64
		badDatagram atomic.Int64
		invalid     atomic.Int64
		outOfOrder  atomic.Int64
		wsState     atomic.Int64
		wsFrames    atomic.Int64
		keepSent    atomic.Int64
	)

	var (
		mu         sync.Mutex
		sessions   []*session
		assocLat   []int64 // 关联耗时（ns）
		assocFirst time.Time
		assocLast  time.Time
	)
	var wg sync.WaitGroup

	// ---------------------------------------------------------------- 登录
	batchSize := min(*loginBatch, *parallel)
	for batch := 0; batch < *parallel; batch += batchSize {
		end := min(batch+batchSize, *parallel)
		var batchWG sync.WaitGroup
		batchWG.Add(end - batch)
		for i := batch; i < end; i++ {
			idx := i
			go func() {
				defer batchWG.Done()
				ws, ack, err := udputil.WSLogin(*addr, *serverID, fmt.Sprintf("udp_user_%d", idx), 5*time.Second)
				if err != nil {
					tlog.Warn(ctx, "udp_downlink login failed client=%d error=%v", idx, err)
					loginFailed.Add(1)
					return
				}
				s := &session{
					ws:     ws,
					connID: ack.SessionId,
					token:  ack.UdpToken,
					port:   int(ack.UdpPort),
				}
				mu.Lock()
				sessions = append(sessions, s)
				mu.Unlock()
			}()
		}
		batchWG.Wait()
		tlog.Info(ctx, "udp_downlink login batch done range=%d~%d ok=%d failed=%d",
			batch, end-1, len(sessions), loginFailed.Load())
	}

	loginOK := len(sessions)
	tlog.Info(ctx, "udp_downlink login done connected=%d failed=%d", loginOK, loginFailed.Load())
	if loginOK == 0 {
		tlog.Info(ctx, "udp_downlink completed connected=0 (no session)")
		return
	}

	// -------------------------------------------------------------- 建联
	var assocWG sync.WaitGroup
	assocWG.Add(len(sessions))
	for _, s := range sessions {
		go func(s *session) {
			defer assocWG.Done()
			started := time.Now()
			mu.Lock()
			if assocFirst.IsZero() || started.Before(assocFirst) {
				assocFirst = started
			}
			mu.Unlock()
			if s.port <= 0 {
				tlog.Warn(ctx, "udp_downlink no udp port in login ack connID=%s (gateway udp.enabled=false?)", s.connID)
				assocFail.Add(1)
				return
			}
			udp, err := udputil.OpenUDP()
			if err != nil {
				tlog.Warn(ctx, "udp_downlink open udp failed connID=%s error=%v", s.connID, err)
				assocFail.Add(1)
				return
			}
			gw, err := udputil.ResolveGW(host, s.port)
			if err != nil {
				_ = udp.Close()
				tlog.Warn(ctx, "udp_downlink resolve gw failed connID=%s port=%d error=%v", s.connID, s.port, err)
				assocFail.Add(1)
				return
			}
			ack, lat, err := udputil.Associate(udp, gw, s.token, *assocTimeout)
			if err != nil {
				_ = udp.Close()
				tlog.Warn(ctx, "udp_downlink associate failed connID=%s error=%v", s.connID, err)
				assocFail.Add(1)
				return
			}
			s.udp = udp
			s.gw = gw
			s.seq = ack.SeqBase
			ended := time.Now()
			assocOK.Add(1)
			mu.Lock()
			assocLat = append(assocLat, int64(lat))
			if ended.After(assocLast) {
				assocLast = ended
			}
			mu.Unlock()
		}(s)
	}
	assocWG.Wait()
	// QPS 口径 = 成功建联数 / 首个 ASSOCIATE 发出 → 末个 Ack 到达（并发爬坡窗口）。
	assocElapsed := 0.0
	if !assocFirst.IsZero() && assocLast.After(assocFirst) {
		assocElapsed = assocLast.Sub(assocFirst).Seconds()
	}

	var live []*session
	for _, s := range sessions {
		if s.udp != nil {
			live = append(live, s)
		} else if s.ws != nil {
			_ = s.ws.Close()
		}
	}

	sort.Slice(assocLat, func(i, j int) bool { return assocLat[i] < assocLat[j] })
	assocQPS, assocMin, assocAvg := udputil.AssocStats(assocLat, assocElapsed)
	tlog.Info(ctx, "udp_downlink associate done ok=%d fail=%d window=%.4fs qps=%.0f min=%.1fµs avg=%.1fµs p50=%.1fµs p95=%.1fµs p99=%.1fµs",
		assocOK.Load(), assocFail.Load(), assocElapsed, assocQPS,
		udputil.Micros(assocMin), udputil.Micros(assocAvg),
		udputil.Micros(udputil.Percentile(assocLat, 0.50)),
		udputil.Micros(udputil.Percentile(assocLat, 0.95)),
		udputil.Micros(udputil.Percentile(assocLat, 0.99)))

	if len(live) == 0 {
		tlog.Info(ctx, "udp_downlink completed assocOK=0 (no live session)")
		closeAll(live, sessions)
		wg.Wait()
		return
	}

	statsBefore, err := udputil.FetchStats(*statsAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_downlink fetch stats before failed error=%v", err)
	}
	logicBefore, err := udputil.FetchLogicCounters(*logicAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_downlink fetch logic counters before failed error=%v", err)
	}

	// -------------------------------------------------------------- 收发
	measureStart := time.Now()
	stopKeepalive := make(chan struct{})

	for _, s := range live {
		// UDP 下行读：时延样本只由本协程写入，无需加锁
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			rbuf := make([]byte, udputil.ReadBufSize)
			var lastVer int64
			for {
				cmd, _, body, err := udputil.ReadDatagram(s.udp, rbuf)
				if err != nil {
					if errors.Is(err, udputil.ErrBadDatagram) {
						badDatagram.Add(1)
						continue
					}
					return // socket 关闭 / 读超时
				}
				if cmd != protocol.CmdUdpState {
					switch cmd {
					case protocol.CmdError:
						udpError.Add(1)
					default:
						udpOther.Add(1)
					}
					continue
				}
				stateVer, sentNano, derr := udputil.DecodeState(body)
				if derr != nil {
					badDatagram.Add(1)
					continue
				}
				lat := time.Now().UnixNano() - sentNano
				if lat < 0 || lat > int64(maxClockSkew) {
					invalid.Add(1)
					continue
				}
				if lastVer != 0 && stateVer < lastVer {
					outOfOrder.Add(1)
				}
				lastVer = stateVer
				s.lats = append(s.lats, lat)
				udpState.Add(1)
			}
		}(s)

		// WS 读：统计回落到 WS 的下行（健康关联下应为 0）
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			for {
				frame, err := udputil.ReadWSFrame(s.ws)
				if err != nil {
					return
				}
				wsFrames.Add(1)
				if frame.Cmd != protocol.CmdUdpState {
					continue
				}
				_, sentNano, derr := udputil.DecodeState(frame.Body)
				if derr != nil {
					badDatagram.Add(1)
					continue
				}
				lat := time.Now().UnixNano() - sentNano
				if lat < 0 || lat > int64(maxClockSkew) {
					invalid.Add(1)
					continue
				}
				s.wsLats = append(s.wsLats, lat)
				wsState.Add(1)
			}
		}(s)

		// 保活上行：刷新关联 lastSeen，否则超过 idleTimeout/2 下行回落 WS
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			ticker := time.NewTicker(*keepalive)
			defer ticker.Stop()
			ka := []byte("ka")
			for {
				select {
				case <-stopKeepalive:
					return
				case <-ticker.C:
					seq := s.seq + 1
					out, err := udputil.EncodeAction(nil, seq, ka)
					if err != nil {
						return
					}
					if _, err := s.udp.WriteToUDP(out, s.gw); err != nil {
						return
					}
					s.seq = seq
					keepSent.Add(1)
				}
			}
		}(s)
	}

	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				el := time.Since(measureStart).Seconds()
				tlog.Info(ctx, "udp_downlink progress elapsed=%.1fs udpState=%d wsState=%d keepalive=%d",
					el, udpState.Load(), wsState.Load(), keepSent.Load())
			case <-progressDone:
				return
			}
		}
	}()

	time.Sleep(*duration)
	close(progressDone)
	close(stopKeepalive)

	elapsed := time.Since(measureStart).Seconds()
	time.Sleep(300 * time.Millisecond) // 让在途下行与 /stats 落账

	statsAfter, err := udputil.FetchStats(*statsAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_downlink fetch stats after failed error=%v", err)
	}
	logicAfter, err := udputil.FetchLogicCounters(*logicAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_downlink fetch logic counters after failed error=%v", err)
	}
	// 客户端计数紧随 logic 计数取样，二者之差 = 取样间隙的在途推送
	udpStateFinal, wsStateFinal := udpState.Load(), wsState.Load()

	closeAll(live, sessions)
	wg.Wait()

	// -------------------------------------------------------------- 报告
	var all, wsAll []int64
	for _, s := range sessions {
		all = append(all, s.lats...)
		wsAll = append(wsAll, s.wsLats...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	sort.Slice(wsAll, func(i, j int) bool { return wsAll[i] < wsAll[j] })

	tlog.Info(ctx, "udp_downlink completed sessions=%d loginFailed=%d assocOK=%d assocFail=%d elapsed=%.1fs",
		loginOK, loginFailed.Load(), assocOK.Load(), assocFail.Load(), elapsed)
	tlog.Info(ctx, "udp_downlink downlink udpState=%d wsState=%d badDatagram=%d invalid=%d outOfOrder=%d other=%d wsFrames=%d",
		udpStateFinal, wsStateFinal, badDatagram.Load(), invalid.Load(), outOfOrder.Load(),
		udpOther.Load(), wsFrames.Load())
	logLatency(ctx, "udp_downlink udp latency", all)
	logLatency(ctx, "udp_downlink wsFallback latency", wsAll)
	tlog.Info(ctx, "udp_downlink keepalive sent=%d", keepSent.Load())

	if wsStateFinal > 0 {
		tlog.Warn(ctx, "udp_downlink wsFallback>0: %d 条下行走了 WS 回落（关联 stale 或 UDP 发送失败），需核对保活间隔与 /stats 的 udpSweptWsSurviving", wsStateFinal)
	}
	var logicPushed int64
	if logicBefore != nil && logicAfter != nil {
		logicPushed = logicAfter.Pushed - logicBefore.Pushed
		tlog.Info(ctx, "udp_downlink logic counters delta pushed=%d pushFailed=%d udpActionReceived=%d",
			logicPushed, logicAfter.PushFailed-logicBefore.PushFailed,
			logicAfter.UdpActionReceived-logicBefore.UdpActionReceived)
	}
	if d := udputil.Delta(statsBefore, statsAfter); d != nil {
		tlog.Info(ctx, "udp_downlink gateway stats delta pushedToClient=%d received=%d forwarded=%d unassociatedDrops=%d sweptWsSurviving=%d",
			d.PushedToClient, d.Received, d.Forwarded, d.UDPUnassociatedDrops, d.UDPSweptWSSurviving)
	}
	if logicPushed > 0 {
		delivered := udpStateFinal + wsStateFinal
		lost := logicPushed - delivered
		// 客户端计数在 logic 之后取样，取样间隙的在途推送会让 loss 出现小负值（正常）。
		tlog.Info(ctx, "udp_downlink delivery logicPushed=%d clientReceived=%d loss=%d (%.3f%%, 负值=取样间隙在途) viaUdp=%d viaWsFallback=%d",
			logicPushed, delivered, lost, 100*float64(lost)/float64(logicPushed), udpStateFinal, wsStateFinal)
	}
}

// logLatency 输出时延分位数（升序样本）。
func logLatency(ctx context.Context, tag string, sorted []int64) {
	if len(sorted) == 0 {
		tlog.Info(ctx, "%s n=0 (no sample)", tag)
		return
	}
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	tlog.Info(ctx, "%s n=%d avg=%.1fµs p50=%.1fµs p95=%.1fµs p99=%.1fµs max=%.1fµs",
		tag, len(sorted),
		udputil.Micros(sum/int64(len(sorted))),
		udputil.Micros(udputil.Percentile(sorted, 0.50)),
		udputil.Micros(udputil.Percentile(sorted, 0.95)),
		udputil.Micros(udputil.Percentile(sorted, 0.99)),
		udputil.Micros(udputil.Percentile(sorted, 1.0)))
}

// closeAll 关闭全部 UDP / WS 连接，让收发协程退出。
func closeAll(live, all []*session) {
	for _, s := range live {
		if s.udp != nil {
			_ = s.udp.Close()
		}
	}
	for _, s := range all {
		if s.ws != nil {
			_ = s.ws.Close()
		}
	}
}
