package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	gateway "github.com/streasure/sgate/internal"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
)

var (
	confFiles = flag.String("conf", "config/config.yaml", "config file path")
	showVer   = flag.Bool("version", false, "show version")
)

func main() {
	flag.Parse()
	if *showVer {
		fmt.Printf("sgate gateway version: %s\n", "1.0.0")
		return
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	if v := os.Getenv("GOGC"); v == "" {
		os.Setenv("GOGC", "200")
		debugSetGCPercent(200)
	}

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	logComp := tlog.NewLogComponent()
	if err := logComp.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
		os.Exit(1)
	}
	defer logComp.Destroy()

	tlog.Info("gateway starting...",
		"version", "1.0.0",
		"cpu", runtime.NumCPU(),
		"GOMAXPROCS", runtime.GOMAXPROCS(0),
	)

	cfg, err := config.LoadConfig(*confFiles)
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}
	tlog.Info("config loaded", "port", cfg.Port)

	gw := gateway.NewGateway(cfg)

	trnComp := gateway.NewTransportComponent(gw, cfg.Transports)
	if err := trnComp.Init(); err != nil {
		tlog.Error("transport init failed", "error", err)
		os.Exit(1)
	}

	gw.StartServices()
	trnComp.StartTransports()

	tlog.Info("all components started, waiting for signal...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("signal received, shutting down...")
	trnComp.Destroy()
	tlog.Info("gateway stopped")
}
