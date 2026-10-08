package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	logic "github.com/streasure/sgate/internal/logic"
	"github.com/streasure/sgate/internal/obs"
	"google.golang.org/protobuf/proto"
)

// main 启动logic1 WebSocket服务，注册gRPC服务并等待信号退出
func main() {
	port := flag.String("port", "50053", "gRPC listen port")
	id := flag.String("id", "logic1-ws", "service instance ID")
	shardCount := flag.Int("shardCount", 0, "streams dialed into each gateway (0=NumCPU*8)")
	logConfig := flag.String("config", "config/tlog.yaml", "log configuration")
	pprofAddr := flag.String("pprof", "", "pprof listen address (empty=off), e.g. 127.0.0.1:6062")
	batchUpstream := flag.Bool("batchUpstream", false, "StreamBatch framing (must match gateway stream.batchUpstream)")
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
		logic.WithBatchUpstream(*batchUpstream),
	)

	// 登录转发消息（LoginGateReq）的空处理器：仅确认送达，消除 unregistered cmd 告警。
	svc.RegisterProto(1000001, &protocol.LoginGateReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		return nil
	})

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("logic1_ws started, gRPC port=%s id=%s\n", *port, *id)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	svc.Stop()
	fmt.Println("logic1_ws stopped")
}
