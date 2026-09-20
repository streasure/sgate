package gateway

// 本文件定义网关路由常量、命令码及消息帧解析函数

import (
	"hash/fnv"

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

// NewGatewayStreamClient 创建网关流客户端
var NewGatewayStreamClient = protocol.NewGatewayStreamClient

// RegisterGatewayStreamServer 注册网关流服务到 gRPC 服务器
func RegisterGatewayStreamServer(s grpc.ServiceRegistrar, srv GatewayStreamServer) {
	protocol.RegisterGatewayStreamServer(s, srv)
}

// 系统命令码常量
const (
	CmdLogin         int32 = 2       // 登录命令
	CmdError         int32 = 3       // 错误命令
	CmdLoginGate     int32 = 1000001 // 网关登录请求
	CmdLoginGateAck  int32 = 1000002 // 网关登录响应
	CmdLogicLoginReq int32 = 1100001 // 逻辑层登录请求
	CmdLogicLoginAck int32 = 1100002 // 逻辑层登录响应
	CmdHeartbeatReq  int32 = 1100010 // 心跳请求
	CmdHeartbeatAck  int32 = 1100011 // 心跳响应
	CmdUserOffline   int32 = 1100012 // 用户下线通知
	CmdPushBatch     int32 = 9000002 // 批量推送命令
)

// 路由常量定义
const (
	RouteLogin       = "login"          // 登录路由
	RouteLoginGate   = "login_gate"     // 网关登录路由
	RouteUserOffline = "user_offline"   // 用户下线路由
	RouteHeartbeat   = "heartbeat"      // 心跳路由
	RouteError       = "error"          // 错误路由

	RouteServerKick             = "server.kick"              // 踢下线命令
	RouteServerJoinGroup        = "server.join_group"         // 加入组
	RouteServerLeaveGroup       = "server.leave_group"        // 离开组
	RouteServerJoinGroupByUser  = "server.join_group_by_user" // 按用户加入组
	RouteServerLeaveGroupByUser = "server.leave_group_by_user"// 按用户离开组
	RouteServerCreateGroup      = "server.create_group"       // 创建组
	RouteServerDeleteGroup      = "server.delete_group"       // 删除组
	RouteServerSendToGroup      = "server.send_to_group"      // 向组发送消息
	RouteServerGetGroupInfo     = "server.get_group_info"     // 获取组信息
	RouteServerBroadcast        = "server.broadcast"          // 广播消息
	RouteServerSendToUser       = "server.send_to_user"       // 向用户发送消息

	RoutePing       = "ping"        // 心跳探测
	RoutePong       = "pong"        // 心跳响应
	RouteTest       = "test"        // 测试路由
	RouteTestResult = "testResult"  // 测试结果
	RouteEcho       = "echo"        // 回显路由

	RouteBatch = "_batch"           // 批量路由后缀
)

// CmdForMessage 根据路由和消息名生成命令码（FNV32a 哈希取正值）
func CmdForMessage(route, msgName string) int32 {
	h := fnv.New32a()
	h.Write([]byte(route))
	h.Write([]byte("."))
	h.Write([]byte(msgName))
	return int32(h.Sum32() & 0x7FFFFFFF)
}

// CmdForRoute 根据路由名获取对应的命令码
func CmdForRoute(route string) int32 {
	switch route {
	case RouteLogin:
		return CmdLogin
	case RouteError:
		return CmdError
	default:
		return CmdForMessage(route, "Message")
	}
}

// RouteForCmd 根据命令码反查路由名
func RouteForCmd(cmd int32) string {
	switch cmd {
	case CmdLogin:
		return RouteLogin
	case CmdError:
		return RouteError
	default:
		return ""
	}
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
	return cmd, seqID, body, cmd != 0 && len(body) > 0
}

// ExtractRouteAndCmd 从 protobuf 数据中提取路由名和命令码
func ExtractRouteAndCmd(data []byte) (route string, cmd int32) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return
		}
		data = data[n:]
		switch num {
		case 3:
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return
			}
			route = string(data[:m])
			data = data[m:]
		case 4:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return
			}
			cmd = int32(v)
			data = data[n:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return
			}
			data = data[m:]
		}
	}
	return
}
