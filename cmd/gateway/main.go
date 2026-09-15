package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/streasure/sgate/internal"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/gc"
	"github.com/streasure/util/tlog"
)

var (
	confFiles = flag.String("conf", "config/config.yaml", "config file path")
	logConfig = flag.String("logger", "config/log.yaml", "log configuration file")
	showVer   = flag.Bool("version", false, "show version")
)

// main 启动网关服务，初始化日志、加载配置、启动所有组件并等待信号退出
func main() {
	flag.Parse()
	if *showVer {
		fmt.Printf("sgate gateway version: %s\n", internal.Version)
		return
	}

	gc.InitGCTuning(200)

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	logComp := tlog.NewLogComponent(*logConfig)
	if err := logComp.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
		return
	}
	defer logComp.Destroy()

	tlog.Info("gateway starting...",
		"version", internal.Version,
		"cpu", runtime.NumCPU(),
		"GOMAXPROCS", runtime.GOMAXPROCS(runtime.NumCPU()),
	)

	cfg, err := config.LoadConfig(*confFiles)
	if err != nil {
		tlog.Error("load config failed error", err)
		return
	}
	if err := cfg.Validate(); err != nil {
		tlog.Error("invalid gateway config error", err)
		return
	}
	tlog.Info("config loaded", "port", cfg.Port)

	gw := internal.NewGateway(*confFiles)

	gw.StartServices()

	tlog.Info("all components started, waiting for signal...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("gateway stopping...")
}
