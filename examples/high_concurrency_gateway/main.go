// Package main demonstrates a production-ready sgate gateway startup.
//
// Architecture:
//
//	Client (TCP/UDP/WS) ──→ sgate(:48080) ──gRPC──→ Logic Server(:50052)
//	                       sgate(:48080) ←─gRPC──── Logic Server(:50052)
//
// Run:
//
//	go build -o gw.exe .
//	./gw.exe
//
// Config: config/config.yaml
package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	gateway "github.com/streasure/sgate/internal"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	if v := os.Getenv("GOGC"); v == "" {
		os.Setenv("GOGC", "200")
		debugSetGCPercent(200)
	} else {
		debugSetGCPercent(parseIntDefault(v, 100))
	}

	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		if applied := applyGOMEMLIMIT(); applied > 0 {
			tlog.Info("GOMEMLIMIT applied", "env", v, "bytes", applied)
		} else {
			tlog.Info("GOMEMLIMIT parse failed", "env", v)
		}
	}

	setProcessPriorityHigh()

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
				os.Exit(1)
			}
		}
	}

	tlog.Info("system info", "cpu", runtime.NumCPU(), "GOMAXPROCS", runtime.GOMAXPROCS(0))

	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}
	tlog.Info("config loaded", "port", cfg.Port, "logLevel", cfg.LogLevel)

	gw := gateway.NewGateway(cfg)

	trnComp := gateway.NewTransportComponent(gw, cfg.Transports)
	if err := trnComp.Init(); err != nil {
		tlog.Error("transport init failed", "error", err)
		os.Exit(1)
	}

	gw.StartServices()
	trnComp.StartTransports()

	tlog.Info("all components started, waiting for signal...")
	tlog.Info("endpoints",
		"tcp", fmt.Sprintf(":%d", cfg.Transports[0].Port),
		"grpc", fmt.Sprintf(":%d", cfg.GRPC.Port),
		"health", fmt.Sprintf(":%d", cfg.Port),
	)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("signal received, shutting down...")
	trnComp.Destroy()
	tlog.Info("gateway stopped")
	tlog.Sync()
}
