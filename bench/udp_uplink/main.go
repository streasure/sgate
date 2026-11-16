// main UDP 阶段 4 上行压测：关联建立 QPS + 上行 msg/s（含 /stats 对账）。
//
// 流程：WS 登录取一次性 udp_token → ASSOCIATE 建联（测成功率 / QPS / 时延分位数）
// → 持续发 UdpAction 上行 → 与网关 /stats 的 forwarded 增量对账。
//
// 前置：
//  1. 网关：set CONF=config\bench\udp-on.yaml & start.bat
//  2. logic：.\logic_udp.exe（注册 CmdUdpAction 计数，对账看它的 udpActionReceived）
//
// 上行 seq 由每个会话的单发送者串行取号（seqBase 起号）：并发取号会让数据报
// 乱序超出网关 seqWindow(1024) 被判「过旧」静默丢，污染对账口径。
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
// WS 是会话本体（保活与回落通道），UDP socket 是关联通道（上下行数据报）。
type session struct {
	ws     net.Conn
	udp    *net.UDPConn
	gw     *net.UDPAddr
	connID string
	token  string // 登录 ack 下发的一次性关联 token（建联后即失效）
	port   int    // 登录 ack 下发的网关 UDP 监听端口
	seq    int64  // 上行 seq 游标，起点 = ASSOCIATE Ack 的 seqBase
}

func main() {
	addr := flag.String("addr", "127.0.0.1:48081", "sgate WebSocket address")
	duration := flag.Duration("duration", 30*time.Second, "uplink measurement duration")
	parallel := flag.Int("parallel", 100, "number of sessions (one WS conn + one UDP assoc each)")
	serverID := flag.String("server-id", "logic-udp", "logic service ID bound at login")
	logConfig := flag.String("config", "config/log/bench.yaml", "log config")
	loginBatch := flag.Int("login-batch", 100, "concurrent login batch size")
	assocTimeout := flag.Duration("assoc-timeout", 5*time.Second, "ASSOCIATE ack timeout")
	payloadSize := flag.Int("payload", 64, "UdpAction payload bytes (datagram must stay <= 1400)")
	statsAddr := flag.String("stats-addr", "http://127.0.0.1:8081/stats", "gateway /stats endpoint (empty = skip reconcile)")
	logicAddr := flag.String("logic-counters", "http://127.0.0.1:8091/counters", "logic_udp /counters endpoint (empty = skip)")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	ctx := context.TODO()
	tlog.Info(ctx, "udp_uplink started addr=%s duration=%s parallel=%d serverID=%s payload=%d",
		*addr, duration.String(), *parallel, *serverID, *payloadSize)

	if *parallel <= 0 {
		tlog.Info(ctx, "udp_uplink completed parallel=0 (nothing to do)")
		return
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		tlog.Error(ctx, "udp_uplink bad addr=%s error=%v", *addr, err)
		return
	}

	var (
		loginFailed atomic.Int64
		assocOK     atomic.Int64
		assocFail   atomic.Int64
		sent        atomic.Int64
		sendErrs    atomic.Int64
		udpState    atomic.Int64
		udpError    atomic.Int64
		udpOther    atomic.Int64
		udpBad      atomic.Int64
		wsFrames    atomic.Int64
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
					tlog.Warn(ctx, "udp_uplink login failed client=%d error=%v", idx, err)
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
				// 登录 ack 已同步读完，此后 WS 只做排空（防服务端推送顶住 TCP 窗口）
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						if _, err := udputil.ReadWSFrame(ws); err != nil {
							return
						}
						wsFrames.Add(1)
					}
				}()
			}()
		}
		batchWG.Wait()
		tlog.Info(ctx, "udp_uplink login batch done range=%d~%d ok=%d failed=%d",
			batch, end-1, len(sessions), loginFailed.Load())
	}

	loginOK := len(sessions)
	tlog.Info(ctx, "udp_uplink login done connected=%d failed=%d", loginOK, loginFailed.Load())
	if loginOK == 0 {
		tlog.Info(ctx, "udp_uplink completed connected=0 (no session)")
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
				tlog.Warn(ctx, "udp_uplink no udp port in login ack connID=%s (gateway udp.enabled=false?)", s.connID)
				assocFail.Add(1)
				return
			}
			udp, err := udputil.OpenUDP()
			if err != nil {
				tlog.Warn(ctx, "udp_uplink open udp failed connID=%s error=%v", s.connID, err)
				assocFail.Add(1)
				return
			}
			gw, err := udputil.ResolveGW(host, s.port)
			if err != nil {
				_ = udp.Close()
				tlog.Warn(ctx, "udp_uplink resolve gw failed connID=%s port=%d error=%v", s.connID, s.port, err)
				assocFail.Add(1)
				return
			}
			ack, lat, err := udputil.Associate(udp, gw, s.token, *assocTimeout)
			if err != nil {
				_ = udp.Close()
				tlog.Warn(ctx, "udp_uplink associate failed connID=%s error=%v", s.connID, err)
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
	// 单会话只有一个样本、窗口为 0，无法度量 QPS，如实报 0。
	assocElapsed := 0.0
	if !assocFirst.IsZero() && assocLast.After(assocFirst) {
		assocElapsed = assocLast.Sub(assocFirst).Seconds()
	}

	// 建联失败的会话退出：token 一次性，重新登录才能重试，本轮不再参与上行
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
	tlog.Info(ctx, "udp_uplink associate done ok=%d fail=%d window=%.4fs qps=%.0f min=%.1fµs avg=%.1fµs p50=%.1fµs p95=%.1fµs p99=%.1fµs max=%.1fµs",
		assocOK.Load(), assocFail.Load(), assocElapsed, assocQPS,
		udputil.Micros(assocMin), udputil.Micros(assocAvg),
		udputil.Micros(udputil.Percentile(assocLat, 0.50)),
		udputil.Micros(udputil.Percentile(assocLat, 0.95)),
		udputil.Micros(udputil.Percentile(assocLat, 0.99)),
		udputil.Micros(udputil.Percentile(assocLat, 1.0)))

	if len(live) == 0 {
		tlog.Info(ctx, "udp_uplink completed assocOK=0 (no live session)")
		closeAll(live, sessions)
		wg.Wait()
		return
	}

	// -------------------------------------------------------------- 上行
	statsBefore, err := udputil.FetchStats(*statsAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_uplink fetch stats before failed error=%v", err)
	}
	logicBefore, err := udputil.FetchLogicCounters(*logicAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_uplink fetch logic counters before failed error=%v", err)
	}

	payload := make([]byte, *payloadSize)
	for i := range payload {
		payload[i] = byte(i)
	}

	measureStart := time.Now()
	deadline := measureStart.Add(*duration)

	for _, s := range live {
		// 发送者
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			buf := make([]byte, 0, 512)
			for time.Now().Before(deadline) {
				seq := s.seq + 1
				out, err := udputil.EncodeAction(buf, seq, payload)
				if err != nil {
					sendErrs.Add(1)
					continue
				}
				if len(out) > udputil.MaxDatagram {
					sendErrs.Add(1)
					tlog.Warn(ctx, "udp_uplink datagram too large len=%d max=%d", len(out), udputil.MaxDatagram)
					return
				}
				if _, err := s.udp.WriteToUDP(out, s.gw); err != nil {
					// 发送缓冲瞬时满等可恢复错误：计数后继续，循环由 deadline 收口
					sendErrs.Add(1)
				}
				s.seq = seq
				buf = out[:0]
				sent.Add(1)
			}
		}(s)
		// 排空下行（错误帧 / 状态帧），同时证明关联在收包
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			rbuf := make([]byte, udputil.ReadBufSize)
			for {
				cmd, _, _, err := udputil.ReadDatagram(s.udp, rbuf)
				if err != nil {
					if errors.Is(err, udputil.ErrBadDatagram) {
						udpBad.Add(1)
						continue
					}
					return // socket 关闭 / 读超时
				}
				switch cmd {
				case protocol.CmdUdpState:
					udpState.Add(1)
				case protocol.CmdError:
					udpError.Add(1)
				default:
					udpOther.Add(1)
				}
			}
		}(s)
	}

	// 进度
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				el := time.Since(measureStart).Seconds()
				tlog.Info(ctx, "udp_uplink progress elapsed=%.1fs sent=%d rate=%.0f sendErrs=%d",
					el, sent.Load(), float64(sent.Load())/el, sendErrs.Load())
			case <-progressDone:
				return
			}
		}
	}()

	time.Sleep(*duration)
	close(progressDone)

	// 快照取样点：此后关 socket 让收发协程退出
	sentFinal := sent.Load()
	elapsed := time.Since(measureStart).Seconds()
	time.Sleep(300 * time.Millisecond) // 让在途 pipeline 落账
	udpStateFinal, udpErrorFinal := udpState.Load(), udpError.Load()
	udpOtherFinal, udpBadFinal := udpOther.Load(), udpBad.Load()

	statsAfter, err := udputil.FetchStats(*statsAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_uplink fetch stats after failed error=%v", err)
	}
	logicAfter, err := udputil.FetchLogicCounters(*logicAddr, 2*time.Second)
	if err != nil {
		tlog.Warn(ctx, "udp_uplink fetch logic counters after failed error=%v", err)
	}

	closeAll(live, sessions)
	wg.Wait()

	// -------------------------------------------------------------- 报告
	tlog.Info(ctx, "udp_uplink completed sessions=%d loginFailed=%d assocOK=%d assocFail=%d",
		loginOK, loginFailed.Load(), assocOK.Load(), assocFail.Load())
	tlog.Info(ctx, "udp_uplink uplink sent=%d elapsed=%.1fs msgPerSec=%.0f sendErrs=%d",
		sentFinal, elapsed, float64(sentFinal)/max(elapsed, 1e-9), sendErrs.Load())
	tlog.Info(ctx, "udp_uplink downlink udpState=%d udpError=%d udpOther=%d badDatagram=%d wsFrames=%d",
		udpStateFinal, udpErrorFinal, udpOtherFinal, udpBadFinal, wsFrames.Load())

	var gwForwarded, logicReceived int64
	if d := udputil.Delta(statsBefore, statsAfter); d != nil {
		gwForwarded = d.Forwarded
		tlog.Info(ctx, "udp_uplink gateway stats delta received=%d forwarded=%d failed=%d droppedTotal=%d rateLimit=%d auth=%d noLogic=%d unsupported=%d",
			d.Received, d.Forwarded, d.Failed, d.DroppedTotal, d.DroppedRateLimit,
			d.DroppedAuth, d.DroppedNoLogic, d.DroppedUnsupported)
	}
	if logicBefore != nil && logicAfter != nil {
		logicReceived = logicAfter.UdpActionReceived - logicBefore.UdpActionReceived
		tlog.Info(ctx, "udp_uplink logic counters delta logins=%d udpActionReceived=%d",
			logicAfter.Logins-logicBefore.Logins, logicReceived)
	}
	if gwForwarded > 0 || logicReceived > 0 {
		ingressLoss := sentFinal - gwForwarded
		linkLoss := gwForwarded - logicReceived
		// logic 在 gateway 之后取样，取样间隙内新转发的消息会让 linkLoss 出现小负值（正常）。
		tlog.Info(ctx, "udp_uplink reconcile sent=%d -> gatewayForwarded=%d (ingressLoss=%d %.3f%%) -> logicReceived=%d (linkLoss=%d, 负值=取样间隙在途)",
			sentFinal, gwForwarded, ingressLoss, 100*float64(ingressLoss)/max(float64(sentFinal), 1),
			logicReceived, linkLoss)
	} else {
		tlog.Info(ctx, "udp_uplink reconcile skipped (stats-addr / logic-counters empty), sent=%d", sentFinal)
	}
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
