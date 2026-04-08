package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/gateway"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

func main() {
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

		// 启动gnet服务器
		go func(addr string) {
			if err := gnet.Run(gw, addr); err != nil {
				tlog.Error("启动服务器失败", "error", err, "addr", addr)
			}
		}(addr)
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