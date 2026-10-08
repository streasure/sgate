package security

import (
	"maps"
	"slices"
	"sync"
)

// WhitelistBlacklist 白名单和黑名单管理
// 字段:
//   whitelist: 白名单
//   blacklist: 黑名单
//   mu: 互斥锁

type WhitelistBlacklist struct {
	whitelist map[string]bool
	blacklist map[string]bool
	mu        sync.RWMutex
}

// NewWhitelistBlacklist 创建白名单和黑名单管理器
// 返回值:
//
//	*WhitelistBlacklist: 白名单和黑名单管理器实例
func NewWhitelistBlacklist() *WhitelistBlacklist {
	return &WhitelistBlacklist{
		whitelist: make(map[string]bool),
		blacklist: make(map[string]bool),
	}
}

// IsInWhitelist 检查是否在白名单中
// 参数:
//
//	key: 要检查的键
//
// 返回值:
//
//	bool: 是否在白名单中
func (wbl *WhitelistBlacklist) IsInWhitelist(key string) bool {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	return wbl.whitelist[key]
}

// IsInBlacklist 检查是否在黑名单中
// 参数:
//
//	key: 要检查的键
//
// 返回值:
//
//	bool: 是否在黑名单中
func (wbl *WhitelistBlacklist) IsInBlacklist(key string) bool {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	return wbl.blacklist[key]
}

// AddToWhitelist 添加到白名单
// 参数:
//
//	key: 要添加的键
//
// 返回值:
//
//	err: 错误信息
func (wbl *WhitelistBlacklist) AddToWhitelist(key string) error {
	// 添加到内存
	wbl.mu.Lock()
	wbl.whitelist[key] = true
	wbl.mu.Unlock()

	return nil
}

// RemoveFromWhitelist 从白名单中移除
// 参数:
//
//	key: 要移除的键
//
// 返回值:
//
//	err: 错误信息
func (wbl *WhitelistBlacklist) RemoveFromWhitelist(key string) error {
	// 从内存移除
	wbl.mu.Lock()
	delete(wbl.whitelist, key)
	wbl.mu.Unlock()

	return nil
}

// AddToBlacklist 添加到黑名单
// 参数:
//
//	key: 要添加的键
//
// 返回值:
//
//	err: 错误信息
func (wbl *WhitelistBlacklist) AddToBlacklist(key string) error {
	// 添加到内存
	wbl.mu.Lock()
	wbl.blacklist[key] = true
	wbl.mu.Unlock()

	return nil
}

// RemoveFromBlacklist 从黑名单中移除
// 参数:
//
//	key: 要移除的键
//
// 返回值:
//
//	err: 错误信息
func (wbl *WhitelistBlacklist) RemoveFromBlacklist(key string) error {
	// 从内存移除
	wbl.mu.Lock()
	delete(wbl.blacklist, key)
	wbl.mu.Unlock()

	return nil
}

// ReplaceWhitelist 原子替换白名单，热更新时先构建新 map 再一次性 swap，避免中间态读到半空列表。
func (wbl *WhitelistBlacklist) ReplaceWhitelist(keys []string) {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	wbl.mu.Lock()
	wbl.whitelist = m
	wbl.mu.Unlock()
}

// ReplaceBlacklist 原子替换黑名单。
func (wbl *WhitelistBlacklist) ReplaceBlacklist(keys []string) {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	wbl.mu.Lock()
	wbl.blacklist = m
	wbl.mu.Unlock()
}

// WhitelistEmpty 返回白名单是否为空（无锁拷贝检查，避免热路径每次 GetWhitelist 分配切片）。
func (wbl *WhitelistBlacklist) WhitelistEmpty() bool {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()
	return len(wbl.whitelist) == 0
}

// GetWhitelist 获取白名单
// 返回值:
//
//	[]string: 白名单列表
func (wbl *WhitelistBlacklist) GetWhitelist() []string {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	return slices.Collect(maps.Keys(wbl.whitelist))
}

// GetBlacklist 获取黑名单
// 返回值:
//
//	[]string: 黑名单列表
func (wbl *WhitelistBlacklist) GetBlacklist() []string {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	return slices.Collect(maps.Keys(wbl.blacklist))
}
