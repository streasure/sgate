package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	protoGw "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// 切维护（maintenance）：通过 HTTP admin 接口按 serverId 或 zone 将目标服务器置入维护，
// 阻断新登录并踢出其上所有在线玩家。
//
// 处理时序（必须先标记后踢人，杜绝踢完又被补进来的竞态）：
//  1. 同步：写入维护标记（isMaintenanceTarget 立即对 LoginGate 生效）
//  2. 同步：快照命中的在线连接（纯内存遍历）
//  3. 异步：worker 并发执行 踢人通知(CMD_KICK_NOTIFY) → 通知逻辑服下线(CmdUserOffline，
//     触发 logic 侧会话/推送组清理) → 断开连接（OnClose 兜底移除网关侧推送组并清理映射）
//  4. 响应 matched/accepted，完成时打点日志

// maintenanceState 切维护标记集合：serverId 维度与 zone 维度二选一（由调用方保证单选）。
type maintenanceState struct {
	mu      sync.RWMutex
	servers map[string]string // serverId -> reason
	zones   map[string]string // zone -> reason
}

func newMaintenanceState() *maintenanceState {
	return &maintenanceState{
		servers: make(map[string]string),
		zones:   make(map[string]string),
	}
}

// enable 写入标记；serverID 与 zone 仅允许其一非空。
func (s *maintenanceState) enable(serverID, zone, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if serverID != "" {
		s.servers[serverID] = reason
		return
	}
	s.zones[zone] = reason
}

// disable 移除标记，返回是否确实存在并被移除。
func (s *maintenanceState) disable(serverID, zone string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if serverID != "" {
		if _, ok := s.servers[serverID]; !ok {
			return false
		}
		delete(s.servers, serverID)
		return true
	}
	if _, ok := s.zones[zone]; !ok {
		return false
	}
	delete(s.zones, zone)
	return true
}

func (s *maintenanceState) serverMarked(serverID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.servers[serverID]
	return ok
}

func (s *maintenanceState) zoneMarked(zone string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.zones[zone]
	return ok
}

// zoneOfServer 返回 serverId 所属 zone：优先 etcd 发现记录，静态模式回退网关自身 zone。
func (g *Gateway) zoneOfServer(serverID string) string {
	if g.logicClientPool != nil {
		if z := g.logicClientPool.ZoneOf(serverID); z != "" {
			return z
		}
	}
	return g.zone
}

// isMaintenanceTarget 判断目标 serverId 是否处于维护（serverId 标记或其所属 zone 标记命中）。
// 供 LoginGate 登录前检查使用。
func (g *Gateway) isMaintenanceTarget(serverID string) bool {
	if g.maintenance == nil || serverID == "" {
		return false
	}
	if g.maintenance.serverMarked(serverID) {
		return true
	}
	return g.maintenance.zoneMarked(g.zoneOfServer(serverID))
}

// snapshotMaintenanceTargets 收集当前在线且命中维护目标的连接。
// serverID 非空时按 serverId 精确匹配；否则按 zone 匹配。
func (g *Gateway) snapshotMaintenanceTargets(serverID, zone string) []*connection.Connection {
	var matched []*connection.Connection
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		cid := conn.GetServerID()
		if cid == "" {
			return true
		}
		if serverID != "" {
			if cid == serverID {
				matched = append(matched, conn)
			}
			return true
		}
		if g.zoneOfServer(cid) == zone {
			matched = append(matched, conn)
		}
		return true
	})
	return matched
}

// pushKickNotify 向客户端推送踢下线通知（CMD_KICK_NOTIFY + logic.KickNotify）。
// Send 内部走 AsyncWrite，event loop 外安全。
func (g *Gateway) pushKickNotify(conn *connection.Connection, reason string) {
	body, err := proto.Marshal(&protoLogic.KickNotify{
		Reason:  reason,
		Code:    503,
		Message: "server maintenance",
	})
	if err != nil {
		return
	}
	data, err := routes.MarshalClientMessage(&protoGw.StreamData{
		Cmd:  routes.CmdKickNotify,
		Data: body,
	})
	if err != nil {
		return
	}
	_ = conn.Send(data)
}

// kickForMaintenance 单连接踢下线：踢人通知 → 逻辑服下线（清会话/推送组） → 断开。
// 断开后 OnClose 会 RemoveConnection 并从网关侧全部推送组移除该成员。
func (g *Gateway) kickForMaintenance(conn *connection.Connection, reason string) {
	g.pushKickNotify(conn, reason)
	g.notifyLogicOffline(conn)
	if conn.Conn != nil {
		_ = conn.Conn.Close()
	}
}

// kickConnectionsAsync 分片并发踢人，完成后打点总耗时结果。
func (g *Gateway) kickConnectionsAsync(matched []*connection.Connection, reason string) {
	if len(matched) == 0 {
		return
	}
	total := len(matched)
	go func() {
		var kicked atomic.Int64
		var wg sync.WaitGroup
		const workers = 32
		chunkSize := (total + workers - 1) / workers
		for w := 0; w < workers; w++ {
			lo := w * chunkSize
			if lo >= total {
				break
			}
			hi := min(lo+chunkSize, total)
			wg.Add(1)
			go func(chunk []*connection.Connection) {
				defer wg.Done()
				for _, conn := range chunk {
					g.kickForMaintenance(conn, reason)
					kicked.Add(1)
				}
			}(matched[lo:hi])
		}
		wg.Wait()
		tlog.Info(context.TODO(), "maintenance kick done matched=%d kicked=%d reason=%s", total, kicked.Load(), reason)
	}()
}

// maintenanceRequest /admin/maintenance 请求体。
type maintenanceRequest struct {
	Action   string `json:"action"`   // enable | disable
	ServerID string `json:"serverId"` // 与 zone 二选一
	Zone     string `json:"zone"`     // 与 serverId 二选一
	Reason   string `json:"reason"`   // 可选，踢人通知附带原因
}

// maintenanceResponse /admin/maintenance 响应体。
type maintenanceResponse struct {
	OK       bool   `json:"ok"`
	Action   string `json:"action"`
	ServerID string `json:"serverId,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Matched  int    `json:"matched"`            // 命中的在线连接数
	Accepted bool   `json:"accepted,omitempty"` // enable：踢人任务已异步受理
	Removed  bool   `json:"removed,omitempty"`  // disable：标记确实被移除
}

// handleAdminMaintenance POST /admin/maintenance 切维护接口。
func (g *Gateway) handleAdminMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !g.adminAuthed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req maintenanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	serverID := strings.TrimSpace(req.ServerID)
	zone := strings.TrimSpace(req.Zone)
	if (serverID == "") == (zone == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serverId or zone required (exactly one)"})
		return
	}
	if g.maintenance == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "maintenance not initialized"})
		return
	}

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "enable":
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = "maintenance"
		}
		// 1) 先打标记：LoginGate 立即拒绝新登录，避免踢人窗口期补进新连接
		g.maintenance.enable(serverID, zone, reason)
		// 2) 快照命中的在线连接（纯内存，微秒级）
		matched := g.snapshotMaintenanceTargets(serverID, zone)
		// 3) 异步并发踢人：通知 → 退推送组 → 断连
		g.kickConnectionsAsync(matched, reason)
		tlog.Info(context.TODO(), "maintenance enabled action=enable serverId=%s zone=%s reason=%s matched=%d",
			serverID, zone, reason, len(matched))
		writeJSON(w, http.StatusOK, maintenanceResponse{
			OK: true, Action: "enable", ServerID: serverID, Zone: zone,
			Reason: reason, Matched: len(matched), Accepted: true,
		})
	case "disable":
		removed := g.maintenance.disable(serverID, zone)
		tlog.Info(context.TODO(), "maintenance disabled action=disable serverId=%s zone=%s removed=%v",
			serverID, zone, removed)
		writeJSON(w, http.StatusOK, maintenanceResponse{
			OK: true, Action: "disable", ServerID: serverID, Zone: zone, Removed: removed,
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be enable or disable"})
	}
}
