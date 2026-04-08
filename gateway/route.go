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
//   mutex: 读写锁，用于保护routes映射

type RouteManager struct {
	routes map[string]RouteHandler // 路由映射
	mutex  sync.RWMutex            // 读写锁
}

// NewRouteManager 创建路由管理器实例
// 返回值:
//
//	*RouteManager: 路由管理器实例
func NewRouteManager() *RouteManager {
	rm := &RouteManager{
		routes: make(map[string]RouteHandler),
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

	return rm
}

// RegisterRoute 注册路由
// 参数:
//
//	route: 路由名称
//	handler: 路由处理函数
func (rm *RouteManager) RegisterRoute(route string, handler RouteHandler) {
	rm.mutex.Lock()
	rm.routes[route] = handler
	rm.mutex.Unlock()
	// 输出调试日志
	tlog.Debug("路由已注册", "route", route)
}

// UnregisterRoute 注销路由
// 参数:
//
//	route: 路由名称
func (rm *RouteManager) UnregisterRoute(route string) {
	rm.mutex.Lock()
	if _, exists := rm.routes[route]; exists {
		delete(rm.routes, route)
		// 输出调试日志
		tlog.Debug("路由已注销", "route", route)
	}
	rm.mutex.Unlock()
}

// HandleRoute 处理路由
// 参数:
//
//	connectionID: 连接ID
//	route: 路由名称
//	payload: 消息负载
//	callback: 回调函数，用于返回响应
func (rm *RouteManager) HandleRoute(connectionID string, route string, payload interface{}, callback func(interface{})) {
	var handler RouteHandler
	var exists bool

	// 减少锁的持有时间
	rm.mutex.RLock()
	{ // 使用大括号限制锁的作用域
		handler, exists = rm.routes[route]
	}
	rm.mutex.RUnlock()

	if !exists {
		// 输出警告日志
		tlog.Warn("未找到路由", "route", route)
		callback(NewErrorMessage("error", "Route not found", "", ""))
		return
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
	rm.mutex.RLock()
	routes := make([]string, 0, len(rm.routes))
	for route := range rm.routes {
		routes = append(routes, route)
	}
	rm.mutex.RUnlock()
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
	rm.mutex.RLock()
	_, exists := rm.routes[route]
	rm.mutex.RUnlock()
	return exists
}
