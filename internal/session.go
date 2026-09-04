package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/panjf2000/gnet/v2"
)

// SessionState tracks the authentication state of a connection.
type SessionState int32

const (
	StateConnected  SessionState = 0 // TCP connected, not yet LoginGate
	StateBound      SessionState = 1 // LoginGate accepted, bound to logic server
	StateAuthenticated SessionState = 2 // Logic returned user_key
)

// Session represents a single client connection.
type Session struct {
	conn      gnet.Conn
	id        string
	ip        string
	state     SessionState
	serverID  string // bound logic server
	userID    string // client user ID
	userKey   string // logic-assigned user key
	groups    map[string]bool
	mu        sync.RWMutex
}

func NewSession(c gnet.Conn, ip string) *Session {
	return &Session{
		conn:   c,
		id:     generateSessionID(),
		ip:     ip,
		state:  StateConnected,
		groups: make(map[string]bool),
	}
}

func (s *Session) ID() string      { return s.id }
func (s *Session) Conn() gnet.Conn { return s.conn }
func (s *Session) IP() string      { return s.ip }
func (s *Session) ServerID() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.serverID }
func (s *Session) UserID() string  { s.mu.RLock(); defer s.mu.RUnlock(); return s.userID }
func (s *Session) UserKey() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.userKey }

func (s *Session) IsBound() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state >= StateBound
}

func (s *Session) IsAuthenticated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state >= StateAuthenticated
}

func (s *Session) Bind(serverID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverID = serverID
	s.userID = userID
	s.state = StateBound
}

func (s *Session) Authenticate(userKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userKey = userKey
	s.state = StateAuthenticated
}

func (s *Session) AddGroup(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[groupID] = true
}

func (s *Session) RemoveGroup(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, groupID)
}

func (s *Session) InGroup(groupID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groups[groupID]
}

// SessionManager manages all active sessions.
type SessionManager struct {
	sessions map[string]*Session      // sessionID -> Session
	byConn   map[gnet.Conn]*Session   // conn -> Session
	mu       sync.RWMutex
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		byConn:   make(map[gnet.Conn]*Session),
	}
}

func (m *SessionManager) Add(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.id] = s
	m.byConn[s.conn] = s
}

func (m *SessionManager) Remove(c gnet.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byConn[c]; ok {
		delete(m.sessions, s.id)
		delete(m.byConn, c)
	}
}

func (m *SessionManager) GetByID(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *SessionManager) GetByConn(c gnet.Conn) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byConn[c]
}

func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *SessionManager) Range(fn func(*Session) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if !fn(s) {
			break
		}
	}
}

func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
