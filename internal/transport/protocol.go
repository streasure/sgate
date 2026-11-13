package transport

import (
	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
)

// Protocol 协议插件接口：封装单个传输协议的启动逻辑（TCP/WebSocket/UDP）。
// 插件在 init() 中注册，gateway 启动时遍历注册表统一启动所有协议监听器。
type Protocol interface {
	// Name 返回协议名称（tcp/websocket/udp），用于配置匹配和日志。
	Name() string

	// Start 启动协议监听器（阻塞调用，失败返回 error）。
	// 参数：
	//   - handler: gnet.EventHandler 实现（gateway.Gateway）
	//   - transport: 单个传输配置（端口/类型/选项）
	//   - cfg: 全局配置（TLS/安全/性能参数）
	// 调用方需在 goroutine 中启动（gnet.Run 阻塞至引擎停止）。
	Start(handler gnet.EventHandler, transport config.Transport, cfg *config.Config) error
}
