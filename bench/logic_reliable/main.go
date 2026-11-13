// logic_reliable 可靠推送端到端测试的逻辑服侧（roadmap #4 阶段 4）。
//
// 与 logic2 的区别仅在推送 API：PushToConnection → PushReliable（带会话内
// 单调 SeqId + 未确认窗口 + janitor 超时重传），并每秒打印 PushMetrics
// 快照（acked/retransmits/giveUps/droppedOffline）供与客户端 bench 对账。
// 登录处理、JoinGroup、worker 池、进度日志骨架与 logic2_ws 一致。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	logic "github.com/streasure/sgate/internal/logic"
	"github.com/streasure/sgate/internal/obs"

	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

const cmdPush int32 = 9000001

func main() {
	port := flag.String("port", "50061", "gRPC listen port")
	id := flag.String("id", "logic-rel", "service instance ID")
	pushWorkers := flag.Int("push-workers", 128, "parallel push workers")
	pushSize := flag.Int("push-size", 64, "payload size in bytes")
	expectedMembers := flag.Int("expected-members", 0, "wait for members before pushing")
	shardCount := flag.Int("shardCount", 0, "streams dialed into each gateway (0=NumCPU*8)")
	windowSize := flag.Int("window-size", 64, "reliable window size per session (0=default 64)")
	scanInterval := flag.Duration("scan-interval", 0, "retransmit scan interval (0=default 200ms)")
	initialBackoff := flag.Duration("initial-backoff", 0, "first retransmit backoff (0=default 300ms)")
	maxRetries := flag.Int("max-retries", 0, "max retransmits per message, excluding first send (0=default 5)")
	logConfig := flag.String("config", "config/log/bench.yaml", "log config")
	pprofAddr := flag.String("pprof", "", "pprof listen address (empty=off), e.g. 127.0.0.1:6064")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	if *pprofAddr != "" {
		obs.StartPProfServer(*pprofAddr)
	}

	svc := logic.NewService(
		logic.WithListenPort(*port),
		logic.WithServiceID(*id),
		logic.WithServiceName("logic"),
		logic.WithServerType("Logic"),
		logic.WithZone("default"),
		logic.WithEtcd("http://127.0.0.1:2379"),
		logic.WithShardCount(*shardCount),
		logic.WithReliability(logic.ReliabilityConfig{
			WindowSize:     *windowSize,
			ScanInterval:   *scanInterval,
			InitialBackoff: *initialBackoff,
			MaxRetries:     *maxRetries,
		}),
	)

	var totalPushed atomic.Int64
	var windowFull atomic.Int64

	svc.RegisterProto(protocol.CmdLoginGate, &protocol.LoginGateReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		_ = req.(*protocol.LoginGateReq)
		sessionID := ctx.ConnectionID
		groupID := "bench_group"
		svc.Server().JoinGroup(groupID, sessionID)
		if ctx.UserUUID != "" {
			svc.Server().JoinGroupForUser(ctx.UserUUID, groupID)
		}
		memberCount := svc.Server().GetGroupCount(groupID)
		tlog.Info(context.TODO(), "user joined sessionID=%s user=%s group=%s members=%d", sessionID, ctx.UserUUID, groupID, memberCount)
		return nil
	})

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	tlog.Info(context.TODO(), "logic_reliable started grpcPort=%s id=%s pushWorkers=%d windowSize=%d initialBackoff=%s",
		*port, *id, *pushWorkers, *windowSize, initialBackoff.String())

	payload := make([]byte, *pushSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	go func() {
		if *expectedMembers > 0 {
			for svc.Server().GetGroupCount("bench_group") < *expectedMembers {
				time.Sleep(time.Millisecond)
			}
			tlog.Info(context.TODO(), "logic_reliable all members joined, waiting 10s before pushing members=%d", *expectedMembers)
			time.Sleep(10 * time.Second)
			tlog.Info(context.TODO(), "logic_reliable push phase started members=%d", *expectedMembers)
		}
		workers := max(*pushWorkers, 1)
		jobs := make(chan string, workers*64)
		var workerWG sync.WaitGroup
		workerWG.Add(workers)
		for range workers {
			go func() {
				defer workerWG.Done()
				for sessionID := range jobs {
					err := svc.Server().PushReliable(sessionID, cmdPush, payload)
					switch {
					case err == nil:
						totalPushed.Add(1)
					case errors.Is(err, logic.ErrReliableWindowFull):
						// 背压（决策⑥）：客户端未及时 ACK 时窗口满，预期可发生，
						// 单独计数，不计入 pushed。
						windowFull.Add(1)
					default:
						// 会话已断等其余失败
					}
				}
			}()
		}
		defer workerWG.Wait()

		var membersCached []string
		var membersTS int64
		for {
			now := time.Now().UnixNano()
			if now-membersTS > 100*int64(time.Millisecond) || len(membersCached) == 0 {
				membersCached = svc.Server().GetGroupMembers("bench_group")
				membersTS = now
			}
			if len(membersCached) == 0 {
				// 空闲退避：无成员时避免 Gosched 忙等烧满整核。
				time.Sleep(time.Millisecond)
				continue
			}
			for _, sessionID := range membersCached {
				jobs <- sessionID
			}
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	start := time.Now()
	go func() {
		for range ticker.C {
			elapsed := time.Since(start).Seconds()
			snap := svc.Server().PushMetricsSnapshot()
			tlog.Info(context.TODO(),
				"logic_reliable progress elapsed=%.1f members=%d pushed=%d windowFull=%d "+
					"acked=%d retransmits=%d giveUps=%d droppedOffline=%d totalFailed=%d",
				elapsed, svc.Server().GetGroupCount("bench_group"), totalPushed.Load(), windowFull.Load(),
				snap["acked"], snap["retransmits"], snap["giveUps"], snap["droppedOffline"], snap["totalFailed"])
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	svc.Stop()
	snap := svc.Server().PushMetricsSnapshot()
	tlog.Info(context.TODO(),
		"logic_reliable stopped totalPushed=%d windowFull=%d acked=%d retransmits=%d giveUps=%d droppedOffline=%d",
		totalPushed.Load(), windowFull.Load(),
		snap["acked"], snap["retransmits"], snap["giveUps"], snap["droppedOffline"])
}
