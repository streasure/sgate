package main

import (
	"fmt"
	"log"

	"github.com/streasure/sgate/gateway"
)

func main() {
	// 创建基于 gnet 的高性能网关实例
	// 配置文件路径: config/config.yaml（可修改 config.go 中的默认路径）
	gw := gateway.NewGatewayGnet()

	// 设置 WebSocket 传输类型（端口 -> 传输类型）
	gw.SetTransportType("8084", "websocket")

	// 启动网关
	// 地址格式: tcp://0.0.0.0:8083 或 udp://0.0.0.0:8083
	addr := "tcp://0.0.0.0:8083"
	fmt.Printf("启动 SGate 网关服务，监听地址: %s\n", addr)
	fmt.Printf("性能目标: QPS 200,000+, P99 < 3ms\n")

	if err := gw.Start(addr); err != nil {
		log.Fatalf("启动网关失败: %v", err)
	}
}
