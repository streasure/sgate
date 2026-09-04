package gateway

import "sync"

type GroupInfo struct {
	ID          string
	Name        string
	MemberCount int
	SessionIDs  []string
}

type GroupManager struct {
	groups map[string]*group
	mu     sync.RWMutex
}

type group struct {
	id      string
	members map[string]*Session
}

func NewGroupManager() *GroupManager {
	return &GroupManager{
		groups: make(map[string]*group),
	}
}

func (m *GroupManager) Join(groupID string, sess *Session) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[groupID]
	if !ok {
		g = &group{
			id:      groupID,
			members: make(map[string]*Session),
		}
		m.groups[groupID] = g
	}
	g.members[sess.ID()] = sess
	return len(g.members)
}

func (m *GroupManager) Leave(groupID string, sess *Session) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[groupID]
	if !ok {
		return 0
	}
	delete(g.members, sess.ID())
	count := len(g.members)
	if count == 0 {
		delete(m.groups, groupID)
	}
	return count
}

func (m *GroupManager) MemberCount(groupID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if g, ok := m.groups[groupID]; ok {
		return len(g.members)
	}
	return 0
}

func (m *GroupManager) GetInfo(groupID string) *GroupInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[groupID]
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(g.members))
	for id := range g.members {
		ids = append(ids, id)
	}
	return &GroupInfo{
		ID:          g.id,
		Name:        g.id,
		MemberCount: len(g.members),
		SessionIDs:  ids,
	}
}

func (m *GroupManager) RemoveSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for groupID, g := range m.groups {
		delete(g.members, sessionID)
		if len(g.members) == 0 {
			delete(m.groups, groupID)
		}
	}
}

func (m *GroupManager) RangeSessions(groupID string, fn func(*Session) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[groupID]
	if !ok {
		return
	}
	for _, sess := range g.members {
		if !fn(sess) {
			break
		}
	}
}
