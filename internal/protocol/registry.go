package protocol

import (
	"fmt"
	"sync"
)

var (
	// 全局注册表：协议名 → 插件实例（tcp/websocket/udp）
	registry   = make(map[string]Protocol)
	registryMu sync.RWMutex
)

// Register 注册协议插件（由插件 init() 调用，启动前装配完成）。
// 重复注册同名协议会 panic（fail-fast，避免插件覆盖竞态）。
func Register(p Protocol) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if name == "" {
		panic("protocol.Register: protocol name cannot be empty")
	}
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("protocol.Register: protocol %q already registered", name))
	}
	registry[name] = p
}

// Get 根据协议名查找插件（配置匹配用，未找到返回 nil）。
func Get(name string) Protocol {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// All 返回所有已注册协议的副本（遍历启动用）。
func All() []Protocol {
	registryMu.RLock()
	defer registryMu.RUnlock()
	protocols := make([]Protocol, 0, len(registry))
	for _, p := range registry {
		protocols = append(protocols, p)
	}
	return protocols
}
