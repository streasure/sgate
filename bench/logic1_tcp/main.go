package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/logic"
)

func main() {
	port := flag.String("port", "50052", "gRPC listen port")
	id := flag.String("id", "logic1-tcp", "service instance ID")
	logConfig := flag.String("config", "configs/log.yaml", "log configuration")
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

	if err := svc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("logic1_tcp started, gRPC port=%s id=%s\n", *port, *id)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	svc.Stop()
	fmt.Println("logic1_tcp stopped")
}
