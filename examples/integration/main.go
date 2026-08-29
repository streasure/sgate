// Package main demonstrates comprehensive integration with sgate,
// covering all push patterns: personal push, group push, and broadcast.
//
// Architecture:
//   Client ──TCP──▶ sgate(:48080) ──gRPC──▶ logic(:50052)
//
// Push patterns (logic → sgate → client):
//   1. Personal push:  PushToConnection(connID, msg)
//   2. Group push:     JoinGroupByUser + SendToGroup
//   3. Broadcast:      Broadcast(msg)
//
// Run:
//   # Start sgate (gateway)
//   cd examples/high_concurrency_gateway && go run main.go
//
//   # Start this logic server
//   cd examples/integration && go run main.go
//
//   # Run bench (duplex baseline)
//   cd examples/bench && go run main.go 127.0.0.1:48080 100 10 16 5000
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
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
	)

	// =========================================================================
	// 1. Duplex baseline (request → response)
	//    Client sends "ping", server responds "pong".
	//    Used by bench tool for QPS measurement.
	// =========================================================================
	svc.RegisterRoute(protobuf.RoutePing, func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RoutePong,
			Payload:      map[string]string{"ts": strconv.FormatInt(time.Now().UnixMilli(), 10)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// BurstRoute: every request triggers a push response (for duplex bench).
	svc.RegisterBurstRoute(protobuf.RouteTest, func(msg *protobuf.Message, push func(*protobuf.Message)) {
		push(&protobuf.Message{
			Route:     protobuf.RouteTestResult,
			Timestamp: time.Now().UnixMilli(),
		})
	})

	// =========================================================================
	// 2. Login + register user
	//    After login, userUUID → connectionID is registered.
	//    This enables PushToConnection (personal push) and group operations.
	// =========================================================================
	svc.RegisterRoute(protobuf.RouteLogin, func(msg *protobuf.Message) *protobuf.Message {
		userID := msg.GetPayload()["userId"]
		if userID == "" {
			userID = msg.ConnectionId
		}
		userUUID := "uuid_" + userID

		// Register user → connection mapping (required for push operations)
		svc.RegisterUser(userUUID, msg.ConnectionId)

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			UserUuid:     userUUID,
			Route:        protobuf.RouteLogin,
			Payload:      map[string]string{"code": "200", "userId": userID, "userUUID": userUUID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// =========================================================================
	// 3. Personal push (via burst callback)
	//    Client sends "push_me", logic pushes a notification back.
	//    Uses burst route's push callback — the most efficient path for
	//    pushing to the requesting client (goes through streamConn directly).
	//
	//    Flow: client → sgate → logic(push callback) → sgate → client
	// =========================================================================
	svc.RegisterBurstRoute("push_me", func(msg *protobuf.Message, push func(*protobuf.Message)) {
		push(&protobuf.Message{
			Route: "personal_notification",
			Payload: map[string]string{
				"type":    "personal_push",
				"message": "Hello from logic server!",
				"from":    "server",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	})

	// =========================================================================
	// 4. Push to another user (via gateway's server.send_to_user route)
	//    Client A sends "send_msg" with targetUUID → logic tells gateway to push to Client B.
	//    Uses PushToServer with route server.send_to_user.
	//
	//    Flow: client_A → sgate → logic(PushToServer) → sgate.gateway.SendToUser → client_B
	// =========================================================================
	svc.RegisterRoute("send_msg", func(msg *protobuf.Message) *protobuf.Message {
		targetUUID := msg.GetPayload()["targetUUID"]
		if targetUUID == "" {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "send_msg_ack",
				Payload:      map[string]string{"code": "400", "message": "missing targetUUID"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		// PushToServer sends a server-side command to the gateway.
		// The gateway handles "server.send_to_user" by looking up the user's connection.
		svc.Server().PushToServer(&protobuf.Message{
			Route: protobuf.RouteServerSendToUser,
			Payload: map[string]string{
				"userUUID": targetUUID,
				"route":    "direct_message",
				"message":  msg.GetPayload()["message"],
				"from":     msg.UserUuid,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "send_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "delivered"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// =========================================================================
	// 5. Group operations
	//    a. join_group  → JoinGroupByUser (user joins a named group)
	//    b. leave_group → LeaveGroupByUser (user leaves a group)
	//    c. group_msg   → SendToGroup (push to all group members)
	//
	//    Groups are gateway-local: each gateway maintains its own group membership.
	//    JoinGroupByUser/LeaveGroupByUser sends a command to the gateway to manage
	//    the group membership for a specific userUUID.
	//
	//    Flow (join):  client → sgate → logic → sgate.gateway.JoinGroupByUser
	//    Flow (push):  client → sgate → logic → sgate.gateway.SendToGroup → all group members
	// =========================================================================
	svc.RegisterRoute("join_group", func(msg *protobuf.Message) *protobuf.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		// Use server.join_group: gateway looks up the connection by msg.ConnectionId,
		// extracts serverID/userUUID from the connection, and adds to the group.
		// This is the correct approach for single-gateway setups.
		svc.Server().PushToServer(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RouteServerJoinGroup,
			Payload:      map[string]string{"groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "join_group_ack",
			Payload:      map[string]string{"code": "200", "groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("leave_group", func(msg *protobuf.Message) *protobuf.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		svc.Server().PushToServer(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RouteServerLeaveGroup,
			Payload:      map[string]string{"groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "leave_group_ack",
			Payload:      map[string]string{"code": "200", "groupID": groupID},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("group_msg", func(msg *protobuf.Message) *protobuf.Message {
		groupID := msg.GetPayload()["groupID"]
		if groupID == "" {
			groupID = "default_room"
		}

		// SendToGroup pushes a message to all members of the specified group.
		svc.Server().SendToGroup(groupID, &protobuf.Message{
			Route: "group_broadcast",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
				"groupID": groupID,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "group_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "sent to group"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// =========================================================================
	// 6. Broadcast (push to ALL connected clients)
	//    Client sends "broadcast_msg", logic broadcasts to every connected client.
	//
	//    Flow: client → sgate → logic(Broadcast) → sgate.gateway.Broadcast → all clients
	//
	//    Broadcast sends via ALL gateways (PushToServer → each gateway's Broadcast).
	//    Use exclude list to skip specific connectionIDs.
	// =========================================================================
	svc.RegisterRoute("broadcast_msg", func(msg *protobuf.Message) *protobuf.Message {
		svc.Server().Broadcast(&protobuf.Message{
			Route: "global_broadcast",
			Payload: map[string]string{
				"message": msg.GetPayload()["message"],
				"from":    msg.UserUuid,
			},
			Timestamp: time.Now().UnixMilli(),
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "broadcast_msg_ack",
			Payload:      map[string]string{"code": "200", "message": "broadcast sent"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	// =========================================================================
	// 7. Batch push (high-throughput pattern)
	//    For scenarios needing massive push throughput (e.g. game state sync),
	//    register a burst handler that pushes to multiple targets per request.
	// =========================================================================
	svc.RegisterBurstRoute("batch_push", func(msg *protobuf.Message, push func(*protobuf.Message)) {
		targetCount := 1
		if n, err := strconv.Atoi(msg.GetPayload()["count"]); err == nil && n > 0 {
			targetCount = n
		}
		for i := 0; i < targetCount; i++ {
			push(&protobuf.Message{
				Route: "batch_push_item",
				Payload: map[string]string{
					"index": strconv.Itoa(i),
					"total": strconv.Itoa(targetCount),
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}
	})

	// =========================================================================
	// API Reference (logic.Server methods):
	//
	// Personal push:
	//   svc.Server().PushToConnection(connID, msg)       // push to specific connection
	//   svc.Server().GetConnectionIDByUser(userUUID)      // lookup connection by user
	//
	// Group operations:
	//   svc.Server().JoinGroupByUser(groupID, serverID, userUUID)   // user joins group
	//   svc.Server().LeaveGroupByUser(groupID, serverID, userUUID)  // user leaves group
	//   svc.Server().SendToGroup(groupID, msg)                      // push to all group members
	//   svc.Server().PushToGroup(groupID, msg, exclude...)          // push with exclude list
	//
	// Broadcast:
	//   svc.Server().Broadcast(msg, exclude...)            // push to all connected clients
	//
	// User registration (required for push operations):
	//   svc.RegisterUser(userUUID, connectionID)           // call in login handler
	//   svc.UnregisterUser(userUUID)                       // call on logout
	//   svc.Server().GetConnectionIDByUser(userUUID)       // lookup
	//
	// Server group (internal, for multi-gateway):
	//   svc.Server().PushToServer(msg)                     // push to all gateways
	// =========================================================================

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
