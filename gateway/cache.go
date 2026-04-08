package gateway

import (
	"sync"
	"time"
)

// CacheItem 缓存项结构
// 功能: 定义缓存项的结构，包含值和过期时间
// 字段:
//   Value: 缓存值
//   Expiration: 过期时间

type CacheItem struct {
	Value      interface{} // 缓存值
	Expiration time.Time   // 过期时间
}

// Cache 缓存结构
// 功能: 管理缓存项，提供设置、获取、删除等操作
// 字段:
//   items: 缓存项映射
//   mu: 互斥锁，用于保证并发安全

type Cache struct {
	items map[string]*CacheItem // 缓存项映射
	mu    sync.RWMutex         // 互斥锁，用于保证并发安全
}

// NewCache 创建缓存实例
// 功能: 创建一个新的缓存实例，并启动缓存清理协程
// 返回值:
//
//	*Cache: 缓存实例
func NewCache() *Cache {
	cache := &Cache{
		items: make(map[string]*CacheItem),
	}
	
	// 启动缓存清理协程
	go cache.cleanup()
	
	return cache
}

// Set 设置缓存
// 功能: 设置缓存项，包括键、值和过期时间
// 参数:
//
//	key: 缓存键
//	value: 缓存值
//	expiration: 过期时间
func (c *Cache) Set(key string, value interface{}, expiration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	c.items[key] = &CacheItem{
		Value:      value,
		Expiration: time.Now().Add(expiration),
	}
}

// Get 获取缓存
// 功能: 根据键获取缓存值，并检查是否过期
// 参数:
//
//	key: 缓存键
//
// 返回值:
//
//	interface{}: 缓存值
//	bool: 是否存在且未过期
func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	item, exists := c.items[key]
	if !exists {
		return nil, false
	}
	
	// 检查是否过期
	if time.Now().After(item.Expiration) {
		return nil, false
	}
	
	return item.Value, true
}

// Delete 删除缓存
// 功能: 根据键删除缓存项
// 参数:
//
//	key: 缓存键
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	delete(c.items, key)
}

// cleanup 清理过期缓存
// 功能: 定期清理过期的缓存项
func (c *Cache) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for {
		<-ticker.C
		c.mu.Lock()
		now := time.Now()
		for key, item := range c.items {
			if now.After(item.Expiration) {
				delete(c.items, key)
			}
		}
		c.mu.Unlock()
	}
}

// Clear 清空缓存
// 功能: 清空所有缓存项
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	c.items = make(map[string]*CacheItem)
}

// Size 获取缓存大小
// 功能: 获取当前缓存项的数量
// 返回值:
//
//	int: 缓存项数量
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	return len(c.items)
}
