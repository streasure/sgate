package gateway

import (
	"sync"
	"time"
)

// BackendServer 后端服务器结构
// 功能: 定义后端服务器的结构，包含服务器的基本信息和状态
// 字段:
//   URL: 服务器URL
//   Weight: 服务器权重
//   Connections: 当前连接数
//   Healthy: 是否健康
//   LastCheck: 最后检查时间

type BackendServer struct {
	URL         string    // 服务器URL
	Weight      int       // 服务器权重
	Connections int       // 当前连接数
	Healthy     bool      // 是否健康
	LastCheck   time.Time // 最后检查时间
}

// LoadBalancer 负载均衡器结构
// 功能: 管理后端服务器，提供负载均衡功能
// 字段:
//   servers: 后端服务器列表
//   mu: 互斥锁，用于保证并发安全
//   index: 当前轮询索引

type LoadBalancer struct {
	servers []*BackendServer // 后端服务器列表
	mu      sync.RWMutex     // 互斥锁，用于保证并发安全
	index   int              // 当前轮询索引
}

// NewLoadBalancer 创建负载均衡器实例
// 功能: 创建一个新的负载均衡器实例
// 返回值:
//
//	*LoadBalancer: 负载均衡器实例
func NewLoadBalancer() *LoadBalancer {
	return &LoadBalancer{
		servers: make([]*BackendServer, 0),
		index:   0,
	}
}

// AddServer 添加后端服务器
// 功能: 添加一个后端服务器到负载均衡器
// 参数:
//
//	url: 服务器URL
//	weight: 服务器权重
func (lb *LoadBalancer) AddServer(url string, weight int) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	
	lb.servers = append(lb.servers, &BackendServer{
		URL:         url,
		Weight:      weight,
		Connections: 0,
		Healthy:     true,
		LastCheck:   time.Now(),
	})
}

// RemoveServer 移除后端服务器
// 功能: 从负载均衡器中移除指定的后端服务器
// 参数:
//
//	url: 服务器URL
func (lb *LoadBalancer) RemoveServer(url string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	
	for i, server := range lb.servers {
		if server.URL == url {
			lb.servers = append(lb.servers[:i], lb.servers[i+1:]...)
			break
		}
	}
}

// GetServer 获取后端服务器（轮询算法）
// 功能: 使用轮询算法获取一个健康的后端服务器
// 返回值:
//
//	*BackendServer: 后端服务器，如果没有健康的服务器则返回nil
func (lb *LoadBalancer) GetServer() *BackendServer {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	
	if len(lb.servers) == 0 {
		return nil
	}
	
	// 轮询选择服务器
	for i := 0; i < len(lb.servers); i++ {
		lb.index = (lb.index + 1) % len(lb.servers)
		server := lb.servers[lb.index]
		if server.Healthy {
			server.Connections++
			return server
		}
	}
	
	return nil
}

// ReleaseServer 释放后端服务器
// 功能: 释放后端服务器，减少其连接计数
// 参数:
//
//	server: 后端服务器
func (lb *LoadBalancer) ReleaseServer(server *BackendServer) {
	if server != nil {
		lb.mu.Lock()
		defer lb.mu.Unlock()
		if server.Connections > 0 {
			server.Connections--
		}
	}
}

// UpdateServerHealth 更新服务器健康状态
// 功能: 更新后端服务器的健康状态
// 参数:
//
//	url: 服务器URL
//	healthy: 是否健康
func (lb *LoadBalancer) UpdateServerHealth(url string, healthy bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	
	for _, server := range lb.servers {
		if server.URL == url {
			server.Healthy = healthy
			server.LastCheck = time.Now()
			break
		}
	}
}

// GetHealthyServers 获取健康的后端服务器
// 功能: 获取所有健康的后端服务器
// 返回值:
//
//	[]*BackendServer: 健康的后端服务器列表
func (lb *LoadBalancer) GetHealthyServers() []*BackendServer {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	healthyServers := make([]*BackendServer, 0)
	for _, server := range lb.servers {
		if server.Healthy {
			healthyServers = append(healthyServers, server)
		}
	}
	
	return healthyServers
}

// GetAllServers 获取所有后端服务器
// 功能: 获取所有后端服务器，包括健康和不健康的
// 返回值:
//
//	[]*BackendServer: 所有后端服务器列表
func (lb *LoadBalancer) GetAllServers() []*BackendServer {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	servers := make([]*BackendServer, len(lb.servers))
	copy(servers, lb.servers)
	return servers
}
