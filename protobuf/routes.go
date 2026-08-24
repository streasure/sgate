package protobuf

import (
	"hash/fnv"
	"strings"

	"google.golang.org/protobuf/proto"
)

func CmdForMessage(route, msgName string) int32 {
	h := fnv.New32a()
	h.Write([]byte(route))
	h.Write([]byte("."))
	h.Write([]byte(msgName))
	return int32(h.Sum32() & 0x7FFFFFFF)
}

// ExtractRouteAndCmd performs a lightweight protobuf field scan to extract
// route (field 3, string) and cmd (field 4, varint) from a serialized Message
// without full proto.Unmarshal. This avoids allocating strings/maps for fields
// that are not needed for dispatch (Payload, UserUuid, etc.).
//
// Used by:
//   - Gateway: quick handshake check on first frame (handleBatchTraffic)
//   - Logic server: RouteBatch handler (avoid full Unmarshal per frame at 20M QPS)
//
// Returns empty route and zero cmd if the data is malformed.
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

// ExtractRouteFast returns only the route field from a serialized Message.
// Even lighter than ExtractRouteAndCmd (stops after finding field 3).
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

func CmdFromProto(route string, msg proto.Message) (cmd int32, respCmd int32) {
	name := proto.MessageName(msg)
	parts := strings.Split(string(name), ".")
	msgName := parts[len(parts)-1]
	cmd = CmdForMessage(route, msgName)
	respCmd = CmdForMessage(route, RespNameForReq(msgName))
	return
}

const (
	RouteHandshake         = "handshake"
	RouteHandshakeResponse = "handshake_response"
	RouteLogin             = "login"
	RouteError             = "error"

	RouteServerKick             = "server.kick"
	RouteServerJoinGroup        = "server.join_group"
	RouteServerLeaveGroup       = "server.leave_group"
	RouteServerJoinGroupByUser  = "server.join_group_by_user"
	RouteServerLeaveGroupByUser = "server.leave_group_by_user"
	RouteServerCreateGroup      = "server.create_group"
	RouteServerDeleteGroup      = "server.delete_group"
	RouteServerSendToGroup      = "server.send_to_group"
	RouteServerGetGroupInfo     = "server.get_group_info"

	RoutePing               = "ping"
	RoutePong               = "pong"
	RouteTest               = "test"
	RouteTestResult         = "testResult"
	RouteEcho               = "echo"
	RouteGetConnections     = "getConnections"
	RouteGame               = "game"
	RouteServerPush         = "server.push"
	RouteServerAnnouncement = "server.announcement"
	RouteServerAnnounce     = "server.announce"
	RouteServerChat         = "server.chat"

	// RouteBatch 是反向链路（logic->sgate）批量消息的伪路由。
	// logic 将多条已序列化的 Message 以长度前缀方式打包进一个 Message.Data，
	// 通过单次 stream.Send 发送，sgate 收到后解包逐条分发，降低 gRPC 调用开销。
	RouteBatch = "_batch"
)

const (
	CmdPushNotify   int32 = int32(Cmd_CMD_PUSH_NOTIFY)
	CmdAnnouncement int32 = int32(Cmd_CMD_ANNOUNCEMENT)
	CmdChatMsg      int32 = int32(Cmd_CMD_CHAT_MSG)
	CmdKickNotify   int32 = int32(Cmd_CMD_KICK_NOTIFY)
)
