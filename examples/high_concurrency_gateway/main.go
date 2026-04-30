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
					gnet.WithReadBufferCap(65536),
					gnet.WithWriteBufferCap(65536),
					gnet.WithSocketRecvBuffer(262144),
					gnet.WithSocketSendBuffer(262144),
				}
			} else {
				options = []gnet.Option{
					gnet.WithMulticore(true),
					gnet.WithReusePort(true),
					gnet.WithReadBufferCap(65536),
					gnet.WithWriteBufferCap(65536),
					gnet.WithSocketRecvBuffer(262144),
					gnet.WithSocketSendBuffer(262144),
				}
			}

			tlog.Info("性能优化选项:", "multicore", true, "reusePort", true,
				"tcpNoDelay", true, "readBuffer", 65536, "writeBuffer", 65536,
				"socketRecvBuffer", 262144, "socketSendBuffer", 262144)

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
