package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	comp "github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/gateway"
	"github.com/streasure/sgate/internal/types"
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
		// CLI 版本输出：tlog 尚未初始化，属非日志输出（AGENTS 允许 fmt）
		fmt.Fprintf(os.Stdout, "sgate gateway version: %s\n", gateway.Version)
		return
	}

	logComp := tlog.NewLogComponent(*logConfig)
	if err := logComp.Init(); err != nil {
		// tlog 初始化失败时 tlog 不可用，只能走 stderr
		fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
		return
	}
	defer logComp.Destroy()

	cfg, err := config.Load(*confFiles)
	if err != nil {
		tlog.Error(context.TODO(), "load config failed error=%v", err)
		// 配置加载失败必须非零退出，避免进程管理器误判为正常退出
		logComp.Destroy()
		os.Exit(1)
	}

	tlog.Info(context.TODO(), "gateway starting http port:%d version=%s cpu=%d GOMAXPROCS=%d",
		cfg.HttpPort, gateway.Version, runtime.NumCPU(), runtime.GOMAXPROCS(runtime.NumCPU()),
	)

	// 创建全局过滤器链并加载 SPI 过滤器
	fc := types.InitFilterChain()
	for _, fi := range cfg.FilterChain.Filters {
		if err := fc.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn(context.TODO(), "failed to load filter from config name=%s error=%v", fi.Name, err)
		}
	}

	// 平铺创建所有生命周期组件，Container 由 main 统一创建和组装
	container := component.NewContainer()
	// uperf 组件 Order 最先：在任何业务组件分配大内存前应用 GC/内存软上限
	container.Add(uperf.New(uperf.Config{
		GcPercent:          cfg.Perf.GcPercent,
		MemoryLimitPercent: cfg.Perf.MemoryLimitPercent,
	}))
	container.Add(comp.NewSecurityComponent())
	container.Add(comp.NewObservabilityComponent())
	container.Add(comp.NewTrafficComponent())
	container.Add(comp.NewClusterComponent())
	container.Add(gateway.NewGateway())
	container.Serve()
}
