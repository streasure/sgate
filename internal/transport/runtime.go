package transport

import (
	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/dataplane"
)

// Runtime 是传输平面（transport）定义、由业务平面实现的消费方接口。
// transport 只负责连接接入、帧编解码与协议状态机；连接注册、准入计数、
// 限流/过载、消息管道、登录登出等业务动作全部经本接口回掉业务侧
// （gateway → 后续的 dataplane.Engine），保证依赖方向 transport → dataplane 单向。
type Runtime interface {
	// GetProtection 返回保护参数热读快照（帧长/缓冲上限等热路径配置）。
	GetProtection() config.ProtectionConfig

	// CanAcceptConnection 连接准入检查（连接数/单 IP 上限）；
	// 超限时由实现方记日志并返回 false，transport 直接关闭新连接。
	CanAcceptConnection(remoteIP string) bool

	// RegisterConnection 注册新连接并返回连接 ID；
	// wsConn 非 nil 表示 WebSocket 连接（实现方负责绑定上下文并登记 WS 连接集合）。
	RegisterConnection(c gnet.Conn, wsConn *dataplane.WebSocketConnection) (connectionID string)

	// OnConnectionOpened 连接建立计数（total/active）。
	OnConnectionOpened()

	// OnConnectionClosed 连接关闭收尾：生命周期指标、离线通知、UDP 关联释放、
	// 管理器移除；wsConn 非 nil 时从 WS 连接集合摘除并置关闭态；
	// counted 表示 OnOpen 已计数需递减活跃计数；connectionID 可能为空（未注册）。
	OnConnectionClosed(connectionID string, wsConn *dataplane.WebSocketConnection, counted bool, err error)

	// OnWSClosed WebSocket 关闭帧的业务摘除：离线通知、拆 UDP 关联、
	// 移除管理器/WS 集合、关闭底层连接。
	OnWSClosed(wsConn *dataplane.WebSocketConnection)

	// ProxyProtocolEnabled 是否启用 PROXY protocol v1 首包解析（热读配置）。
	ProxyProtocolEnabled() bool

	// IsTrustedProxy 对端是否为可信反代（WS 握手解析 XFF/X-Real-IP 用）。
	IsTrustedProxy(host string) bool

	// UpdateConnectionIP 更新连接真实客户端 IP，返回是否发生变更。
	UpdateConnectionIP(connectionID, ip string) bool

	// HandleUDPDatagram 处理单个 UDP 数据报（gnet OnTraffic 的 UDP 分支）。
	HandleUDPDatagram(c gnet.Conn, data []byte) gnet.Action

	// HandleTCPFrame 处理一条完整 TCP 逻辑帧（业务消息管道入口，事件循环内调用）。
	HandleTCPFrame(c gnet.Conn, connectionID string, frame []byte) gnet.Action

	// HandleWSData 处理一条 WebSocket 数据帧载荷；返回错误由 transport 断连。
	HandleWSData(wsConn *dataplane.WebSocketConnection, payload []byte) error

	// OnTick 每秒采样回调（消息速率 / 指标日志），由 transport.OnTick 驱动。
	OnTick()

	// Shutdown 触发业务整体关闭流程（关闭握手的首个触发者：主动 Close 或 OnShutdown）。
	Shutdown()
}

// 编译期校验：数据面引擎实现本接口（gateway 壳经嵌入提升复用同一实现）。
var _ Runtime = (*dataplane.Engine)(nil)
