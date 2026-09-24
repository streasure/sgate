package connection

import (
	"sync"
)

type serverUserKey struct {
	serverID string
	userUUID string
}

// ConnectionGroupInfo 表示连接分组信息，包含分组名称和成员列表。
type ConnectionGroupInfo struct {
	Name    string
	Members map[serverUserKey]struct{}
	mu      sync.RWMutex
}

func (g *ConnectionGroupInfo) AddMember(key serverUserKey) {
	g.mu.Lock()
	g.Members[key] = struct{}{}
	g.mu.Unlock()
}

func (g *ConnectionGroupInfo) RemoveMember(key serverUserKey) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.Members, key)
	return len(g.Members) == 0
}

func (g *ConnectionGroupInfo) MemberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.Members)
}

// Snapshot 返回分组成员的快照列表。
func (g *ConnectionGroupInfo) Snapshot() []serverUserKey {
	g.mu.RLock()
	defer g.mu.RUnlock()
	members := make([]serverUserKey, 0, len(g.Members))
	for key := range g.Members {
		members = append(members, key)
	}
	return members
}
