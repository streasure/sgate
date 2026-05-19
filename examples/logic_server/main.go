package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			tlog.New("../../config/tlog.yaml")
		}
	}

	svc := logic.NewService(
		logic.WithListenPort(getEnv("LOGIC_PORT", "50052")),
		logic.WithAdvertiseAddr(getEnv("LOGIC_ADVERTISE_ADDR", "")),
		logic.WithServiceID(getEnv("LOGIC_SERVICE_ID", "")),
		logic.WithServiceName(getEnv("LOGIC_SERVICE_NAME", "logic")),
		logic.WithRedisAddr(getEnv("REDIS_ADDR", "127.0.0.1:6379")),
		logic.WithRedisPassword(getEnv("REDIS_PASSWORD", "")),
		logic.WithHeartbeat(3*time.Second, 10*time.Second),
	)

	svc.RegisterRoute("ping", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "ping",
			Payload: map[string]string{
				"timestamp":       fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":         "Pong from logic server",
				"connectionCount": fmt.Sprintf("%d", svc.Server().GetConnectionCount()),
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("test", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "test",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":   "Test response from logic server",
				"data":      msg.GetPayload()["data"],
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("echo", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "echo",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":   "Echo from logic server",
				"echo":      msg.GetPayload()["message"],
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("getConnections", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "getConnections",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"count":     fmt.Sprintf("%d", svc.Server().GetConnectionCount()),
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	if err := svc.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "logic service failed: %v\n", err)
		os.Exit(1)
	}
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
