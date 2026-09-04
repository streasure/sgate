//go:build ignore

// Package main runs a cmd-only logic service for the sgate examples.
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	enums "github.com/streasure/protocol/enums"
	logicproto "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/logic"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

func main() {
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
				os.Exit(1)
			}
		}
	}
	if addr := os.Getenv("LOGIC_PPROF_ADDR"); addr != "" {
		go func() { _ = http.ListenAndServe(addr, nil) }()
	}

	cfg, err := logic.LoadConfig("config/logic.yaml")
	if err != nil {
		cfg, _ = logic.LoadConfig("../../config/logic.yaml")
	}
	svc := logic.NewService(
		logic.WithConfig(cfg),
		logic.WithServiceID(envOr("LOGIC_SERVICE_ID", "logic-1")),
		logic.WithAdvertiseAddr(envOr("LOGIC_ADVERTISE_ADDR", "localhost:50052")),
		logic.WithListenPort(envOr("LOGIC_PORT", "50052")),
		logic.WithStreamSendChSize(envInt("LOGIC_STREAM_CH_SIZE", 1048576)),
	)
	registerHandlers(svc)
	if err := svc.Run(); err != nil {
		tlog.Error("logic service failed", "error", err)
		os.Exit(1)
	}
}

func registerHandlers(svc *logic.Service) {
	svc.RegisterProto(int32(enums.Cmd_CMD_LOGIN_REQ), &logicproto.LoginReq{}, int32(enums.Cmd_CMD_LOGIN_ACK), func(ctx *logic.Context, req proto.Message) proto.Message {
		login := req.(*logicproto.LoginReq)
		userID := login.GetUserId()
		if userID == "" {
			userID = ctx.ConnectionID
		}
		userKey := "user_" + userID
		svc.RegisterUser(userKey, ctx.ConnectionID)
		return &logicproto.LoginAck{UserKey: userKey, ServerTime: time.Now().UnixMilli()}
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_HEARTBEAT_REQ), &logicproto.HeartbeatReq{}, int32(enums.Cmd_CMD_HEARTBEAT_ACK), func(_ *logic.Context, req proto.Message) proto.Message {
		heartbeat := req.(*logicproto.HeartbeatReq)
		now := time.Now().UnixMilli()
		return &logicproto.HeartbeatAck{ServerTime: now, RttMs: now - heartbeat.GetClientTime()}
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_USER_OFFLINE_NTF), &logicproto.UserOfflineNtf{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		offline := req.(*logicproto.UserOfflineNtf)
		ctx.Server.Offline(offline.GetSessionId(), offline.GetUserKey())
		return nil
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_JOIN_GROUP_REQ), &logicproto.JoinGroupReq{}, int32(enums.Cmd_CMD_JOIN_GROUP_ACK), func(ctx *logic.Context, req proto.Message) proto.Message {
		join := req.(*logicproto.JoinGroupReq)
		return &logicproto.JoinGroupAck{Code: 0, GroupId: join.GetGroupId(), MemberCount: int32(ctx.Server.JoinGroup(join.GetGroupId(), ctx.ConnectionID))}
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_LEAVE_GROUP_REQ), &logicproto.LeaveGroupReq{}, int32(enums.Cmd_CMD_LEAVE_GROUP_ACK), func(ctx *logic.Context, req proto.Message) proto.Message {
		leave := req.(*logicproto.LeaveGroupReq)
		return &logicproto.LeaveGroupAck{Code: 0, GroupId: leave.GetGroupId(), MemberCount: int32(ctx.Server.LeaveGroup(leave.GetGroupId(), ctx.ConnectionID))}
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_SEND_TO_GROUP_REQ), &logicproto.SendToGroupReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		push := req.(*logicproto.SendToGroupReq)
		ctx.Server.SendToGroup(push.GetGroupId(), push.GetTargetCmd(), push.GetData())
		return nil
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_BROADCAST_REQ), &logicproto.BroadcastReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		push := req.(*logicproto.BroadcastReq)
		ctx.Server.Broadcast(push.GetTargetCmd(), push.GetData())
		return nil
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_SEND_TO_USER_REQ), &logicproto.SendToUserReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		push := req.(*logicproto.SendToUserReq)
		ctx.Server.SendToUser(push.GetUserKey(), push.GetTargetCmd(), push.GetData())
		return nil
	})

	svc.RegisterProto(int32(enums.Cmd_CMD_KICK_NTF), &logicproto.KickNtf{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		kick := req.(*logicproto.KickNtf)
		if kick.GetSessionId() != "" {
			ctx.Server.Kick(kick.GetSessionId(), mustMarshal(kick))
		}
		return nil
	})
}

func mustMarshal(message proto.Message) []byte {
	data, _ := proto.Marshal(message)
	return data
}

func envOr(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}

func envInt(key string, def int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return def
}
