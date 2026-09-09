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
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/tlog"
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

	serverID := "logic-1"
	if v := os.Getenv("LOGIC_SERVER_ID"); v != "" {
		serverID = v
	}

	etcdEndpoint := "http://127.0.0.1:2379"
	if v := os.Getenv("ETCD_ENDPOINT"); v != "" {
		etcdEndpoint = v
	}

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			tlog.New("")
		}
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

	// Register to etcd for gateway discovery
	registry := etcd.New(etcd.ComponentConfig{
		Enabled: true,
		Etcd:    etcd.Config{Endpoints: []string{etcdEndpoint}, ServicePrefix: "/services"},
		Registration: etcd.RegistrationConfig{
			Enabled:   true,
			ServiceID: "Logic:default",
			InstanceID: serverID,
			Address:   address,
			LeaseTTL:  "10s",
		},
	})
	if err := registry.Start(); err != nil {
		tlog.Warn("etcd registration failed, running without discovery", "error", err)
		registry = nil
	} else {
		tlog.Info("registered to etcd", "serviceID", "Logic:default", "instanceID", serverID, "address", address)
	}

	go func() {
		if err := server.Serve(listener); err != nil {
			tlog.Error("gRPC server stopped", "error", err)
		}
	}()

	tlog.Info("logic noop server started", "addr", address)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	if registry != nil {
		registry.Destroy()
	}
	server.GracefulStop()
}
