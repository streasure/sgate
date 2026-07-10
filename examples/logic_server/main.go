package main

import (
	"os"
	"strconv"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
)

func main() {
	svc := logic.NewService(
		logic.WithServiceID(envOr("LOGIC_SERVICE_ID", "logic-1")),
		logic.WithAdvertiseAddr(envOr("LOGIC_ADVERTISE_ADDR", "localhost:50052")),
		logic.WithListenPort(envOr("LOGIC_PORT", "50052")),
		logic.WithRedisAddr(envOr("REDIS_ADDR", "127.0.0.1:6379")),
		logic.WithGRPCWindowSize(envInt("GRPC_WINDOW_SIZE", 67108864)),
		logic.WithGRPCMaxMessageSize(envInt("GRPC_MAX_MSG_SIZE", 4194304)),
		logic.WithStreamSendChSize(envInt("LOGIC_STREAM_CH_SIZE", 65536)),
	)

	svc.RegisterRoute(protobuf.RoutePing, func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RoutePong,
			Payload:      map[string]string{"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// 反向链路压测：每条 RouteTest 触发 burstCount 条推送回客户端
	// 正向流量极低（仅触发），反向流量被放大 burstCount 倍
	// 避免正向与反向在同一 gRPC 双向流上竞争流控窗口
	burstCount := envInt("BURST_COUNT", 1000)
	svc.RegisterBurstRoute(protobuf.RouteTest, func(msg *protobuf.Message, push func(*protobuf.Message)) {
		ts := time.Now().UnixMilli()
		for i := 0; i < burstCount; i++ {
			push(&protobuf.Message{
				Route:     protobuf.RouteTestResult,
				Timestamp: ts,
			})
		}
	})

	svc.RegisterRoute(protobuf.RouteLogin, func(msg *protobuf.Message) *protobuf.Message {
		userID := msg.GetPayload()["userId"]
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			UserUuid:     "uuid_" + userID,
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

	if err := svc.Run(); err != nil {
		panic(err)
	}
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
