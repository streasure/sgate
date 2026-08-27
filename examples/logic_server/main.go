package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
)

func main() {
	if addr := os.Getenv("LOGIC_PPROF_ADDR"); addr != "" {
		go http.ListenAndServe(addr, nil)
	}
	svc := logic.NewService(
		logic.WithServiceID(envOr("LOGIC_SERVICE_ID", "logic-1")),
		logic.WithAdvertiseAddr(envOr("LOGIC_ADVERTISE_ADDR", "localhost:50052")),
		logic.WithListenPort(envOr("LOGIC_PORT", "50052")),
		// Nacos 注册默认关闭（仅监控 sgate 网关）；需要注册 logic 时设置 NACOS_ENDPOINT/NACOS_NAMING_ENDPOINT
		logic.WithNacosEndpoint(envOr("NACOS_ENDPOINT", "")),
		logic.WithNacosNamingEndpoint(envOr("NACOS_NAMING_ENDPOINT", "")),
		logic.WithNacosNamespace(envOr("NACOS_NAMESPACE", "public")),
		logic.WithNacosGroup(envOr("NACOS_GROUP", "DEFAULT_GROUP")),
		logic.WithNacosAuth(envOr("NACOS_USERNAME", "nacos"), envOr("NACOS_PASSWORD", "nacos")),
		logic.WithNacosAPIVersion(envOr("NACOS_API_VERSION", "v3")),
		logic.WithServiceName(envOr("LOGIC_SERVICE_NAME", "logic")),
		logic.WithGRPCWindowSize(envInt("GRPC_WINDOW_SIZE", 67108864)),
		logic.WithGRPCMaxMessageSize(envInt("GRPC_MAX_MSG_SIZE", 4194304)),
		logic.WithStreamSendChSize(envInt("LOGIC_STREAM_CH_SIZE", 65536)),
		// 分发 worker 数：默认 NumCPU*128，高并发下过多 worker 会加剧
		// 调度与 sync.Pool 竞争，可通过 LOGIC_DISPATCH_WORKERS 调优
		logic.WithDispatchWorkerCount(envInt("LOGIC_DISPATCH_WORKERS", 0)),
		// 默认走路由分发，确保 BURST_COUNT 反向推送处理器在双向压测中生效。
		// 需要纯透传吞吐时仍可显式设置 LOGIC_PASSTHROUGH=true。
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

	// 反向链路压测：每条 RouteTest 触发 burstCount 条推送回客户端
	// 正向流量极低（仅触发），反向流量被放大 burstCount 倍
	// 避免正向与反向在同一 gRPC 双向流上竞争流控窗口
	// 默认放大反向响应，配合批量发送可覆盖千万级双向吞吐压测；
	// 生产环境可通过 BURST_COUNT 调整为业务所需值。
	burstCount := envInt("BURST_COUNT", 2000)
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

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return def
}
