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

// IsUDPCmd 判定命令是否走 UDP 通道（sgate 下行分流的唯一判据）。
// 必须用区间而非单边界：CmdPushBatch = 9000002 > 1299999，
// 单边界会把批量推送误判进 UDP。
// 规则：可丢的状态同步消息落在区间内，可靠消息永远 < CmdUDPRangeStart。
// 号段上下界定义在 protocol/gateway/cmds.go（三端共享协议契约）。
func IsUDPCmd(cmd int32) bool {
	return cmd >= protocol.CmdUDPRangeStart && cmd <= protocol.CmdUDPRangeEnd
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
