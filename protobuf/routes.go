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
	RouteLogin             = "login"
	RoutePing              = "ping"
	RoutePong              = "pong"
	RouteTest              = "test"
	RouteTestResult        = "testResult"
	RouteVersion           = "version"
	RouteGetConnections    = "getConnections"
	RouteBroadcast         = "broadcast"
	RouteHealth            = "health"
	RouteAPIDocs           = "api-docs"
	RouteError             = "error"
	RouteEcho              = "echo"
	RouteMessage           = "message"
	RouteKick              = "kick"
	RouteTimeout           = "timeout"
	RouteHandshakeResponse = "handshake_response"
	RouteQueueTest         = "queueTest"
	RouteAddWhitelist      = "addWhitelist"
	RouteRemoveWhitelist   = "removeWhitelist"
	RouteGetWhitelist      = "getWhitelist"
	RouteAddBlacklist      = "addBlacklist"
	RouteRemoveBlacklist   = "removeBlacklist"
	RouteGetBlacklist      = "getBlacklist"

	RouteServerKick             = "server.kick"
	RouteServerJoinGroup        = "server.join_group"
	RouteServerLeaveGroup       = "server.leave_group"
	RouteServerJoinGroupByUser  = "server.join_group_by_user"
	RouteServerLeaveGroupByUser = "server.leave_group_by_user"
	RouteServerCreateGroup      = "server.create_group"
	RouteServerDeleteGroup      = "server.delete_group"
	RouteServerSendToGroup      = "server.send_to_group"
	RouteServerGetGroupInfo     = "server.get_group_info"
	RouteServerPlayerOnline     = "server.playerOnline"
	RouteServerPlayerOffline    = "server.playerOffline"
	RouteServerPlayerMoved      = "server.playerMoved"
	RouteServerChat             = "server.chat"
	RouteServerPush             = "server.push"
	RouteServerAnnouncement     = "server.announcement"
	RouteServerAnnounce         = "server.announce"
	RouteServerRoomPlayerJoined = "server.room.playerJoined"
	RouteServerRoomPlayerLeft   = "server.room.playerLeft"
	RouteServerTeamMemberJoined = "server.team.memberJoined"
	RouteServerTeamMemberLeft   = "server.team.memberLeft"
	RouteServerDamageNotify     = "server.damageNotify"
	RouteServerAttackBroadcast  = "server.attackBroadcast"

	RoutePlayerLogin       = "player.login"
	RoutePlayerHeartbeat   = "player.heartbeat"
	RoutePlayerMove        = "player.move"
	RoutePlayerChat        = "player.chat"
	RoutePlayerAttack      = "player.attack"
	RoutePlayerUseItem     = "player.useItem"
	RoutePlayerQueryStatus = "player.queryStatus"
	RoutePlayerQueryOnline = "player.queryOnline"

	RouteRoomJoin  = "room.join"
	RouteRoomLeave = "room.leave"
	RouteRoomInfo  = "room.info"

	RouteTeamCreate = "team.create"
	RouteTeamJoin   = "team.join"
	RouteTeamLeave  = "team.leave"
	RouteTeamInfo   = "team.info"

	RouteGame = "game"
)
