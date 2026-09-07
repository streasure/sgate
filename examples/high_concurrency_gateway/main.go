package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	gateway "github.com/streasure/sgate/internal"
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

	gw := gateway.NewGateway()

	gw.StartServices()

	tlog.Info("all components started, waiting for signal...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("gateway stopping...")
	tlog.Sync()
}
