package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"

	"github.com/streasure/sgate/internal"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uperf"
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

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Printf("panic: %v\n%s\n", r, buf[:n])
		}
	}()

	logComp := tlog.NewLogComponent(*logConfig)
	if err := logComp.Init(); err != nil {
		fmt.Printf("failed to initialize tlog: %v\n", err)
		return
	}
	defer logComp.Destroy()

	cfg, err := config.Load(*confFiles)
	if err != nil {
		tlog.Error(context.TODO(), "load config failed error=%v", err)
		return
	}
	if err := cfg.Validate(); err != nil {
		tlog.Error(context.TODO(), "invalid gateway config error=%v", err)
		return
	}

	uperf.Apply(cfg.Perf.GcPercent, cfg.Perf.MemoryLimitPercent)

	tlog.Info(context.TODO(), "gateway starting... version=%s cpu=%d GOMAXPROCS=%d",
		internal.Version, runtime.NumCPU(), runtime.GOMAXPROCS(runtime.NumCPU()),
	)
	tlog.Info(context.TODO(), "config loaded port=%v", cfg.Port)

	container := component.NewContainer()
	gw := internal.NewGateway(*confFiles)
	for _, comp := range gw.Components() {
		container.Add(comp)
	}
	container.Serve()
}
