package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

func main() {
	// tlog 负责自动创建日志目录（基于 exe 所在目录解析相对路径）；
	// 初始化失败意味着日志不可用，直接退出避免静默丢失错误信息。
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
				os.Exit(1)
			}
		}
	}

	if addr := os.Getenv("LOGIC_PPROF_ADDR"); addr != "" {
		go http.ListenAndServe(addr, nil)
	}
	svc := logic.NewService(
		logic.WithServiceID(envOr("LOGIC_SERVICE_ID", "logic-1")),
		logic.WithAdvertiseAddr(envOr("LOGIC_ADVERTISE_ADDR", "localhost:50052")),
		logic.WithListenPort(envOr("LOGIC_PORT", "50052")),
		logic.WithNacosEndpoint(envOr("NACOS_ENDPOINT", "")),
		logic.WithNacosNamingEndpoint(envOr("NACOS_NAMING_ENDPOINT", "")),
		logic.WithNacosNamespace(envOr("NACOS_NAMESPACE", "public")),
		logic.WithNacosGroup(envOr("NACOS_GROUP", "DEFAULT_GROUP")),
		logic.WithNacosAuth(envOr("NACOS_USERNAME", "nacos"), envOr("NACOS_PASSWORD", "nacos")),
		logic.WithNacosAPIVersion(envOr("NACOS_API_VERSION", "v3")),
		logic.WithServiceName(envOr("LOGIC_SERVICE_NAME", "logic")),
		logic.WithGRPCWindowSize(envInt("GRPC_WINDOW_SIZE", 67108864)),
		logic.WithGRPCMaxMessageSize(envInt("GRPC_MAX_MSG_SIZE", 4194304)),
		logic.WithStreamSendChSize(envInt("LOGIC_STREAM_CH_SIZE", 1048576)),
		logic.WithDispatchWorkerCount(envInt("LOGIC_DISPATCH_WORKERS", 24)),
		logic.WithPassthrough(envBool("LOGIC_PASSTHROUGH", false)),
	)

	svc.RegisterRoute(protobuf.RoutePing, func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RoutePong,
			Payload:      map[string]string{"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterBurstRoute(protobuf.RouteTest, func(msg *protobuf.Message, push func(*protobuf.Message)) {
		push(&protobuf.Message{
			Route:     protobuf.RouteTestResult,
			Timestamp: time.Now().UnixMilli(),
		})
	})

	svc.RegisterRoute(protobuf.RouteLogin, func(msg *protobuf.Message) *protobuf.Message {
		userID := msg.GetPayload()["userId"]
		userUUID := "uuid_" + userID
		// 注册 userUUID → connectionID 映射，使 Broadcast/PushToGroup 能正确工作
		svc.RegisterUser(userUUID, msg.ConnectionId)
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			UserUuid:     userUUID,
			Route:        protobuf.RouteLogin,
			Payload:      map[string]string{"code": "200", "message": "ok", "userId": userID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute(protobuf.RouteEcho, func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RouteEcho,
			Payload:      map[string]string{"echo": msg.GetPayload()["message"], "timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("test_broadcast", func(msg *protobuf.Message) *protobuf.Message {
		svc.Server().Broadcast(&protobuf.Message{
			Route: "broadcast_event",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
			},
		})
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "test_broadcast_ack",
			Payload:      map[string]string{"code": "200", "message": "broadcast sent"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("test_send_to_user", func(msg *protobuf.Message) *protobuf.Message {
		targetUUID := msg.GetPayload()["targetUUID"]
		connID, ok := svc.Server().GetConnectionIDByUser(targetUUID)
		if !ok {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "test_send_to_user_ack",
				Payload:      map[string]string{"code": "404", "message": "target user not found"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}
		svc.Server().PushToConnection(connID, &protobuf.Message{
			Route: "direct_message",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
			},
		})
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "test_send_to_user_ack",
			Payload:      map[string]string{"code": "200", "message": "sent"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	if err := svc.Run(); err != nil {
		tlog.Error("logic service failed", "error", err)
		tlog.Sync()
		os.Exit(1)
	}
	tlog.Sync()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return def
}
