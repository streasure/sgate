package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ConnectionState struct {
	ConnectionID  string    `json:"connection_id"`
	UserUUID      string    `json:"user_uuid"`
	ServerID      string    `json:"server_id"`
	Protocol      string    `json:"protocol"`
	RemoteAddr    string    `json:"remote_addr"`
	ConnectedAt   time.Time `json:"connected_at"`
	LastActiveAt  time.Time `json:"last_active_at"`
}

type MemoryCache struct {
	mu         sync.RWMutex
	data       map[string]*CacheEntry
	userCache  map[string]string
	groupCache map[string]map[string]bool
	stats      CacheStats
}

type CacheEntry struct {
	Value      interface{}
	ExpireAt   time.Time
	CreatedAt  time.Time
	AccessCount int64
}

type CacheStats struct {
	Hits   int64
	Misses int64
	Sets   int64
	Gets   int64
}

var (
	globalCache     *MemoryCache
	cacheOnce       sync.Once
	singleflightMap singleflightGroup
)

type singleflightGroup struct {
	mu sync.Map
}

type singleflightCall struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

func (g *singleflightGroup) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	if c, ok := g.mu.Load(key); ok {
		call := c.(*singleflightCall)
		call.wg.Wait()
		return call.val, call.err
	}

	c := &singleflightCall{}
	c.wg.Add(1)
	g.mu.Store(key, c)
	c.val, c.err = fn()
	c.wg.Done()
	g.mu.Delete(key)

	return c.val, c.err
}

func GetGlobalCache() *MemoryCache {
	cacheOnce.Do(func() {
		globalCache = NewMemoryCache()
	})
	return globalCache
}

func NewMemoryCache() *MemoryCache {
	c := &MemoryCache{
		data:       make(map[string]*CacheEntry),
		userCache:  make(map[string]string),
		groupCache: make(map[string]map[string]bool),
	}
	go c.cleanupLoop()
	return c
}

func (c *MemoryCache) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		c.cleanup()
	}
}

func (c *MemoryCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.data {
		if !entry.ExpireAt.IsZero() && now.After(entry.ExpireAt) {
			delete(c.data, key)
		}
	}
}

func (c *MemoryCache) Set(key string, value interface{}, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	expireAt := time.Time{}
	if expiration > 0 {
		expireAt = time.Now().Add(expiration)
	}

	c.data[key] = &CacheEntry{
		Value:      value,
		ExpireAt:   expireAt,
		CreatedAt:  time.Now(),
	}
	atomic.AddInt64(&c.stats.Sets, 1)

	return nil
}

func (c *MemoryCache) Get(key string) (interface{}, error) {
	c.mu.RLock()
	entry, exists := c.data[key]
	c.mu.RUnlock()

	if !exists {
		atomic.AddInt64(&c.stats.Misses, 1)
		return nil, nil
	}

	if !entry.ExpireAt.IsZero() && time.Now().After(entry.ExpireAt) {
		c.mu.Lock()
		delete(c.data, key)
		c.mu.Unlock()
		atomic.AddInt64(&c.stats.Misses, 1)
		return nil, nil
	}

	atomic.AddInt64(&entry.AccessCount, 1)
	atomic.AddInt64(&c.stats.Gets, 1)
	atomic.AddInt64(&c.stats.Hits, 1)

	return entry.Value, nil
}

func (c *MemoryCache) GetOrSet(key string, fn func() (interface{}, error), expiration time.Duration) (interface{}, error) {
	val, err := c.Get(key)
	if err != nil {
		return nil, err
	}
	if val != nil {
		return val, nil
	}

	newVal, err := singleflightMap.Do(key, fn)
	if err != nil {
		return nil, err
	}

	if newVal != nil {
		c.Set(key, newVal, expiration)
	}

	return newVal, nil
}

func (c *MemoryCache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.data, key)
	return nil
}

func (c *MemoryCache) Exists(key string) bool {
	c.mu.RLock()
	entry, exists := c.data[key]
	c.mu.RUnlock()

	if !exists {
		return false
	}

	if !entry.ExpireAt.IsZero() && time.Now().After(entry.ExpireAt) {
		c.mu.Lock()
		delete(c.data, key)
		c.mu.Unlock()
		return false
	}

	return true
}

func (c *MemoryCache) Increment(key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.data[key]
	if !exists {
		c.data[key] = &CacheEntry{Value: int64(1)}
		return 1, nil
	}

	switch v := entry.Value.(type) {
	case int64:
		v++
		entry.Value = v
		return v, nil
	case int:
		v++
		entry.Value = int64(v)
		return int64(v), nil
	}
	return 0, nil
}

func (c *MemoryCache) Decrement(key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.data[key]
	if !exists {
		c.data[key] = &CacheEntry{Value: int64(-1)}
		return -1, nil
	}

	switch v := entry.Value.(type) {
	case int64:
		v--
		entry.Value = v
		return v, nil
	case int:
		v--
		entry.Value = int64(v)
		return int64(v), nil
	}
	return 0, nil
}

func (c *MemoryCache) HashSet(key, field string, value interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var hash map[string]interface{}
	entry, exists := c.data[key]
	if !exists {
		hash = make(map[string]interface{})
		c.data[key] = &CacheEntry{Value: hash}
	} else {
		var ok bool
		hash, ok = entry.Value.(map[string]interface{})
		if !ok {
			hash = make(map[string]interface{})
			c.data[key] = &CacheEntry{Value: hash}
		}
	}
	hash[field] = value
	return nil
}

func (c *MemoryCache) HashGet(key, field string) (interface{}, error) {
	c.mu.RLock()
	entry, exists := c.data[key]
	c.mu.RUnlock()

	if !exists {
		return nil, nil
	}

	hash, ok := entry.Value.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	return hash[field], nil
}

func (c *MemoryCache) HashDelete(key string, fields ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.data[key]
	if !exists {
		return nil
	}

	hash, ok := entry.Value.(map[string]interface{})
	if !ok {
		return nil
	}

	for _, field := range fields {
		delete(hash, field)
	}
	return nil
}

func (c *MemoryCache) SetAdd(key string, members ...interface{}) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var set map[string]bool
	entry, exists := c.data[key]
	if !exists {
		set = make(map[string]bool)
		c.data[key] = &CacheEntry{Value: set}
	} else {
		var ok bool
		set, ok = entry.Value.(map[string]bool)
		if !ok {
			set = make(map[string]bool)
			c.data[key] = &CacheEntry{Value: set}
		}
	}

	for _, m := range members {
		set[fmt.Sprintf("%v", m)] = true
	}
	return int64(len(set)), nil
}

func (c *MemoryCache) SetMembers(key string) ([]string, error) {
	c.mu.RLock()
	entry, exists := c.data[key]
	c.mu.RUnlock()

	if !exists {
		return []string{}, nil
	}

	set, ok := entry.Value.(map[string]bool)
	if !ok {
		return []string{}, nil
	}

	members := make([]string, 0, len(set))
	for m := range set {
		members = append(members, m)
	}
	return members, nil
}

func (c *MemoryCache) SetContains(key string, member interface{}) (bool, error) {
	c.mu.RLock()
	entry, exists := c.data[key]
	c.mu.RUnlock()

	if !exists {
		return false, nil
	}

	set, ok := entry.Value.(map[string]bool)
	if !ok {
		return false, nil
	}

	return set[fmt.Sprintf("%v", member)], nil
}

func (c *MemoryCache) AcquireLock(key string, value string, expiration time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, exists := c.data[key]
	if exists {
		return false, nil
	}

	c.data[key] = &CacheEntry{
		Value:    value,
		ExpireAt: time.Now().Add(expiration),
	}
	return true, nil
}

func (c *MemoryCache) ReleaseLock(key string, value string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.data[key]
	if !exists {
		return false, nil
	}

	if entry.Value == value {
		delete(c.data, key)
		return true, nil
	}
	return false, nil
}

func (c *MemoryCache) UserLogin(userUUID, connectionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldConnID, exists := c.userCache[userUUID]
	if exists {
		delete(c.data, oldConnID)
	}
	c.userCache[userUUID] = connectionID
}

func (c *MemoryCache) UserLogout(userUUID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.userCache, userUUID)
}

func (c *MemoryCache) GetUserConnection(userUUID string) (string, bool) {
	c.mu.RLock()
	connID, exists := c.userCache[userUUID]
	c.mu.RUnlock()
	return connID, exists
}

func (c *MemoryCache) JoinGroup(groupID, userUUID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.groupCache[groupID]; !exists {
		c.groupCache[groupID] = make(map[string]bool)
	}
	c.groupCache[groupID][userUUID] = true
}

func (c *MemoryCache) LeaveGroup(groupID, userUUID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if users, exists := c.groupCache[groupID]; exists {
		delete(users, userUUID)
		if len(users) == 0 {
			delete(c.groupCache, groupID)
		}
	}
}

func (c *MemoryCache) GetGroupMembers(groupID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if users, exists := c.groupCache[groupID]; exists {
		members := make([]string, 0, len(users))
		for u := range users {
			members = append(members, u)
		}
		return members
	}
	return []string{}
}

func (c *MemoryCache) GetGroupMemberCount(groupID string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if users, exists := c.groupCache[groupID]; exists {
		return len(users)
	}
	return 0
}

func (c *MemoryCache) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"hits":     atomic.LoadInt64(&c.stats.Hits),
		"misses":   atomic.LoadInt64(&c.stats.Misses),
		"sets":     atomic.LoadInt64(&c.stats.Sets),
		"gets":     atomic.LoadInt64(&c.stats.Gets),
		"keys":     c.Count(),
	}
}

func (c *MemoryCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

func (c *MemoryCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = make(map[string]*CacheEntry)
	c.userCache = make(map[string]string)
	c.groupCache = make(map[string]map[string]bool)
}

type DistributedManager struct {
	cache *MemoryCache
}

func newDistributedManager() *DistributedManager {
	return NewDistributedManager()
}

func NewDistributedManager() *DistributedManager {
	return &DistributedManager{
		cache: GetGlobalCache(),
	}
}

func (dm *DistributedManager) RegisterConnection(state *ConnectionState) error {
	dm.cache.UserLogin(state.UserUUID, state.ConnectionID)
	return dm.cache.Set("conn:"+state.ConnectionID, state, 5*time.Minute)
}

func (dm *DistributedManager) UnregisterConnection(connectionID string) error {
	dm.cache.Delete("conn:" + connectionID)
	return nil
}

func (dm *DistributedManager) GetConnection(connectionID string) (*ConnectionState, error) {
	val, _ := dm.cache.Get("conn:" + connectionID)
	if val == nil {
		return nil, nil
	}
	return val.(*ConnectionState), nil
}

func (dm *DistributedManager) GetAllConnections() ([]*ConnectionState, error) {
	return []*ConnectionState{}, nil
}

func (dm *DistributedManager) GetStats() (map[string]interface{}, error) {
	return dm.cache.GetStats(), nil
}

func (dm *DistributedManager) Close() {
	dm.cache.Close()
}

type CacheConfig struct {
	Enabled      bool
	Host         string
	Port         int
	Password     string
	DB           int
	PoolSize     int
	MinIdleConns int
	KeyPrefix    string
}

func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		Enabled:   false,
		KeyPrefix: "sgate",
	}
}

func ProductionCacheConfig() CacheConfig {
	return CacheConfig{
		Enabled:   true,
		KeyPrefix: "sgate",
	}
}