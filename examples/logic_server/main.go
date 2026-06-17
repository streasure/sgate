package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/streasure/sgate/logic"
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

	ApplyAllHandlers(svc)
	prebuildResponses()

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
