package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	protocol "github.com/streasure/protocol/gateway"
	logicproto "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/logic"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

func main() {
	port := flag.String("port", "50070", "gRPC listen port")
	id := flag.String("id", "logic-logincheck", "service instance ID")
	logConfig := flag.String("config", "../logincheck/config/log.yaml", "log configuration")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	svc := logic.NewService(
		logic.WithListenPort(*port),
		logic.WithServiceID(*id),
		logic.WithServiceName("logic"),
		logic.WithServerType("Logic"),
		logic.WithZone("default"),
		logic.WithEtcd("http://127.0.0.1:2379"),
	)
	svc.RegisterProto(1000001, &protocol.LoginGateReq{}, 0, func(ctx *logic.Context, req proto.Message) proto.Message {
		tlog.Info(context.TODO(), "login gate received accountId=%s sessionID=%s", ctx.UserUUID, ctx.ConnectionID)
		return nil
	})
	svc.RegisterProto(1100001, &logicproto.LoginReq{}, 1100002, func(ctx *logic.Context, req proto.Message) proto.Message {
		login := req.(*logicproto.LoginReq)
		ack := &logicproto.LoginAck{ServerTime: 1, Version: "logincheck", UserKey: ctx.UserUUID}
		tlog.Info(context.TODO(), "login request received userId=%s sessionID=%s", login.UserId, ctx.ConnectionID)
		return ack
	})
	if err := svc.Start(); err != nil {
		tlog.Error(context.TODO(), "start logincheck logic failed error=%v", err)
		return
	}
	tlog.Info(context.TODO(), "logincheck logic started port=%s id=%s", *port, *id)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	svc.Stop()
}
