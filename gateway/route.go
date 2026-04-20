package gateway

import (
	"sync"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// RouteHandler 路由处理函数类型
// 参数:
//
//	connectionID: 连接ID
//	payload: 消息负载
//	callback: 回调函数，用于返回响应

type RouteHandler func(connectionID string, payload interface{}, callback func(interface{}))

// RouteManager 路由管理器
// 字段:
//   routes: 路由映射，key为路由名称，value为路由处理函数
//   使用 sync.Map 实现并发安全
//   fastPathRoutes: 快速路径路由缓存，存储常用路由的处理函数

 type RouteManager struct {
	routes        sync.Map // 路由映射，使用 sync.Map 实现并发安全
	fastPathRoutes map[string]RouteHandler // 快速路径路由缓存，存储常用路由
	fastPathMutex  sync.RWMutex // 快速路径缓存的互斥锁
 }

// NewRouteManager 创建路由管理器实例
// 返回值:
//
//	*RouteManager: 路由管理器实例
func NewRouteManager() *RouteManager {
	rm := &RouteManager{
		fastPathRoutes: make(map[string]RouteHandler),
	}

	// 注册默认路由
	rm.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("pong", map[string]interface{}{
			"timestamp": time.Now().UnixMilli(),
		}))
	})

	rm.RegisterRoute("getConnections", func(connectionID string, payload interface{}, callback func(interface{})) {
		callback(NewResponseMessage("connections", map[string]interface{}{
			"count": 0,
		}))
	})

	rm.RegisterRoute("broadcast", func(connectionID string, payload interface{}, callback func(interface{})) {
		if payloadMap, ok := payload.(map[string]interface{}); ok {
			if _, ok := payloadMap["message"]; ok {
				callback(NewResponseMessage("broadcastResult", map[string]interface{}{
					"success": true,
				}))
			} else {
				callback(NewResponseMessage("broadcastResult", map[string]interface{}{
					"success": false,
					"error":   "No message provided",
				}))
			}
		} else {
			callback(NewResponseMessage("broadcastResult", map[string]interface{}{
				"success": false,
				"error":   "Invalid payload format",
			}))
		}
	})

	// 初始化快速路径缓存
	rm.updateFastPathRoutes()

	return rm
}

// updateFastPathRoutes 更新快速路径缓存
// 功能: 更新快速路径缓存，将常用路由的处理函数缓存到map中
func (rm *RouteManager) updateFastPathRoutes() {
	rm.fastPathMutex.Lock()
	defer rm.fastPathMutex.Unlock()
	
	// 常用路由列表
	commonRoutes := []string{"ping", "version", "getConnections", "broadcast", "health", "api-docs"}
	
	// 清空并重新填充快速路径缓存
	rm.fastPathRoutes = make(map[string]RouteHandler)
	for _, route := range commonRoutes {
		if handler, exists := rm.routes.Load(route); exists {
			rm.fastPathRoutes[route] = handler.(RouteHandler)
		}
	}
}

// RegisterRoute 注册路由
// 参数:
//
//	route: 路由名称
//	handler: 路由处理函数
func (rm *RouteManager) RegisterRoute(route string, handler RouteHandler) {
	rm.routes.Store(route, handler)
	// 更新快速路径缓存
	rm.updateFastPathRoutes()
	// 输出调试日志
	tlog.Debug("路由已注册", "route", route)
}

// UnregisterRoute 注销路由
// 参数:
//
//	route: 路由名称
func (rm *RouteManager) UnregisterRoute(route string) {
	if _, exists := rm.routes.Load(route); exists {
		rm.routes.Delete(route)
		// 更新快速路径缓存
		rm.updateFastPathRoutes()
		// 输出调试日志
		tlog.Debug("路由已注销", "route", route)
	}
}

// HandleRoute 处理路由
// 参数:
//
//	connectionID: 连接ID
//	route: 路由名称
//	payload: 消息负载
//	callback: 回调函数，用于返回响应
func (rm *RouteManager) HandleRoute(connectionID string, route string, payload interface{}, callback func(interface{})) {
	// 首先检查快速路径缓存
	rm.fastPathMutex.RLock()
	handler, exists := rm.fastPathRoutes[route]
	rm.fastPathMutex.RUnlock()

	if !exists {
		// 从sync.Map中查找
		handlerInterface, exists := rm.routes.Load(route)
		if !exists {
			// 输出警告日志
			tlog.Warn("未找到路由", "route", route)
			callback(NewErrorMessage("error", "Route not found", "", ""))
			return
		}
		handler = handlerInterface.(RouteHandler)
	}

	// 执行路由处理函数
	defer func() {
		if r := recover(); r != nil {
			// 输出错误日志
			tlog.Error("路由处理异常", "route", route, "error", r)
			callback(NewErrorMessage("error", "Internal server error", "", ""))
		}
	}()

	handler(connectionID, payload, callback)
}

// GetRoutes 获取所有路由
// 返回值:
//
//	[]string: 路由名称列表
func (rm *RouteManager) GetRoutes() []string {
	var routes []string
	rm.routes.Range(func(key, value interface{}) bool {
		routes = append(routes, key.(string))
		return true
	})
	return routes
}

// HasRoute 检查路由是否存在
// 参数:
//
//	route: 路由名称
//
// 返回值:
//
//	bool: 是否存在
func (rm *RouteManager) HasRoute(route string) bool {
	_, exists := rm.routes.Load(route)
	return exists
}
