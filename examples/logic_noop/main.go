package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type noopLogic struct {
	protoGw.UnimplementedGatewayStreamServer
}

// OnData is a raw sink for forward-only gateway benchmarks. It receives and
// discards every StreamData without decoding payloads, logging, or responding.
func (noopLogic) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

func main() {
	address := ":50052"
	if value := os.Getenv("LOGIC_ADDR"); value != "" {
		address = value
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logic_noop listen failed: %v\n", err)
		os.Exit(1)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(8*1024*1024),
		grpc.InitialWindowSize(64*1024*1024),
		grpc.InitialConnWindowSize(64*1024*1024),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	protoGw.RegisterGatewayStreamServer(server, noopLogic{})
	go func() { _ = server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	server.GracefulStop()
}
