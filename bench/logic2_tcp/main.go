package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/logic"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

const cmdPush int32 = 9000001

// main 启动logic2 TCP服务，注册登录处理并启动推送工作协程
func main() {
	port := flag.String("port", "50060", "gRPC listen port")
	id := flag.String("id", "logic2-tcp", "service instance ID")
	pushInterval := flag.Duration("push-interval", 1*time.Millisecond, "interval between group pushes")
	pushSize := flag.Int("push-size", 64, "payload size in bytes")
	pushWorkers := flag.Int("push-workers", runtime.NumCPU(), "parallel group push workers")
	expectedMembers := flag.Int("expected-members", 0, "wait for this many logged-in group members before pushing")
	logConfig := flag.String("config", "configs/logic2_tcp_log.yaml", "log configuration")
	flag.Parse()
	defer logutil.Init(*logConfig)()
	svc := logic.NewService(
		logic.WithListenPort(*port),
		logic.WithServiceID(*id),
		logic.WithServiceName("logic"),
		logic.WithServerType("Logic"),
		logic.WithZone("default"),
		logic.WithEtcd("http://127.0.0.1:2379"),
	)

	var totalPushed atomic.Int64

	// 注册登录请求处理函数，将用户加入测试组
	svc.RegisterProto(1000001, &protocol.LoginGateReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		_ = req.(*protocol.LoginGateReq)
		sessionID := ctx.ConnectionID
		groupID := "bench_group"

		svc.Server().JoinGroup(groupID, sessionID)
		if ctx.UserUUID != "" {
			svc.Server().JoinGroupForUser(ctx.UserUUID, groupID)
		}
		memberCount := svc.Server().GetGroupCount(groupID)
		tlog.Info(context.Background(), "user joined sessionID=%s user=%s group=%s members=%d", sessionID, ctx.UserUUID, groupID, memberCount)
		return nil
	})

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	tlog.Info(context.Background(), "logic2 started transport=tcp grpcPort=%s id=%s pushWorkers=%d", *port, *id, *pushWorkers)

	payload := make([]byte, *pushSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	// 启动推送工作协程，向组内所有成员发送消息
	go func() {
		if *expectedMembers > 0 {
			for svc.Server().GetGroupCount("bench_group") < *expectedMembers {
				time.Sleep(time.Millisecond)
			}
			tlog.Info(context.Background(), "logic2 push phase started transport=tcp members=%d", *expectedMembers)
		}
		workers := *pushWorkers
		if workers < 1 {
			workers = 1
		}
		jobs := make(chan string, workers*4)
		var workerWG sync.WaitGroup
		workerWG.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer workerWG.Done()
				for sessionID := range jobs {
					if svc.Server().PushToConnection(sessionID, cmdPush, payload) == nil {
						totalPushed.Add(1)
					}
				}
			}()
		}
		defer workerWG.Wait()
		for {
			members := svc.Server().GetGroupMembers("bench_group")
			if len(members) == 0 {
				time.Sleep(*pushInterval)
				continue
			}
			for _, sessionID := range members {
				jobs <- sessionID
			}
			time.Sleep(*pushInterval)
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	start := time.Now()
	go func() {
		for range ticker.C {
			elapsed := time.Since(start).Seconds()
			pushed := totalPushed.Load()
			members := svc.Server().GetGroupCount("bench_group")
			rate := float64(pushed) / elapsed
			tlog.Info(context.Background(), "logic2 progress transport=tcp members=%d pushed=%d rate=%.0f", members, pushed, rate)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	svc.Stop()
	tlog.Info(context.Background(), "logic2 stopped transport=tcp totalPushed=%d", totalPushed.Load())
}
