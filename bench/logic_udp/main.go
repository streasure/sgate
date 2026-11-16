// main logic 侧 UDP 阶段 4 压测服务（bench/logic_udp）：
//   - LoginGate 登记会话并加入推送分组
//   - 计数上行 CmdUdpAction（供 bench/udp_uplink 与 /stats 对账）
//   - 按 -push-interval 向组内会话推送 CmdUdpState（snapshot 内嵌发送时刻，
//     由 bench/udp_downlink 据此算端到端下行时延分位数）
//
// 用法：
//
//	.\logic_udp.exe                          # 只登记 + 计数（上行压测）
//	.\logic_udp.exe -push-interval 10ms      # 另加下行推送（下行压测）
//
// 服务 ID 必须与压测客户端的 -server-id 一致（默认 logic-udp）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	logic "github.com/streasure/logic"
	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/bench/udputil"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

func main() {
	port := flag.String("port", "50062", "gRPC listen port")
	id := flag.String("id", "logic-udp", "service instance ID")
	shardCount := flag.Int("shardCount", 0, "streams dialed into each gateway (0=NumCPU*8)")
	logConfig := flag.String("config", "config/log/bench.yaml", "log configuration")
	pprofAddr := flag.String("pprof", "", "pprof listen address (empty=off), e.g. 127.0.0.1:6063")
	pushInterval := flag.Duration("push-interval", 0, "CmdUdpState 推送节拍（0=不推送，仅登记与计数）")
	group := flag.String("group", "udp_bench", "推送分组（LoginGate 时加入）")
	httpAddr := flag.String("http-addr", "127.0.0.1:8091", "计数器 HTTP 端点（/counters，空=关闭）")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	ctx := context.TODO()
	if *pprofAddr != "" {
		obs.StartPProfServer(*pprofAddr)
	}

	var (
		logins            atomic.Int64
		udpActionReceived atomic.Int64
		pushed            atomic.Int64
		pushFailed        atomic.Int64
		stateVer          atomic.Int64
	)

	svc := logic.NewService(
		logic.WithListenPort(*port),
		logic.WithServiceID(*id),
		logic.WithServiceName("logic"),
		logic.WithServerType("Logic"),
		logic.WithZone("default"),
		logic.WithEtcd("http://127.0.0.1:2379"),
		logic.WithShardCount(*shardCount),
	)

	// 登录：绑定会话并加入推送分组（respCmd=0，登录应答由网关自己回）
	svc.RegisterProto(protocol.CmdLoginGate, &protocol.LoginGateReq{}, 0, func(hctx *logic.Context, req proto.Message) proto.Message {
		_ = req.(*protocol.LoginGateReq)
		svc.Server().JoinGroup(*group, hctx.ConnectionID)
		if hctx.UserUUID != "" {
			_ = svc.Server().JoinGroupForUser(hctx.UserUUID, *group)
		}
		logins.Add(1)
		return nil
	})

	// 上行 UdpAction：只计数不回包（回包会污染下行时延口径）
	svc.RegisterProto(protocol.CmdUdpAction, &protocol.UdpAction{}, 0, func(hctx *logic.Context, req proto.Message) proto.Message {
		_ = req.(*protocol.UdpAction)
		if n := udpActionReceived.Add(1); n == 1 {
			tlog.Info(ctx, "logic_udp first UdpAction received sessionID=%s", hctx.ConnectionID)
		}
		return nil
	})

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}
	tlog.Info(ctx, "logic_udp started port=%s id=%s group=%s pushInterval=%s", *port, *id, *group, pushInterval.String())

	// 计数器端点：让压测客户端做「客户端发送 → 网关转发 → logic 收到」三方对账
	var counterSrv *http.Server
	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/counters", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(udputil.LogicCounters{
				Logins:            logins.Load(),
				UdpActionReceived: udpActionReceived.Load(),
				Pushed:            pushed.Load(),
				PushFailed:        pushFailed.Load(),
				StateVer:          stateVer.Load(),
				Members:           int64(svc.Server().GetGroupCount(*group)),
			})
		})
		counterSrv = &http.Server{Addr: *httpAddr, Handler: mux}
		panicutil.Go(ctx, "logic_udp.countersServer", func() {
			if err := counterSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				tlog.Warn(ctx, "logic_udp counters server stopped addr=%s error=%v", *httpAddr, err)
			}
		})
		tlog.Info(ctx, "logic_udp counters endpoint addr=%s path=/counters", *httpAddr)
	}

	pushCtx, cancelPush := context.WithCancel(context.Background())
	defer cancelPush()

	if *pushInterval > 0 {
		panicutil.GoLoop(ctx, "logic_udp.pushLoop", func() {
			ticker := time.NewTicker(*pushInterval)
			defer ticker.Stop()
			var membersCached []string
			var membersTS int64
			for {
				select {
				case <-pushCtx.Done():
					return
				case <-ticker.C:
					now := time.Now().UnixNano()
					if now-membersTS > int64(100*time.Millisecond) || len(membersCached) == 0 {
						membersCached = svc.Server().GetGroupMembers(*group)
						membersTS = now
					}
					for _, sessionID := range membersCached {
						ver := stateVer.Add(1)
						body, err := udputil.EncodeState(ver, time.Now().UnixNano())
						if err != nil {
							pushFailed.Add(1)
							continue
						}
						if err := svc.Server().PushToConnection(sessionID, protocol.CmdUdpState, body); err != nil {
							pushFailed.Add(1)
							continue
						}
						pushed.Add(1)
					}
				}
			}
		})
	}

	panicutil.GoLoop(ctx, "logic_udp.progressLoop", func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pushCtx.Done():
				return
			case <-ticker.C:
				tlog.Info(ctx, "logic_udp progress members=%d logins=%d udpActionReceived=%d pushed=%d pushFailed=%d stateVer=%d",
					svc.Server().GetGroupCount(*group), logins.Load(), udpActionReceived.Load(),
					pushed.Load(), pushFailed.Load(), stateVer.Load())
			}
		}
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	cancelPush()
	if counterSrv != nil {
		_ = counterSrv.Close()
	}
	svc.Stop()
	tlog.Info(ctx, "logic_udp stopped logins=%d udpActionReceived=%d pushed=%d pushFailed=%d",
		logins.Load(), udpActionReceived.Load(), pushed.Load(), pushFailed.Load())
}
