package main

import (
	"fmt"
	"log"

	"github.com/streasure/sgate/gateway"
)

func main() {
	// 创建基于 gnet 的网关实例
	gw := gateway.NewGatewayGnet()

	// 设置WebSocket传输类型（如果需要）
	gw.SetTransportType("8084", "websocket")

	// 启动网关
	addr := "0.0.0.0:8083"
	fmt.Printf("启动网关服务，监听地址: %s\n", addr)

	if err := gw.Start(addr); err != nil {
		log.Fatalf("启动网关失败: %v", err)
	}
}
