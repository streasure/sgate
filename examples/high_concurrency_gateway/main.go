// Package main demonstrates a production-ready sgate gateway startup.
//
// Architecture:
//
//	Client (TCP/UDP/WS) ──→ sgate(:48080) ──gRPC──→ Logic Server(:50052)
//	                       sgate(:50051) ←─gRPC──── Logic Server(:50052)
//
// Features:
//   - TCP/UDP/WebSocket multi-protocol接入
//   - gRPC bidirectional streaming with logic server
//   - Nacos service discovery & config center (optional)
//   - IP whitelist/blacklist, rate limiting, circuit breaker
//   - Prometheus metrics, health checks, distributed tracing
//   - Write coalescing + batch flush for high throughput
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

	"github.com/streasure/sgate/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/types"
	"github.com/streasure/util/component"
	tlog "github.com/streasure/treasure-slog"
)

func main() {
	// ── 1. Runtime 调优 ──────────────────────────────────────────────
	runtime.GOMAXPROCS(runtime.NumCPU())

	// GOGC: 控制 GC 频率，默认 100（每分配等量内存触发一次 GC）。
	// 高吞吐场景设 200 减少 GC 频率，降低 STW 对 QPS 的影响。
	if v := os.Getenv("GOGC"); v == "" {
		os.Setenv("GOGC", "200")
		debugSetGCPercent(200)
	} else {
		debugSetGCPercent(parseIntDefault(v, 100))
	}

	// GOMEMLIMIT: Go 1.19+ 软内存上限，防止 heap 无限增长导致 OOM。
	// 格式: "4GiB" / "2048MiB" / "512KiB" / "2G" / "2048M"
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		if applied := applyGOMEMLIMIT(); applied > 0 {
			tlog.Info("GOMEMLIMIT applied", "env", v, "bytes", applied)
		} else {
			tlog.Info("GOMEMLIMIT parse failed", "env", v)
		}
	}

	// Windows: 提升进程优先级到 HIGH_PRIORITY_CLASS
	setProcessPriorityHigh()

	// ── 2. 日志初始化 ────────────────────────────────────────────────
	// tlog 自动创建日志目录（基于 exe 所在目录解析相对路径）
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

	// ── 3. 加载配置 ──────────────────────────────────────────────────
	// config.yaml 仅保留高频调整字段；未列出的字段用 defaults.go 常量填充
	tlog.Info("loading config...")
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}
	tlog.Info("config loaded", "port", cfg.Port, "logLevel", cfg.LogLevel)

	for _, proto := range cfg.Transports {
		tlog.Info("transport configured", "protocol", proto.Protocol, "port", proto.Port, "type", proto.Type)
	}

	// ── 4. 共享 FilterChain ─────────────────────────────────────────
	// 所有组件共享同一个 FilterChain，按 Security→Observability→Traffic 顺序执行
	fc := types.NewFilterChain()

	// ── 5. 创建生命周期组件 ──────────────────────────────────────────
	// 各组件实现 component.Component 接口 (Init/Start/Destroy)
	secComp := gateway.NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
	obsComp := gateway.NewObservabilityComponent(cfg.OTelTracer, pprofAddrFromEnv(), fc)
	traComp := gateway.NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
	clsComp := gateway.NewClusterComponent(*cfg, cfg.GRPC.Port, nil)
	trnComp := gateway.NewTransportComponent(nil, cfg.Transports)

	// ── 6. Init + Start 所有组件 ────────────────────────────────────
	for _, c := range []component.Component{secComp, obsComp, traComp, clsComp} {
		if err := c.Init(); err != nil {
			tlog.Error("component init failed", "name", c.Name(), "error", err)
			os.Exit(1)
		}
	}
	for _, c := range []component.Component{secComp, obsComp, traComp, clsComp} {
		if err := c.Start(); err != nil {
			tlog.Error("component start failed", "name", c.Name(), "error", err)
			os.Exit(1)
		}
	}

	// ── 7. 加载 SPI 过滤器 ──────────────────────────────────────────
	// 从 config.yaml 的 filterChain.filters 加载业务过滤器
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
	// StartServices: 连接 logic server，启动健康检查、连接检查等
	trnComp.SetGateway(gw)
	gw.StartServices()

	// StartTransports: 启动 TCP/UDP/WebSocket 监听
	tlog.Info("starting gateway components...")
	trnComp.StartTransports()

	// ── 10. 监听端口 ────────────────────────────────────────────────
	tlog.Info("all components started, waiting for signal...")
	tlog.Info("endpoints",
		"tcp", fmt.Sprintf(":%d", cfg.Transports[0].Port),
		"grpc", fmt.Sprintf(":%d", cfg.GRPC.Port),
		"health", fmt.Sprintf(":%d", cfg.Port),
	)

	// ── 11. 优雅退出 ────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("signal received, shutting down...")

	// 按顺序关闭: Transport → Gateway → Components（反向销毁）
	trnComp.Destroy()
	gw.Close()
	for i := len([]component.Component{clsComp, traComp, obsComp, secComp}) - 1; i >= 0; i-- {
		comps := []component.Component{secComp, obsComp, traComp, clsComp}
		comps[i].Destroy()
	}

	tlog.Info("gateway stopped")
	tlog.Sync()
}

func pprofAddrFromEnv() string {
	if addr := os.Getenv("SGATE_PPROF_ADDR"); addr != "" {
		return addr
	}
	return ":6060"
}
