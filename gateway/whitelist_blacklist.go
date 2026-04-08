package gateway

import (
	"sync"
)

// WhitelistBlacklist 白名单和黑名单管理
// 字段:
//   whitelist: 白名单
//   blacklist: 黑名单
//   mu: 互斥锁

type WhitelistBlacklist struct {
	whitelist        map[string]bool
	blacklist        map[string]bool
	mu               sync.RWMutex
}

// NewWhitelistBlacklist 创建白名单和黑名单管理器
// 返回值:
//
//	*WhitelistBlacklist: 白名单和黑名单管理器实例
func NewWhitelistBlacklist() *WhitelistBlacklist {
	return &WhitelistBlacklist{
		whitelist:       make(map[string]bool),
		blacklist:       make(map[string]bool),
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

// GetWhitelist 获取白名单
// 返回值:
//
//	[]string: 白名单列表
func (wbl *WhitelistBlacklist) GetWhitelist() []string {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	whitelist := make([]string, 0, len(wbl.whitelist))
	for key := range wbl.whitelist {
		whitelist = append(whitelist, key)
	}

	return whitelist
}

// GetBlacklist 获取黑名单
// 返回值:
//
//	[]string: 黑名单列表
func (wbl *WhitelistBlacklist) GetBlacklist() []string {
	wbl.mu.RLock()
	defer wbl.mu.RUnlock()

	blacklist := make([]string, 0, len(wbl.blacklist))
	for key := range wbl.blacklist {
		blacklist = append(blacklist, key)
	}

	return blacklist
}
