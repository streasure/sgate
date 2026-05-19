package main

import (
	"fmt"
	"os"
	"runtime"
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
	MP            int
	MaxMP         int
	PosX          float64
	PosY          float64
	CurrentRoomID string
	TeamID        string
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

	server.OnDisconnect(pm.onPlayerDisconnect)

	return pm
}

func (pm *PlayerManager) onPlayerDisconnect(connectionID string) {
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

	serverID := player.ServerID
	roomID := player.CurrentRoomID
	teamID := player.TeamID

	delete(pm.players, userID)
	delete(pm.connToPlayer, connectionID)

	tlog.Info("player offline",
		"userID", userID,
		"name", player.Name,
		"serverID", serverID,
		"connectionID", connectionID,
		"onlineCount", len(pm.players),
	)

	if serverID != "" {
		pm.server.PushToGroup("server:"+serverID, &protobuf.Message{
			Route: "server.playerOffline",
			Payload: map[string]string{
				"userID":   userID,
				"name":     player.Name,
				"serverID": serverID,
			},
		}, connectionID)
	}

	if roomID != "" {
		pm.server.PushToGroup(roomID, &protobuf.Message{
			Route: "server.room.playerLeft",
			Payload: map[string]string{
				"userID": userID,
				"name":   player.Name,
				"roomID": roomID,
			},
		}, connectionID)
	}

	if teamID != "" {
		pm.server.PushToGroup(teamID, &protobuf.Message{
			Route: "server.team.memberLeft",
			Payload: map[string]string{
				"userID": userID,
				"name":   player.Name,
				"teamID": teamID,
			},
		}, connectionID)
	}
}

func (pm *PlayerManager) Login(connectionID, userID, name, serverID string) *Player {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if existing, ok := pm.players[userID]; ok {
		oldConnID := existing.ConnectionID
		if oldConnID != connectionID {
			pm.server.PushToConnection(oldConnID, &protobuf.Message{
				Route:   "server.kick",
				Payload: map[string]string{"reason": "duplicate_login", "serverID": serverID},
			})
			delete(pm.connToPlayer, oldConnID)
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
		MP:            50,
		MaxMP:         50,
		PosX:          0,
		PosY:          0,
		LastHeartbeat: time.Now(),
	}

	pm.players[userID] = player
	pm.connToPlayer[connectionID] = userID

	if serverID != "" {
		pm.server.JoinGroup("server:"+serverID, connectionID)
		player.CurrentRoomID = "room:world"
		pm.server.JoinGroup("room:world", connectionID)
	}

	tlog.Info("player online",
		"userID", userID,
		"name", name,
		"serverID", serverID,
		"connectionID", connectionID,
		"onlineCount", len(pm.players),
	)

	if serverID != "" {
		pm.server.PushToGroup("server:"+serverID, &protobuf.Message{
			Route: "server.playerOnline",
			Payload: map[string]string{
				"userID":   userID,
				"name":     name,
				"level":    fmt.Sprintf("%d", player.Level),
				"serverID": serverID,
			},
		}, connectionID)

		pm.server.PushToGroup("room:world", &protobuf.Message{
			Route: "server.room.playerJoined",
			Payload: map[string]string{
				"userID": userID,
				"name":   name,
				"roomID": "room:world",
			},
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

func (pm *PlayerManager) CheckHeartbeatTimeout(timeout time.Duration) []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	var timedOut []string

	for userID, player := range pm.players {
		if now.Sub(player.LastHeartbeat) > timeout {
			timedOut = append(timedOut, userID)
			delete(pm.players, userID)
			delete(pm.connToPlayer, player.ConnectionID)

			tlog.Warn("player heartbeat timeout",
				"userID", userID,
				"name", player.Name,
				"lastHeartbeat", player.LastHeartbeat.Format(time.RFC3339),
			)
		}
	}

	return timedOut
}

type GameLogic struct {
	pm     *PlayerManager
	server *logic.Server
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewGameLogic(server *logic.Server) *GameLogic {
	gl := &GameLogic{
		pm:     NewPlayerManager(server),
		server: server,
		stopCh: make(chan struct{}),
	}

	gl.registerRoutes()
	gl.startBackgroundLoops()

	return gl
}

func (gl *GameLogic) registerRoutes() {
	gl.server.RegisterRoute("player.login", gl.handleLogin)
	gl.server.RegisterRoute("player.heartbeat", gl.handleHeartbeat)
	gl.server.RegisterRoute("player.move", gl.handleMove)
	gl.server.RegisterRoute("player.attack", gl.handleAttack)
	gl.server.RegisterRoute("player.useItem", gl.handleUseItem)
	gl.server.RegisterRoute("player.chat", gl.handleChat)
	gl.server.RegisterRoute("player.queryStatus", gl.handleQueryStatus)
	gl.server.RegisterRoute("player.queryOnline", gl.handleQueryOnline)
	gl.server.RegisterRoute("room.join", gl.handleRoomJoin)
	gl.server.RegisterRoute("room.leave", gl.handleRoomLeave)
	gl.server.RegisterRoute("room.info", gl.handleRoomInfo)
	gl.server.RegisterRoute("team.create", gl.handleTeamCreate)
	gl.server.RegisterRoute("team.join", gl.handleTeamJoin)
	gl.server.RegisterRoute("team.leave", gl.handleTeamLeave)
	gl.server.RegisterRoute("team.info", gl.handleTeamInfo)
}

func (gl *GameLogic) handleLogin(msg *protobuf.Message) *protobuf.Message {
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

	if serverID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.login",
			Payload:      map[string]string{"code": "403", "message": "serverID required"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	player := gl.pm.Login(msg.ConnectionId, userID, name, serverID)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.login",
		Payload: map[string]string{
			"code":     "200",
			"level":    fmt.Sprintf("%d", player.Level),
			"hp":       fmt.Sprintf("%d", player.HP),
			"mp":       fmt.Sprintf("%d", player.MP),
			"posX":     fmt.Sprintf("%.1f", player.PosX),
			"posY":     fmt.Sprintf("%.1f", player.PosY),
			"roomID":   player.CurrentRoomID,
			"serverID": player.ServerID,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleHeartbeat(msg *protobuf.Message) *protobuf.Message {
	ok := gl.pm.UpdateHeartbeat(msg.ConnectionId)
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
			"onlineCount": fmt.Sprintf("%d", gl.pm.GetOnlineCount()),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleMove(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.move",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	var posX, posY float64
	fmt.Sscanf(msg.GetPayload()["posX"], "%f", &posX)
	fmt.Sscanf(msg.GetPayload()["posY"], "%f", &posY)
	player.PosX = posX
	player.PosY = posY
	roomID := player.CurrentRoomID
	gl.pm.mu.Unlock()

	if roomID != "" {
		gl.server.PushToGroup(roomID, &protobuf.Message{
			Route: "server.playerMoved",
			Payload: map[string]string{
				"userID": player.UserID,
				"name":   player.Name,
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
}

func (gl *GameLogic) handleAttack(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.attack",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	targetID := msg.GetPayload()["targetID"]
	skillID := msg.GetPayload()["skillID"]

	gl.pm.mu.RLock()
	target, exists := gl.pm.players[targetID]
	gl.pm.mu.RUnlock()

	if !exists {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.attack",
			Payload:      map[string]string{"code": "404", "message": "target not found"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	damage := 25
	gl.pm.mu.Lock()
	target.HP -= damage
	if target.HP < 0 {
		target.HP = 0
	}
	remainingHP := target.HP
	targetConnID := target.ConnectionID
	roomID := player.CurrentRoomID
	gl.pm.mu.Unlock()

	gl.server.PushToConnection(targetConnID, &protobuf.Message{
		Route: "server.damageNotify",
		Payload: map[string]string{
			"attackerID":  player.UserID,
			"attacker":    player.Name,
			"skillID":     skillID,
			"damage":      fmt.Sprintf("%d", damage),
			"remainingHP": fmt.Sprintf("%d", remainingHP),
		},
	})

	if roomID != "" {
		gl.server.PushToGroup(roomID, &protobuf.Message{
			Route: "server.attackBroadcast",
			Payload: map[string]string{
				"attackerID":  player.UserID,
				"attacker":    player.Name,
				"targetID":    targetID,
				"target":      target.Name,
				"skillID":     skillID,
				"damage":      fmt.Sprintf("%d", damage),
				"remainingHP": fmt.Sprintf("%d", remainingHP),
			},
		}, msg.ConnectionId, targetConnID)
	}

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.attack",
		Payload: map[string]string{
			"code":        "200",
			"damage":      fmt.Sprintf("%d", damage),
			"remainingHP": fmt.Sprintf("%d", remainingHP),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleUseItem(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.useItem",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	itemID := msg.GetPayload()["itemID"]

	gl.pm.mu.Lock()
	var effect string
	switch itemID {
	case "hp_potion":
		player.HP += 30
		if player.HP > player.MaxHP {
			player.HP = player.MaxHP
		}
		effect = fmt.Sprintf("HP+%d, currentHP=%d", 30, player.HP)
	case "mp_potion":
		player.MP += 20
		if player.MP > player.MaxMP {
			player.MP = player.MaxMP
		}
		effect = fmt.Sprintf("MP+%d, currentMP=%d", 20, player.MP)
	default:
		gl.pm.mu.Unlock()
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.useItem",
			Payload:      map[string]string{"code": "400", "message": "unknown item"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}
	gl.pm.mu.Unlock()

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.useItem",
		Payload: map[string]string{
			"code":   "200",
			"itemID": itemID,
			"effect": effect,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleChat(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.chat",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	chatMsg := msg.GetPayload()["message"]
	chatType := msg.GetPayload()["type"]
	if chatType == "" {
		chatType = "world"
	}

	chatPayload := map[string]string{
		"type":     chatType,
		"userID":   player.UserID,
		"userName": player.Name,
		"message":  chatMsg,
	}

	switch chatType {
	case "world":
		if player.ServerID != "" {
			gl.server.PushToGroup("server:"+player.ServerID, &protobuf.Message{
				Route: "server.chat", Payload: chatPayload,
			})
		}
	case "room":
		gl.pm.mu.RLock()
		roomID := player.CurrentRoomID
		gl.pm.mu.RUnlock()
		if roomID == "" {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.chat",
				Payload:      map[string]string{"code": "403", "message": "not in a room"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}
		gl.server.PushToGroup(roomID, &protobuf.Message{
			Route: "server.chat", Payload: chatPayload,
		}, msg.ConnectionId)
	case "team":
		gl.pm.mu.RLock()
		teamID := player.TeamID
		gl.pm.mu.RUnlock()
		if teamID == "" {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.chat",
				Payload:      map[string]string{"code": "403", "message": "not in a team"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}
		gl.server.PushToGroup(teamID, &protobuf.Message{
			Route: "server.chat", Payload: chatPayload,
		}, msg.ConnectionId)
	case "private":
		targetID := msg.GetPayload()["targetID"]
		gl.pm.mu.RLock()
		target, exists := gl.pm.players[targetID]
		gl.pm.mu.RUnlock()
		if !exists {
			return &protobuf.Message{
				ConnectionId: msg.ConnectionId,
				Route:        "player.chat",
				Payload:      map[string]string{"code": "404", "message": "target not online"},
				Timestamp:    time.Now().UnixMilli(),
			}
		}
		gl.server.PushToConnection(target.ConnectionID, &protobuf.Message{
			Route: "server.chat", Payload: chatPayload,
		})
	}

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.chat",
		Payload:      map[string]string{"code": "200"},
		Timestamp:    time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleQueryStatus(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "player.queryStatus",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.RLock()
	defer gl.pm.mu.RUnlock()

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.queryStatus",
		Payload: map[string]string{
			"code":     "200",
			"level":    fmt.Sprintf("%d", player.Level),
			"hp":       fmt.Sprintf("%d", player.HP),
			"maxHP":    fmt.Sprintf("%d", player.MaxHP),
			"mp":       fmt.Sprintf("%d", player.MP),
			"maxMP":    fmt.Sprintf("%d", player.MaxMP),
			"posX":     fmt.Sprintf("%.1f", player.PosX),
			"posY":     fmt.Sprintf("%.1f", player.PosY),
			"roomID":   player.CurrentRoomID,
			"teamID":   player.TeamID,
			"serverID": player.ServerID,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleQueryOnline(msg *protobuf.Message) *protobuf.Message {
	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "player.queryOnline",
		Payload: map[string]string{
			"code":        "200",
			"onlineCount": fmt.Sprintf("%d", gl.pm.GetOnlineCount()),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleRoomJoin(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.join",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	if player.ServerID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.join",
			Payload:      map[string]string{"code": "403", "message": "serverID not bound"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	roomID := msg.GetPayload()["roomID"]
	if roomID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.join",
			Payload:      map[string]string{"code": "400", "message": "roomID required"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	oldRoomID := player.CurrentRoomID
	player.CurrentRoomID = roomID
	gl.pm.mu.Unlock()

	gl.server.LeaveGroup(oldRoomID, msg.ConnectionId)
	gl.server.PushToGroup(oldRoomID, &protobuf.Message{
		Route: "server.room.playerLeft",
		Payload: map[string]string{
			"userID": player.UserID,
			"name":   player.Name,
			"roomID": oldRoomID,
		},
	})

	memberCount := gl.server.JoinGroup(roomID, msg.ConnectionId)
	gl.server.PushToGroup(roomID, &protobuf.Message{
		Route: "server.room.playerJoined",
		Payload: map[string]string{
			"userID": player.UserID,
			"name":   player.Name,
			"roomID": roomID,
		},
	}, msg.ConnectionId)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "room.join",
		Payload: map[string]string{
			"code":        "200",
			"roomID":      roomID,
			"memberCount": fmt.Sprintf("%d", memberCount),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleRoomLeave(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.leave",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	if player.ServerID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.leave",
			Payload:      map[string]string{"code": "403", "message": "serverID not bound"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	oldRoomID := player.CurrentRoomID
	player.CurrentRoomID = "room:world"
	gl.pm.mu.Unlock()

	gl.server.LeaveGroup(oldRoomID, msg.ConnectionId)
	gl.server.PushToGroup(oldRoomID, &protobuf.Message{
		Route: "server.room.playerLeft",
		Payload: map[string]string{
			"userID": player.UserID,
			"name":   player.Name,
			"roomID": oldRoomID,
		},
	})

	gl.server.JoinGroup("room:world", msg.ConnectionId)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "room.leave",
		Payload: map[string]string{
			"code":   "200",
			"roomID": "room:world",
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleRoomInfo(msg *protobuf.Message) *protobuf.Message {
	roomID := msg.GetPayload()["roomID"]
	if roomID == "" {
		player := gl.pm.GetByConnection(msg.ConnectionId)
		if player != nil {
			gl.pm.mu.RLock()
			roomID = player.CurrentRoomID
			gl.pm.mu.RUnlock()
		}
	}

	if roomID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "room.info",
			Payload:      map[string]string{"code": "400", "message": "roomID required"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	members := gl.server.GetGroupMembers(roomID)
	memberCount := gl.server.GetGroupCount(roomID)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "room.info",
		Payload: map[string]string{
			"code":        "200",
			"roomID":      roomID,
			"memberCount": fmt.Sprintf("%d", memberCount),
			"members":     fmt.Sprintf("%v", members),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleTeamCreate(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.create",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	if player.TeamID != "" {
		oldTeamID := player.TeamID
		gl.pm.mu.Unlock()
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.create",
			Payload:      map[string]string{"code": "403", "message": "already in team", "teamID": oldTeamID},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	teamID := fmt.Sprintf("team_%d", time.Now().UnixNano())
	player.TeamID = teamID
	gl.pm.mu.Unlock()

	gl.server.JoinGroup(teamID, msg.ConnectionId)

	tlog.Info("team created", "teamID", teamID, "leader", player.UserID)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "team.create",
		Payload: map[string]string{
			"code":   "200",
			"teamID": teamID,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleTeamJoin(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.join",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	teamID := msg.GetPayload()["teamID"]
	if teamID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.join",
			Payload:      map[string]string{"code": "400", "message": "teamID required"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	if player.TeamID != "" {
		oldTeamID := player.TeamID
		gl.pm.mu.Unlock()
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.join",
			Payload:      map[string]string{"code": "403", "message": "already in team", "teamID": oldTeamID},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	memberCount := gl.server.GetGroupCount(teamID)
	if memberCount == 0 {
		gl.pm.mu.Unlock()
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.join",
			Payload:      map[string]string{"code": "404", "message": "team not found"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	player.TeamID = teamID
	gl.pm.mu.Unlock()

	memberCount = gl.server.JoinGroup(teamID, msg.ConnectionId)

	gl.server.PushToGroup(teamID, &protobuf.Message{
		Route: "server.team.memberJoined",
		Payload: map[string]string{
			"userID": player.UserID,
			"name":   player.Name,
			"teamID": teamID,
		},
	}, msg.ConnectionId)

	tlog.Info("player joined team", "teamID", teamID, "userID", player.UserID, "memberCount", memberCount)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "team.join",
		Payload: map[string]string{
			"code":        "200",
			"teamID":      teamID,
			"memberCount": fmt.Sprintf("%d", memberCount),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleTeamLeave(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.leave",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	gl.pm.mu.Lock()
	teamID := player.TeamID
	if teamID == "" {
		gl.pm.mu.Unlock()
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.leave",
			Payload:      map[string]string{"code": "403", "message": "not in a team"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}
	player.TeamID = ""
	gl.pm.mu.Unlock()

	remainingCount := gl.server.LeaveGroup(teamID, msg.ConnectionId)

	gl.server.PushToGroup(teamID, &protobuf.Message{
		Route: "server.team.memberLeft",
		Payload: map[string]string{
			"userID": player.UserID,
			"name":   player.Name,
			"teamID": teamID,
		},
	})

	tlog.Info("player left team", "teamID", teamID, "userID", player.UserID, "remainingCount", remainingCount)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "team.leave",
		Payload: map[string]string{
			"code":           "200",
			"teamID":         teamID,
			"remainingCount": fmt.Sprintf("%d", remainingCount),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) handleTeamInfo(msg *protobuf.Message) *protobuf.Message {
	player := gl.pm.GetByConnection(msg.ConnectionId)
	if player == nil {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.info",
			Payload:      map[string]string{"code": "401", "message": "not logged in"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	teamID := msg.GetPayload()["teamID"]
	if teamID == "" {
		gl.pm.mu.RLock()
		teamID = player.TeamID
		gl.pm.mu.RUnlock()
	}

	if teamID == "" {
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "team.info",
			Payload:      map[string]string{"code": "403", "message": "not in a team"},
			Timestamp:    time.Now().UnixMilli(),
		}
	}

	members := gl.server.GetGroupMembers(teamID)
	memberCount := gl.server.GetGroupCount(teamID)

	return &protobuf.Message{
		ConnectionId: msg.ConnectionId,
		Route:        "team.info",
		Payload: map[string]string{
			"code":        "200",
			"teamID":      teamID,
			"memberCount": fmt.Sprintf("%d", memberCount),
			"members":     fmt.Sprintf("%v", members),
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

func (gl *GameLogic) startBackgroundLoops() {
	gl.wg.Add(2)
	go gl.heartbeatCheckLoop()
	go gl.serverNotifyLoop()
}

func (gl *GameLogic) heartbeatCheckLoop() {
	defer gl.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			timedOut := gl.pm.CheckHeartbeatTimeout(30 * time.Second)
			for _, userID := range timedOut {
				tlog.Warn("kicked player due to heartbeat timeout", "userID", userID)
			}
		case <-gl.stopCh:
			return
		}
	}
}

func (gl *GameLogic) serverNotifyLoop() {
	defer gl.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			gl.server.PushToServer(&protobuf.Message{
				Route: "server.announce",
				Payload: map[string]string{
					"onlineCount": fmt.Sprintf("%d", gl.pm.GetOnlineCount()),
					"serverTime":  fmt.Sprintf("%d", time.Now().UnixMilli()),
				},
			})
		case <-gl.stopCh:
			return
		}
	}
}

func (gl *GameLogic) Stop() {
	close(gl.stopCh)
	gl.wg.Wait()
}

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

	gameLogic := NewGameLogic(svc.Server())

	if err := svc.Run(); err != nil {
		gameLogic.Stop()
		fmt.Fprintf(os.Stderr, "game logic service failed: %v\n", err)
		os.Exit(1)
	}
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
