package routes

// 本文件定义网关路由常量、命令码及消息帧解析函数

import (
	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// MessageFrame 是公共消息帧协议类型别名。
type MessageFrame = protocol.MessageFrame

// LoginGateReq 是网关登录请求协议类型别名。
type LoginGateReq = protocol.LoginGateReq

// LoginGateAck 是网关登录响应协议类型别名。
type LoginGateAck = protocol.LoginGateAck

// ProtocolStreamData 是逻辑流数据协议类型别名。
type ProtocolStreamData = protocol.StreamData

// GatewayStreamClient 是网关流客户端接口别名。
type GatewayStreamClient = protocol.GatewayStreamClient

// GatewayStream_OnDataClient 是网关流客户端数据接口别名。
type GatewayStream_OnDataClient = protocol.GatewayStream_OnDataClient

// GatewayStreamServer 是网关流服务端接口别名。
type GatewayStreamServer = protocol.GatewayStreamServer

// GatewayStream_OnDataServer 是网关流服务端数据接口别名。
type GatewayStream_OnDataServer = protocol.GatewayStream_OnDataServer

// UnimplementedGatewayStreamServer 是未实现网关流服务端的嵌入式实现。
type UnimplementedGatewayStreamServer = protocol.UnimplementedGatewayStreamServer

// RegisterGatewayStreamServer 注册网关流服务到 gRPC 服务器
func RegisterGatewayStreamServer(s grpc.ServiceRegistrar, srv GatewayStreamServer) {
	protocol.RegisterGatewayStreamServer(s, srv)
}

// 系统命令码常量
const (
	CmdError         int32 = 3       // 错误命令
	CmdLoginGate     int32 = 1000001 // 网关登录请求
	CmdLoginGateAck  int32 = 1000002 // 网关登录响应
	CmdLogoutGate    int32 = 1000003 // 网关登出请求（断开当前连接，无业务推送）
	CmdLogoutGateAck int32 = 1000004 // 网关登出响应
	CmdLogicLoginReq int32 = 1100001 // 逻辑层登录请求
	CmdLogicLoginAck int32 = 1100002 // 逻辑层登录响应
	CmdKickNotify    int32 = 1100009 // 踢下线通知推送（先推后断，body 为 logic.KickNotify）
	CmdHeartbeatReq  int32 = 1100010 // 心跳请求
	CmdUserOffline   int32 = 1100012 // 用户下线通知
	CmdMsgAck        int32 = 1100013 // 可靠推送端到端 ACK（C→L，body 为 logic.MsgAck）
	CmdPushBatch     int32 = 9000002 // 批量推送命令
)

// UDP 通道命令号段。与 protocol/enums/cmd.proto 的 1,200,000-1,299,999 段一致
// （沿用 cmd.proto 既有 1,000,000 / 1,100,000 分段惯例取下一空段）。
// 号段是三端（客户端/网关/logic）共享的协议契约，定义为常量而非 yaml 配置：
// 改号段 = 改协议，必须走代码评审，避免实例间配置漂移。
const (
	CmdUDPRangeStart int32 = 1200000 // UDP 通道命令下界
	CmdUDPRangeEnd   int32 = 1299999 // 上界（1,300,000 及以上保留给 WS）
)

// 传输控制命令（落在 UDP 号段内，随关联通道走 UDP）。
const (
	CmdUdpAssociate    int32 = 1200000 // UDP 关联请求 C→S（body 为 UdpAssociateReq）
	CmdUdpAssociateAck int32 = 1200001 // UDP 关联应答 S→C（body 为 UdpAssociateAck）
	CmdUdpAction       int32 = 1200002 // UDP 上行动作 C→S（body 为 UdpAction）
	CmdUdpState        int32 = 1200003 // UDP 下行状态 S→C（body 为 UdpState）
	CmdUdpPing         int32 = 1200004 // UDP RTT 探测 C→S（WS 有队头阻塞，测出 RTT 不准）
	CmdUdpPong         int32 = 1200005 // UDP RTT 应答 S→C
)

// IsUDPCmd 判定命令是否走 UDP 通道（sgate 下行分流的唯一判据）。
// 必须用区间而非单边界：CmdPushBatch = 9000002 > 1299999，
// 单边界会把批量推送误判进 UDP。
// 规则：可丢的状态同步消息落在区间内，可靠消息永远 < CmdUDPRangeStart。
func IsUDPCmd(cmd int32) bool {
	return cmd >= CmdUDPRangeStart && cmd <= CmdUDPRangeEnd
}

// ExtractMessageFrame 从 protobuf 编码的数据中提取消息帧（命令码、序列号、消息体）
func ExtractMessageFrame(data []byte) (cmd int32, seqID int64, body []byte, ok bool) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, nil, false
		}
		data = data[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			cmd = int32(v)
			data = data[m:]
		case 2:
			if typ != protowire.VarintType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			seqID = int64(v)
			data = data[m:]
		case 99:
			if typ != protowire.BytesType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			body = v
			data = data[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, nil, false
			}
			data = data[m:]
		}
	}
	// body 允许为空（仅 cmd+seq 的控制消息）；必须有 cmd 才是合法消息帧
	return cmd, seqID, body, cmd != 0
}
