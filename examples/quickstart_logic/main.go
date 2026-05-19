package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

type Player struct {
	ConnectionID  string
	UserID        string
	Name          string
	ServerID      string
	Level         int
	HP            int
	MaxHP         int
	PosX          float64
	PosY          float64
	LastHeartbeat time.Time
}

type PlayerManager struct {
	mu           sync.RWMutex
	players      map[string]*Player
	connToPlayer map[string]string
	server       *logic.Server
}

func NewPlayerManager(server *logic.Server) *PlayerManager {
	pm := &PlayerManager{
		players:      make(map[string]*Player),
		connToPlayer: make(map[string]string),
		server:       server,
	}
	server.OnDisconnect(pm.OnDisconnect)
	return pm
}

func (pm *PlayerManager) OnDisconnect(connectionID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	userID, ok := pm.connToPlayer[connectionID]
	if !ok {
		return
	}

	player, exists := pm.players[userID]
	if !exists {
		return
	}

	delete(pm.players, userID)
	delete(pm.connToPlayer, connectionID)

	tlog.Info("player offline", "userID", userID, "name", player.Name, "onlineCount", len(pm.players))

	if player.ServerID != "" {
		pm.server.PushToGroup("server:"+player.ServerID, &protobuf.Message{
			Route:   "server.playerOffline",
			Payload: map[string]string{"userID": userID, "name": player.Name},
		}, connectionID)
	}
}

func (pm *PlayerManager) Login(connectionID, userID, name, serverID string) *Player {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if existing, ok := pm.players[userID]; ok {
		if existing.ConnectionID != connectionID {
			pm.server.PushToConnection(existing.ConnectionID, &protobuf.Message{
				Route:   "server.kick",
				Payload: map[string]string{"reason": "duplicate_login"},
			})
			delete(pm.connToPlayer, existing.ConnectionID)
		}
		existing.ConnectionID = connectionID
		existing.ServerID = serverID
		existing.LastHeartbeat = time.Now()
		if serverID != "" {
			pm.server.JoinGroup("server:"+serverID, connectionID)
		}
		return existing
	}

	player := &Player{
		ConnectionID:  connectionID,
		UserID:        userID,
		Name:          name,
		ServerID:      serverID,
		Level:         1,
		HP:            100,
		MaxHP:         100,
		LastHeartbeat: time.Now(),
	}

	pm.players[userID] = player
	pm.connToPlayer[connectionID] = userID

	if serverID != "" {
		pm.server.JoinGroup("server:"+serverID, connectionID)
	}

	tlog.Info("player online", "userID", userID, "name", name, "serverID", serverID, "onlineCount", len(pm.players))

	if serverID != "" {
		pm.server.PushToGroup("server:"+serverID, &protobuf.Message{
			Route:   "server.playerOnline",
			Payload: map[string]string{"userID": userID, "name": name, "level": fmt.Sprintf("%d", player.Level)},
		}, connectionID)
	}

	return player
}

func (pm *PlayerManager) GetByConnection(connectionID string) *Player {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	userID, ok := pm.connToPlayer[connectionID]
	if !ok {
		return nil
	}
	return pm.players[userID]
}

func (pm *PlayerManager) UpdateHeartbeat(connectionID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	userID, ok := pm.connToPlayer[connectionID]
	if !ok {
		return false
	}
	player, exists := pm.players[userID]
	if !exists {
		return false
	}
	player.LastHeartbeat = time.Now()
	return true
}

func (pm *PlayerManager) GetOnlineCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.players)
}

func main() {
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			tlog.New("../../config/tlog.yaml")
		}
	}

	svc := logic.NewService(
		logic.WithServiceID(getEnv("LOGIC_SERVICE_ID", "game_logic_1")),
		logic.WithServiceName(getEnv("LOGIC_SERVICE_NAME", "logic")),
		logic.WithListenPort(getEnv("LOGIC_PORT", "50052")),
		logic.WithAdvertiseAddr(getEnv("LOGIC_ADVERTISE_ADDR", "")),
		logic.WithRedisAddr(getEnv("REDIS_ADDR", "127.0.0.1:6379")),
		logic.WithHeartbeat(3*time.Second, 10*time.Second),
	)

	pm := NewPlayerManager(svc.Server())

	svc.RegisterRoute("player.login", func(msg *protobuf.Message) *protobuf.Message {
		userID := msg.GetPayload()["userID"]
		name := msg.GetPayload()["name"]
		serverID := msg.GetPayload()["serverID"]

		if userID == "" || name == "" {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.login",
				Payload:      map[string]string{"code": "400", "message": "userID and name required"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		player := pm.Login(msg.ConnectionId, userID, name, serverID)

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.login",
			Payload: map[string]string{
				"code":     "200",
				"level":    fmt.Sprintf("%d", player.Level),
				"hp":       fmt.Sprintf("%d", player.HP),
				"serverID": player.ServerID,
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("player.heartbeat", func(msg *protobuf.Message) *protobuf.Message {
		ok := pm.UpdateHeartbeat(msg.ConnectionId)
		if !ok {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.heartbeat",
				Payload:      map[string]string{"code": "401", "message": "not logged in"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.heartbeat",
			Payload: map[string]string{
				"code":        "200",
				"serverTime":  fmt.Sprintf("%d", time.Now().UnixMilli()),
				"onlineCount": fmt.Sprintf("%d", pm.GetOnlineCount()),
			},
			Timestamp: time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("player.move", func(msg *protobuf.Message) *protobuf.Message {
		player := pm.GetByConnection(msg.ConnectionId)
		if player == nil {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.move",
				Payload:      map[string]string{"code": "401", "message": "not logged in"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		var posX, posY float64
		fmt.Sscanf(msg.GetPayload()["posX"], "%f", &posX)
		fmt.Sscanf(msg.GetPayload()["posY"], "%f", &posY)

		pm.mu.Lock()
		player.PosX = posX
		player.PosY = posY
		serverID := player.ServerID
		pm.mu.Unlock()

		if serverID != "" {
			svc.Server().PushToGroup("server:"+serverID, &protobuf.Message{
				Route: "server.playerMoved",
				Payload: map[string]string{
					"userID": player.UserID,
					"posX":   fmt.Sprintf("%.1f", posX),
					"posY":   fmt.Sprintf("%.1f", posY),
				},
			}, msg.ConnectionId)
		}

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.move",
			Payload:      map[string]string{"code": "200"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("player.chat", func(msg *protobuf.Message) *protobuf.Message {
		player := pm.GetByConnection(msg.ConnectionId)
		if player == nil {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.chat",
				Payload:      map[string]string{"code": "401", "message": "not logged in"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		chatMsg := msg.GetPayload()["message"]

		if player.ServerID != "" {
			svc.Server().PushToGroup("server:"+player.ServerID, &protobuf.Message{
				Route: "server.chat",
				Payload: map[string]string{
					"userID":   player.UserID,
					"userName": player.Name,
					"message":  chatMsg,
				},
			})
		}

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.chat",
			Payload:      map[string]string{"code": "200"},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("server.push", func(msg *protobuf.Message) *protobuf.Message {
		serverID := msg.GetPayload()["serverID"]
		message := msg.GetPayload()["message"]

		if serverID == "" {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "server.push",
				Payload:      map[string]string{"code": "400", "message": "serverID required"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}

		sent := svc.Server().PushToGroup("server:"+serverID, &protobuf.Message{
			Route:   "server.announcement",
			Payload: map[string]string{"message": message, "from": "system"},
		})

		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "server.push",
			Payload:      map[string]string{"code": "200", "sent": fmt.Sprintf("%d", sent)},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("ping", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "ping",
			Payload:      map[string]string{"message": "pong", "timestamp": fmt.Sprintf("%d", time.Now().UnixMilli())},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	svc.RegisterRoute("test", func(msg *protobuf.Message) *protobuf.Message {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "test",
			Payload:      map[string]string{"message": "ok", "timestamp": fmt.Sprintf("%d", time.Now().UnixMilli())},
			Timestamp:    time.Now().UnixMilli(),
		}
	})

	tlog.Info("quickstart logic service starting...")

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
