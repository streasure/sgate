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
	// 输出系统信息
	tlog.Info("系统信息:", "CPU核心数", runtime.NumCPU(), "GOMAXPROCS", runtime.GOMAXPROCS(0))

	// 开始启动高并发网关服务
	tlog.Info("开始启动高并发网关服务...")

	// 加载配置
	tlog.Info("加载服务配置...")
	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("加载配置失败，使用默认配置", "error", err)
	}
	tlog.Info("配置加载成功:", "port", cfg.Port, "logLevel", cfg.LogLevel)

	// 输出支持的协议
	tlog.Info("支持的协议:")
	for _, proto := range cfg.Transports {
		tlog.Info("协议配置:", "protocol", proto.Protocol, "port", proto.Port, "type", proto.Type)
	}

	// 创建网关实例
	tlog.Info("创建网关实例...")
	gw := gateway.NewGateway()
	if gw == nil {
		tlog.Error("创建网关实例失败")
		return
	}
	tlog.Info("网关实例创建成功")

	// 启动服务器
	for _, proto := range cfg.Transports {
		addr := fmt.Sprintf("%s://:%d", proto.Protocol, proto.Port)
		tlog.Info("启动服务器:", "addr", addr, "type", proto.Type)

		// 设置传输类型
		gw.SetTransportType(fmt.Sprintf("%d", proto.Port), proto.Type)

		// 启动服务器
		tlog.Info("网关服务已启动:", "addr", addr, "type", proto.Type)
		tlog.Info("开始启动gnet服务器:", "addr", addr)

		// 启动gnet服务器，使用性能优化选项
		go func(addr string, protoType string) {
			// 根据协议类型选择优化选项
			var options []gnet.Option
			if protoType == "websocket" || proto.Protocol == "tcp" {
				// TCP 和 WebSocket 使用完整的性能优化
				options = []gnet.Option{
					// 启用多核模式，充分利用多核 CPU
					gnet.WithMulticore(true),
					// 启用端口复用，提高并发能力
					gnet.WithReusePort(true),
					// 禁用 Nagle 算法，降低延迟
					gnet.WithTCPNoDelay(gnet.TCPNoDelay),
					// 64KB 读取缓冲区，提高读取性能
					gnet.WithReadBufferCap(65536),
					// 64KB 写入缓冲区，提高写入性能
					gnet.WithWriteBufferCap(65536),
					// 256KB 系统接收缓冲区
					gnet.WithSocketRecvBuffer(262144),
					// 256KB 系统发送缓冲区
					gnet.WithSocketSendBuffer(262144),
				}
			} else {
				// UDP 使用无连接的优化
				options = []gnet.Option{
					// 启用多核模式
					gnet.WithMulticore(true),
					// 启用端口复用
					gnet.WithReusePort(true),
					// 64KB 读取缓冲区
					gnet.WithReadBufferCap(65536),
					// 64KB 写入缓冲区
					gnet.WithWriteBufferCap(65536),
					// 256KB 系统接收缓冲区
					gnet.WithSocketRecvBuffer(262144),
					// 256KB 系统发送缓冲区
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

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	// 关闭网关
	tlog.Info("关闭网关服务...")
	gw.Close()
	tlog.Info("网关服务已关闭")
}
