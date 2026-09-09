package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type noopLogic struct {
	protoGw.UnimplementedGatewayStreamServer
}

// OnData decouples recv and send: recv loop pushes into a buffered channel,
// a sender goroutine drains it and calls stream.Send. This avoids blocking
// the recv loop on gRPC transport back-pressure.
func (noopLogic) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	ch := make(chan *protoGw.StreamData, 1<<20) // 1M buffer
	done := make(chan struct{})

	go func() {
		defer close(done)
		for msg := range ch {
			_ = stream.Send(msg)
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			close(ch)
			<-done
			return err
		}
		select {
		case ch <- msg:
		default:
			// buffer full, drop under back-pressure
		}
	}
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

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
