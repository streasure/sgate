package gateway

import (
	"hash/fnv"
	"sync"
)

const groupShardCount = 256

// GroupManager provides sharded lock for better concurrency
type GroupManager struct {
	shards [groupShardCount]groupShard
}

type groupShard struct {
	mu    sync.RWMutex
	items map[string]*ConnectionGroupInfo
}

func NewGroupManager() *GroupManager {
	gm := &GroupManager{}
	for i := range gm.shards {
		gm.shards[i].items = make(map[string]*ConnectionGroupInfo)
	}
	return gm
}

func (gm *GroupManager) getShard(key string) *groupShard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &gm.shards[h.Sum32()%groupShardCount]
}

func (gm *GroupManager) GetOrCreateGroup(groupID, groupName string) *ConnectionGroupInfo {
	shard := gm.getShard(groupID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if group, ok := shard.items[groupID]; ok {
		return group
	}
	group := &ConnectionGroupInfo{
		Name:    groupName,
		Members: make(map[serverUserKey]struct{}),
	}
	shard.items[groupID] = group
	return group
}

func (gm *GroupManager) GetGroup(groupID string) *ConnectionGroupInfo {
	shard := gm.getShard(groupID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	return shard.items[groupID]
}

func (gm *GroupManager) AddMember(groupID string, key serverUserKey) {
	shard := gm.getShard(groupID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	group, ok := shard.items[groupID]
	if !ok {
		group = &ConnectionGroupInfo{
			Name:    groupID,
			Members: make(map[serverUserKey]struct{}),
		}
		shard.items[groupID] = group
	}
	group.AddMember(key)
}

func (gm *GroupManager) RemoveMember(groupID string, key serverUserKey) bool {
	shard := gm.getShard(groupID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	group, ok := shard.items[groupID]
	if !ok {
		return false
	}
	empty := group.RemoveMember(key)
	if empty {
		delete(shard.items, groupID)
	}
	return empty
}

func (gm *GroupManager) DeleteGroup(groupID string) {
	shard := gm.getShard(groupID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.items, groupID)
}

func (gm *GroupManager) MemberCount(groupID string) int {
	shard := gm.getShard(groupID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if group, ok := shard.items[groupID]; ok {
		return group.MemberCount()
	}
	return 0
}

func (gm *GroupManager) Snapshot(groupID string) []serverUserKey {
	shard := gm.getShard(groupID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if group, ok := shard.items[groupID]; ok {
		return group.Snapshot()
	}
	return nil
}
