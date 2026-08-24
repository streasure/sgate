package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/gateway"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	// GC 调优：千万级吞吐下减少 STW
	//   GOGC=200 — 提高 GC 触发阈值，降低 GC 频率（默认 100）
	//   GOMEMLIMIT — Go 1.19+ 软内存上限，建议在容器启动时通过环境变量设置：
	//     GOMEMLIMIT=4GiB ./high_concurrency_gateway
	//   （runtime 在启动时读环境变量；main 内设置已太晚）
	if v := os.Getenv("GOGC"); v == "" {
		os.Setenv("GOGC", "200")
		debugSetGCPercent(200)
	} else {
		debugSetGCPercent(parseIntDefault(v, 100))
	}
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		tlog.Info("GOMEMLIMIT from env", "value", v)
	}

	setProcessPriorityHigh()

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	// 启动 pprof server（独立 goroutine，默认 :6060）
	// 监控：goroutine 数量、heap 泄漏、schedlatency、CPU 热点
	if addr := os.Getenv("SGATE_PPROF_ADDR"); addr != "" {
		gateway.StartPProfServer(addr)
	} else {
		gateway.StartPProfServer(":6060")
	}

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Println("failed to initialize tlog:", err)
			}
		}
	}

	tlog.Info("系统信息:", "CPU核心数", runtime.NumCPU(), "GOMAXPROCS", runtime.GOMAXPROCS(0))
	tlog.Info("开始启动高并发网关服务...")

	tlog.Info("加载服务配置...")
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("加载配置失败，使用默认配置", "error", err)
	}
	tlog.Info("配置加载成功:", "port", cfg.Port, "logLevel", cfg.LogLevel)

	tlog.Info("支持的协议:")
	for _, proto := range cfg.Transports {
		tlog.Info("协议配置:", "protocol", proto.Protocol, "port", proto.Port, "type", proto.Type)
	}

	tlog.Info("创建网关实例...")
	gw := gateway.NewGateway()
	if gw == nil {
		tlog.Error("创建网关实例失败")
		tlog.Sync()
		return
	}
	tlog.Info("网关实例创建成功")

	for _, proto := range cfg.Transports {
		addr := fmt.Sprintf("%s://:%d", proto.Protocol, proto.Port)
		tlog.Info("启动服务器:", "addr", addr, "type", proto.Type)

		gw.SetTransportType(fmt.Sprintf("%d", proto.Port), proto.Type)

		tlog.Info("网关服务已启动:", "addr", addr, "type", proto.Type)
		tlog.Info("开始启动gnet服务器:", "addr", addr)

		go func(addr string, protoType string) {
			var options []gnet.Option
			if protoType == "websocket" || proto.Protocol == "tcp" {
				options = []gnet.Option{
					gnet.WithMulticore(true),
					gnet.WithReusePort(true),
					gnet.WithTCPNoDelay(gnet.TCPNoDelay),
					gnet.WithReadBufferCap(262144),             // 256KB — more data per OnTraffic = bigger batches
					gnet.WithWriteBufferCap(262144),            // 256KB — larger write queue for reverse push
					gnet.WithSocketRecvBuffer(4 * 1024 * 1024), // 4MB kernel recv buffer
					gnet.WithSocketSendBuffer(4 * 1024 * 1024), // 4MB kernel send buffer
				}
			} else {
				options = []gnet.Option{
					gnet.WithMulticore(true),
					gnet.WithReusePort(true),
					gnet.WithReadBufferCap(262144),
					gnet.WithWriteBufferCap(262144),
					gnet.WithSocketRecvBuffer(4 * 1024 * 1024),
					gnet.WithSocketSendBuffer(4 * 1024 * 1024),
				}
			}

			tlog.Info("性能优化选项:", "multicore", true, "reusePort", true,
				"tcpNoDelay", true, "readBuffer", 262144, "writeBuffer", 262144,
				"socketRecvBuffer", 4*1024*1024, "socketSendBuffer", 4*1024*1024)

			if err := gnet.Run(gw, addr, options...); err != nil {
				tlog.Error("启动服务器失败", "error", err, "addr", addr)
			}
		}(addr, proto.Type)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	tlog.Info("收到退出信号，开始关闭...", "signal", sig.String())
	gw.Close()
	tlog.Info("网关服务已关闭")
	tlog.Sync()
}
