// Package main demonstrates a production-ready logic server with sgate.
//
// Architecture:
//
//	Client ──TCP──▶ sgate(:48080) ──gRPC──▶ logic(:50052)
//
// Push patterns (logic → sgate → client):
//
//  1. Personal push:  PushToConnection(connID, msg)        // push to specific connection
//  2. Personal push:  PushToServer + server.send_to_user    // push by userUUID
//  3. Personal push:  burst route push callback             // push to requesting client (most efficient)
//  4. Group push:     JoinGroupByUser + SendToGroup         // push to group members
//  5. Broadcast:      Broadcast(msg)                        // push to all connected clients
//
// Run:
//
//	go build -o logic.exe .
//	./logic.exe
//
// Environment variables:
//
//	LOGIC_SERVICE_ID        logic-1           Service instance ID
//	LOGIC_ADVERTISE_ADDR    localhost:50052   Advertise address
//	LOGIC_PORT              50052             gRPC listen port
//	NACOS_ENDPOINT          ""                Nacos console URL
//	NACOS_NAMESPACE         public            Nacos namespace
//	GRPC_WINDOW_SIZE        67108864          gRPC window size
//	GRPC_MAX_MSG_SIZE       4194304           Max gRPC message size
//	LOGIC_STREAM_CH_SIZE    1048576           Stream send channel size
//	LOGIC_DISPATCH_WORKERS  24                Consumer goroutine count
//	LOGIC_PASSTHROUGH       false             Passthrough mode
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/streasure/protocol/commonstruct"
	"github.com/streasure/protocol/sgate"
	"github.com/streasure/sgate/logic"
	"github.com/streasure/util/tlog"
)

func main() {
	// ── 1. 日志初始化 ────────────────────────────────────────────────
	// tlog 自动创建日志目录（基于 exe 所在目录解析相对路径）
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize tlog: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// ── 2. pprof 可选 ────────────────────────────────────────────────
	if addr := os.Getenv("LOGIC_PPROF_ADDR"); addr != "" {
		go http.ListenAndServe(addr, nil)
	}

	// ── 3. 创建 Logic Service ───────────────────────────────────────
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

	// ══════════════════════════════════════════════════════════════════
	// 路由注册
	// ══════════════════════════════════════════════════════════════════

	// ── Ping/Pong: 基准心跳 ─────────────────────────────────────────
	// 客户端发 "ping"，服务端回 "pong"，用于连通性检测和基准压测
	svc.RegisterRoute(sgate.RoutePing, func(msg *commonstruct.Message) *commonstruct.Message {
		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        sgate.RoutePong,
			Payload:      map[string]string{"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ── BurstRoute: 每次请求触发推送（duplex 压测用）────────────────
	// push 回调直接写入 gRPC stream，是最高效的推送路径
	svc.RegisterBurstRoute(sgate.RouteTest, func(msg *commonstruct.Message, push func(*commonstruct.Message)) {
		push(&commonstruct.Message{
			Route:     sgate.RouteTestResult,
			Timestamp: time.Now().UnixMilli(),
		})
	})

	// ── Login: 用户登录（必须） ──────────────────────────────────────
	// 注册 userUUID → connectionID 映射，使所有推送功能可用
	svc.RegisterRoute(sgate.RouteLogin, func(msg *commonstruct.Message) *commonstruct.Message {
		userID := msg.GetPayload()["userId"]
		if userID == "" {
			userID = msg.ConnectionId
		}
		userUUID := "uuid_" + userID

		// ★ 关键: RegisterUser 注册映射，否则 PushToConnection/PushToGroup 不工作
		svc.RegisterUser(userUUID, msg.ConnectionId)

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			UserUuid:     userUUID,
			Route:        sgate.RouteLogin,
			Payload:      map[string]string{"code": "200", "userId": userID, "userUUID": userUUID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ── Echo: 回显测试 ──────────────────────────────────────────────
	svc.RegisterRoute(sgate.RouteEcho, func(msg *commonstruct.Message) *commonstruct.Message {
		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        sgate.RouteEcho,
			Payload: map[string]string{
				"echo":      msg.GetPayload()["message"],
				"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10),
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 1: 个人推送（push to self）
	// ══════════════════════════════════════════════════════════════════
	// 使用 burst route 的 push 回调，最高效路径（直接写 stream）
	// Flow: client → sgate → logic(push callback) → sgate → client
	svc.RegisterBurstRoute("push_me", func(msg *commonstruct.Message, push func(*commonstruct.Message)) {
		push(&commonstruct.Message{
			Route: "personal_notification",
			Payload: map[string]string{
				"type":    "personal_push",
				"message": "Hello from logic server!",
				"from":    "server",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 2: 个人推送（push to another user）
	// ══════════════════════════════════════════════════════════════════
	// 使用 PushToServer + server.send_to_user 路由
	// Flow: client_A → sgate → logic(PushToServer) → sgate.gateway.SendToUser → client_B
	svc.RegisterRoute("send_msg", func(msg *commonstruct.Message) *commonstruct.Message {
		targetUUID := msg.GetPayload()["targetUUID"]
		if targetUUID == "" {
			return &commonstruct.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "send_msg_ack",
				Payload:      map[string]string{"code": "400", "message": "missing targetUUID"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		svc.Server().PushToServer(&commonstruct.Message{
			Route: sgate.RouteServerSendToUser,
			Payload: map[string]string{
				"userUUID": targetUUID,
				"route":    "direct_message",
				"message":  msg.GetPayload()["message"],
				"from":     msg.UserUuid,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "send_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "delivered"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 3: 组管理（join/leave）
	// ══════════════════════════════════════════════════════════════════
	// 组是 Gateway 本地的，每个 Gateway 维护自己的组成员列表
	// JoinGroup/LeaveGroup 通过 PushToServer 发送到 Gateway，异步生效

	svc.RegisterRoute("join_group", func(msg *commonstruct.Message) *commonstruct.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		// server.join_group: Gateway 根据 ConnectionId 查找连接，自动加入组
		svc.Server().PushToServer(&commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        sgate.RouteServerJoinGroup,
			Payload:      map[string]string{"groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		})

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "join_group_ack",
			Payload:      map[string]string{"code": "200", "groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("leave_group", func(msg *commonstruct.Message) *commonstruct.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		svc.Server().PushToServer(&commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        sgate.RouteServerLeaveGroup,
			Payload:      map[string]string{"groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		})

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "leave_group_ack",
			Payload:      map[string]string{"code": "200"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 4: 组推送（group push）
	// ══════════════════════════════════════════════════════════════════
	// SendToGroup 路由到 Gateway 的 ConnectionManager.SendToGroup
	// 组成员需先通过 server.join_group 加入
	// Flow: client → sgate → logic → sgate.gateway.SendToGroup → all group members
	svc.RegisterRoute("group_msg", func(msg *commonstruct.Message) *commonstruct.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		svc.Server().SendToGroup(groupID, &commonstruct.Message{
			Route: "group_broadcast",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
				"groupID": groupID,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "group_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "sent to group"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 5: 全服广播（broadcast）
	// ══════════════════════════════════════════════════════════════════
	// Broadcast 推送到所有连接的客户端
	// Flow: client → sgate → logic(Broadcast) → sgate.gateway.Broadcast → all clients
	svc.RegisterRoute("broadcast_msg", func(msg *commonstruct.Message) *commonstruct.Message {
		svc.Server().Broadcast(&commonstruct.Message{
			Route: "global_broadcast",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &commonstruct.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "broadcast_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "broadcast sent"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 推送模式 6: 批量推送（burst pattern）
	// ══════════════════════════════════════════════════════════════════
	// 高吞吐场景：每次请求推送多条消息（如游戏状态同步）
	// burst route 的 push 回调可多次调用，每条消息独立序列化
	svc.RegisterBurstRoute("batch_push", func(msg *commonstruct.Message, push func(*commonstruct.Message)) {
		targetCount := 1
		if n, err := strconv.Atoi(msg.GetPayload()["count"]); err == nil && n > 0 {
			targetCount = n
		}
		for i := 0; i < targetCount; i++ {
			push(&commonstruct.Message{
				Route: "batch_push_item",
				Payload: map[string]string{
					"index": strconv.Itoa(i),
					"total": strconv.Itoa(targetCount),
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}
	})

	// ══════════════════════════════════════════════════════════════════
	// 启动服务
	// ══════════════════════════════════════════════════════════════════
	if err := svc.Run(); err != nil {
		tlog.Error("logic service failed", "error", err)
		tlog.Sync()
		os.Exit(1)
	}
	tlog.Sync()
}

// ══════════════════════════════════════════════════════════════════════
// API Reference (logic.Server methods):
//
// Personal push:
//
//	svc.Server().PushToConnection(connID, msg)       // push to specific connection
//	svc.Server().GetConnectionIDByUser(userUUID)      // lookup connection by user
//
// Group operations:
//
//	svc.Server().JoinGroupByUser(groupID, serverID, userUUID)   // user joins group
//	svc.Server().LeaveGroupByUser(groupID, serverID, userUUID)  // user leaves group
//	svc.Server().SendToGroup(groupID, msg)                      // push to all group members
//	svc.Server().PushToGroup(groupID, msg, exclude...)          // push with exclude list
//
// Broadcast:
//
//	svc.Server().Broadcast(msg, exclude...)            // push to all connected clients
//
// User registration (required for push operations):
//
//	svc.RegisterUser(userUUID, connectionID)           // call in login handler
//	svc.UnregisterUser(userUUID)                       // call on logout
//	svc.Server().GetConnectionIDByUser(userUUID)       // lookup
//
// Server group (internal, for multi-gateway):
//
//	svc.Server().PushToServer(msg)                     // push to all gateways
// ══════════════════════════════════════════════════════════════════════

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
