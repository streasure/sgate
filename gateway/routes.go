package gateway

import (
	"hash/fnv"
	"strings"

	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

type MessageFrame = protocol.MessageFrame
type StreamData = protocol.StreamData
type GatewayStreamClient = protocol.GatewayStreamClient
type GatewayStream_OnDataClient = protocol.GatewayStream_OnDataClient
type GatewayStreamServer = protocol.GatewayStreamServer
type GatewayStream_OnDataServer = protocol.GatewayStream_OnDataServer
type UnimplementedGatewayStreamServer = protocol.UnimplementedGatewayStreamServer

var NewGatewayStreamClient = protocol.NewGatewayStreamClient

func RegisterGatewayStreamServer(s grpc.ServiceRegistrar, srv GatewayStreamServer) {
	protocol.RegisterGatewayStreamServer(s, srv)
}

const (
	CmdLogin int32 = 2
	CmdError int32 = 3
)

const (
	RouteLogin = "login"
	RouteError = "error"

	RouteServerKick             = "server.kick"
	RouteServerJoinGroup        = "server.join_group"
	RouteServerLeaveGroup       = "server.leave_group"
	RouteServerJoinGroupByUser  = "server.join_group_by_user"
	RouteServerLeaveGroupByUser = "server.leave_group_by_user"
	RouteServerCreateGroup      = "server.create_group"
	RouteServerDeleteGroup      = "server.delete_group"
	RouteServerSendToGroup      = "server.send_to_group"
	RouteServerGetGroupInfo     = "server.get_group_info"
	RouteServerBroadcast        = "server.broadcast"
	RouteServerSendToUser       = "server.send_to_user"

	RoutePing       = "ping"
	RoutePong       = "pong"
	RouteTest       = "test"
	RouteTestResult = "testResult"
	RouteEcho       = "echo"

	RouteBatch = "_batch"
)

func CmdForMessage(route, msgName string) int32 {
	h := fnv.New32a()
	h.Write([]byte(route))
	h.Write([]byte("."))
	h.Write([]byte(msgName))
	return int32(h.Sum32() & 0x7FFFFFFF)
}

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

func ExtractRouteAndCmd(data []byte) (route string, cmd int32) {
	offset := 0
	for offset < len(data) {
		b := data[offset]
		if b < 0x80 {
			offset++
			fieldNum := int(b >> 3)
			wireType := int(b & 0x7)

			switch wireType {
			case 0:
				if fieldNum == 4 {
					v, n := decodeVarintFast(data[offset:])
					if n > 0 {
						cmd = int32(v)
					}
				}
				for offset < len(data) && data[offset] >= 0x80 {
					offset++
				}
				if offset < len(data) {
					offset++
				}
			case 1:
				offset += 8
			case 2:
				if offset >= len(data) {
					return
				}
				l := int(data[offset])
				offset++
				if l >= 0x80 {
					if offset >= len(data) {
						return
					}
					l2 := int(data[offset])
					offset++
					l = (l & 0x7F) | (l2 << 7)
				}
				if fieldNum == 3 {
					if offset+l <= len(data) {
						route = string(data[offset : offset+l])
					}
				}
				offset += l
			case 5:
				offset += 4
			default:
				return
			}
		} else {
			offset++
		}
	}
	return
}

func ExtractRouteCmdAndConnID(data []byte) (route string, cmd int32, connID string) {
	offset := 0
	for offset < len(data) {
		b := data[offset]
		if b < 0x80 {
			offset++
			fieldNum := int(b >> 3)
			wireType := int(b & 0x7)

			switch wireType {
			case 0:
				if fieldNum == 4 {
					v, n := decodeVarintFast(data[offset:])
					if n > 0 {
						cmd = int32(v)
					}
				}
				for offset < len(data) && data[offset] >= 0x80 {
					offset++
				}
				if offset < len(data) {
					offset++
				}
			case 1:
				offset += 8
			case 2:
				if offset >= len(data) {
					return
				}
				l := int(data[offset])
				offset++
				if l >= 0x80 {
					if offset >= len(data) {
						return
					}
					l2 := int(data[offset])
					offset++
					l = (l & 0x7F) | (l2 << 7)
				}
				if fieldNum == 1 {
					if offset+l <= len(data) {
						connID = string(data[offset : offset+l])
					}
				} else if fieldNum == 3 {
					if offset+l <= len(data) {
						route = string(data[offset : offset+l])
					}
				}
				offset += l
			case 5:
				offset += 4
			default:
				return
			}
		} else {
			offset++
		}
	}
	return
}

func ExtractRouteFast(data []byte) string {
	route, _ := ExtractRouteAndCmd(data)
	return route
}

func decodeVarintFast(data []byte) (uint64, int) {
	var result uint64
	var shift uint
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		result |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return result, i + 1
		}
		shift += 7
	}
	return 0, 0
}

func RespNameForReq(reqName string) string {
	if strings.HasSuffix(reqName, "Req") {
		return reqName[:len(reqName)-3] + "Ack"
	}
	return reqName + "Ack"
}

func CmdFromProto(route string, msgName string) (cmd int32, respCmd int32) {
	cmd = CmdForMessage(route, msgName)
	respCmd = CmdForMessage(route, RespNameForReq(msgName))
	return
}
