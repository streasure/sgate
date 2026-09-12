package main

import (
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

func main() {
	port := flag.String("port", "50061", "gRPC listen port")
	id := flag.String("id", "logic2-ws", "service instance ID")
	pushInterval := flag.Duration("push-interval", 100*time.Millisecond, "interval between group pushes")
	pushSize := flag.Int("push-size", 64, "payload size in bytes")
	pushWorkers := flag.Int("push-workers", runtime.NumCPU(), "parallel group push workers")
	expectedMembers := flag.Int("expected-members", 0, "wait for this many logged-in group members before pushing")
	logConfig := flag.String("config", "configs/logic2_ws_log.yaml", "log configuration")
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

	svc.RegisterProto(1000001, &protocol.LoginGateReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		_ = req.(*protocol.LoginGateReq)
		sessionID := ctx.ConnectionID
		groupID := "bench_group"

		svc.Server().JoinGroup(groupID, sessionID)
		if ctx.UserUUID != "" {
			svc.Server().JoinGroupForUser(ctx.UserUUID, groupID)
		}
		memberCount := svc.Server().GetGroupCount(groupID)
		tlog.Info("user joined", "sessionID", sessionID, "user", ctx.UserUUID, "group", groupID, "members", memberCount)
		return nil
	})

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	tlog.Info("logic2 started", "transport", "websocket", "grpcPort", *port, "id", *id, "pushWorkers", *pushWorkers)

	payload := make([]byte, *pushSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	go func() {
		if *expectedMembers > 0 {
			for svc.Server().GetGroupCount("bench_group") < *expectedMembers {
				time.Sleep(time.Millisecond)
			}
			tlog.Info("logic2 push phase started", "transport", "websocket", "members", *expectedMembers)
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
			tlog.Info("logic2 progress", "transport", "websocket", "members", members, "pushed", pushed, "rate", rate)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	svc.Stop()
	tlog.Info("logic2 stopped", "transport", "websocket", "totalPushed", totalPushed.Load())
}
