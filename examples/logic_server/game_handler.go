package main

import (
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

func init() {
	RegisterDispatcherBuilder(func() *logic.Dispatcher {
		return logic.NewDispatcher(protobuf.RouteGame).
			HandleFromProto(&protobuf.LoginReq{}, handleGameLogin).
			HandleFromProto(&protobuf.LogoutReq{}, handleGameLogout)
	})
}

func handleGameLogin(ctx *logic.Context, req proto.Message) proto.Message {
	loginReq := req.(*protobuf.LoginReq)
	now := time.Now().UnixMilli()
	return &protobuf.LoginAck{
		User: &protobuf.UserInfo{
			Uuid:        loginReq.UserId,
			NickName:    "Player_" + loginReq.UserId,
			CurAvatarId: 0,
		},
		ServerTime:     now,
		Version:        "1.0.0",
		OpenServerTime: now - 86400000,
		ServerStage:    1,
	}
}

func handleGameLogout(ctx *logic.Context, req proto.Message) proto.Message {
	logoutReq := req.(*protobuf.LogoutReq)
	return &protobuf.LogoutAck{
		UserId:  logoutReq.UserId,
		Code:    200,
		Message: "logout success",
	}
}
