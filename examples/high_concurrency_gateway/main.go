package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/streasure/util/component"
	"github.com/streasure/sgate/gateway"
	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
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

	// tlog init
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
				os.Exit(1)
			}
		}
	}

	tlog.Info("system info", "cpu", runtime.NumCPU(), "GOMAXPROCS", runtime.GOMAXPROCS(0))
	tlog.Info("loading config...")
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}
	tlog.Info("config loaded", "port", cfg.Port, "logLevel", cfg.LogLevel)

	for _, proto := range cfg.Transports {
		tlog.Info("transport configured", "protocol", proto.Protocol, "port", proto.Port, "type", proto.Type)
	}

	// Shared FilterChain
	fc := types.NewFilterChain()

	// Create all lifecycle components
	secComp := gateway.NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
	obsComp := gateway.NewObservabilityComponent(cfg.OTelTracer, pprofAddrFromEnv(), fc)
	traComp := gateway.NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
	clsComp := gateway.NewClusterComponent(*cfg, cfg.GRPC.Port, nil)
	trnComp := gateway.NewTransportComponent(nil, cfg.Transports)

	// Init + Start all components
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

	// Load SPI filters from config
	for _, fi := range cfg.FilterChain.Filters {
		if err := fc.LoadByName(fi.Name, fi.Config); err != nil {
			tlog.Warn("failed to load filter", "name", fi.Name, "error", err)
		}
	}

	// Create Gateway via dependency injection
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

	trnComp.SetGateway(gw)
	gw.StartServices()

	tlog.Info("starting gateway components...")
	trnComp.StartTransports()

	tlog.Info("all components started, waiting for signal...")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	tlog.Info("signal received, shutting down...")
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
