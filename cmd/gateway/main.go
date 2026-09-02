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
	"github.com/streasure/sgate/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

var (
	confFiles = flag.String("conf", "config/config.yaml", "config file path")
	showVer   = flag.Bool("version", false, "show version")
)

func main() {
	flag.Parse()
	if *showVer {
		fmt.Printf("sgate gateway version: %s\n", gateway.BuildVersion)
		return
	}

	// ── 1. Runtime 调优 ──────────────────────────────────────────────
	runtime.GOMAXPROCS(runtime.NumCPU())
	if v := os.Getenv("GOGC"); v == "" {
		os.Setenv("GOGC", "200")
		debugSetGCPercent(200)
	}

	// ── 2. 日志初始化 ────────────────────────────────────────────────
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
		"version", gateway.BuildVersion,
		"cpu", runtime.NumCPU(),
		"GOMAXPROCS", runtime.GOMAXPROCS(0),
	)

	// ── 3. 加载配置 ──────────────────────────────────────────────────
	cfg, err := config.LoadConfig(*confFiles)
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}
	tlog.Info("config loaded", "port", cfg.Port)

	// ── 4. 共享 FilterChain ─────────────────────────────────────────
	fc := types.NewFilterChain()

	// ── 5. 创建生命周期组件 ──────────────────────────────────────────
	secComp := gateway.NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
	obsComp := gateway.NewObservabilityComponent(cfg.OTelTracer, pprofAddrFromEnv(cfg.Monitoring.PprofAddr), fc)
	traComp := gateway.NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
	clsComp := gateway.NewClusterComponent(*cfg, cfg.GRPC.Port, nil)
	trnComp := gateway.NewTransportComponent(nil, cfg.Transports)

	components := []component.Component{secComp, obsComp, traComp, clsComp, trnComp}

	// ── 6. Init + Start 所有组件 ────────────────────────────────────
	for _, c := range components {
		if err := c.Init(); err != nil {
			tlog.Error("component init failed", "name", c.Name(), "error", err)
			os.Exit(1)
		}
	}
	for _, c := range components {
		if err := c.Start(); err != nil {
			tlog.Error("component start failed", "name", c.Name(), "error", err)
			os.Exit(1)
		}
	}

	// ── 7. 加载 SPI 过滤器 ──────────────────────────────────────────
	for _, fi := range cfg.FilterChain.Filters {
		if err := fc.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn("failed to load filter", "name", fi.Name, "error", err)
		}
	}

	// ── 8. 创建 Gateway（依赖注入）──────────────────────────────────
	gw := gateway.NewGatewayWithDeps(gateway.GatewayDeps{
		Config:             *cfg,
		FilterChain:        fc,
		LogSanitizer:       obsComp.LogSanitizer,
		WhitelistBlacklist: secComp.WhitelistBlacklist,
		WAF:                secComp.WAF,
		RateLimiter:        secComp.RateLimiter,
		JWTAuth:            secComp.JWTAuth,
		CircuitBreakerMgr:  secComp.CircuitBreakerMgr,
		Tracer:             obsComp.Tracer,
		OTelTracer:         obsComp.OTelTracer,
		LatencyTracker:     obsComp.LatencyTracker,
		CanaryFilter:       traComp.CanaryFilter,
		TrafficMirror:      traComp.TrafficMirror,
		Degradation:        traComp.Degradation,
		Discovery:          clsComp.Discovery,
		Balancer:           clsComp.Balancer,
		ConfigCenter:       clsComp.ConfigCenter,
		ClusterNode:        clsComp.Cluster,
		AlertWebhook:       clsComp.AlertWebhook,
	})

	// ── 9. 启动 Gateway 服务 ────────────────────────────────────────
	trnComp.SetGateway(gw)
	gw.StartServices()
	trnComp.StartTransports()

	tlog.Info("all components started, waiting for signal...")

	// ── 10. 优雅退出 ────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("signal received, shutting down...")

	// 反向销毁: Transport → Gateway → Components
	trnComp.Destroy()
	gw.Close()
	for i := len(components) - 1; i >= 0; i-- {
		components[i].Destroy()
	}

	tlog.Info("gateway stopped")
}

func pprofAddrFromEnv(defaultAddr string) string {
	if addr := os.Getenv("SGATE_PPROF_ADDR"); addr != "" {
		return addr
	}
	return defaultAddr
}
