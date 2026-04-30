package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

// RouteHandler 路由处理函数类型
// 参数:
//
//	connectionID: 连接ID
//	payload: 消息负载
//	callback: 回调函数，用于返回响应

type RouteHandler func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{})

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
	rm.RegisterRoute("ping", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("pong", map[string]interface{}{
			"timestamp": time.Now().UnixMilli(),
		}))
	})

	rm.RegisterRoute("getConnections", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		callback(NewResponseMessage("connections", map[string]interface{}{
			"count": 0,
		}))
	})

	rm.RegisterRoute("broadcast", func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
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
	
	commonRoutes := []string{"ping", "version", "getConnections", "broadcast", "health", "api-docs"}
	
	rm.fastPathRoutes = make(map[string]RouteHandler)
	for _, route := range commonRoutes {
		if handler, exists := rm.routes.Load(route); exists {
			typedHandler, ok := handler.(RouteHandler)
			if !ok {
				tlog.Error("route handler type mismatch", "route", route, "type", fmt.Sprintf("%T", handler))
				continue
			}
			rm.fastPathRoutes[route] = typedHandler
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
	rm.updateFastPathRoutes()
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
func (rm *RouteManager) HandleRoute(connectionID string, route string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
	rm.fastPathMutex.RLock()
	handler, exists := rm.fastPathRoutes[route]
	rm.fastPathMutex.RUnlock()

	if !exists {
		handlerInterface, exists := rm.routes.Load(route)
		if !exists {
			tlog.Warn("未找到路由", "route", route)
			callback(NewErrorMessage("error", "Route not found", "", ""))
			return
		}
		typedHandler, ok := handlerInterface.(RouteHandler)
		if !ok {
			tlog.Error("route handler type mismatch in HandleRoute", "route", route, "type", fmt.Sprintf("%T", handlerInterface))
			callback(NewErrorMessage("error", "Route handler type mismatch", "", ""))
			return
		}
		handler = typedHandler
	}

	defer func() {
		if r := recover(); r != nil {
			tlog.Error("路由处理异常", "route", route, "error", r)
			callback(NewErrorMessage("error", "Internal server error", "", ""))
		}
	}()

	handler(connectionID, payload, callback, ctx)
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

type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protobuf.Message) error
}

// RegisterLogicRoute registers a logic server route
func (rm *RouteManager) RegisterLogicRoute(route string) {
	var handler RouteHandler = func(connectionID string, payload interface{}, callback func(interface{}), ctx map[string]interface{}) {
		var logicClient LogicClientProvider
		if gw, ok := ctx["gateway"].(*Gateway); ok {
			logicClient = gw.logicClient
		} else if gw, ok := ctx["gateway"].(*GatewayGnet); ok {
			logicClient = gw.logicClient
		}
		if logicClient == nil {
			callback(NewErrorMessage("error", "Logic client not found", "", ""))
			return
		}

		if !logicClient.IsConnected() {
			callback(NewResponseMessage(route, map[string]string{
				"status":  "success",
				"message": "Logic server not connected, returning mock response",
			}))
			return
		}

		var payloadMap map[string]string
		if pm, ok := payload.(map[string]string); ok {
			payloadMap = pm
		} else {
			payloadMap = map[string]string{}
		}

		msg := &protobuf.Message{
			ConnectionId: connectionID,
			Route:        route,
			Payload:      payloadMap,
			Timestamp:    time.Now().UnixMilli(),
		}

		err := logicClient.SendMessage(msg)
		if err != nil {
			tlog.Error("failed to send message to logic server", "error", err, "route", route)
			callback(NewErrorMessage("error", "Failed to send message to logic server", err.Error(), ""))
			return
		}

		callback(nil)
	}

	rm.routes.Store(route, handler)
	rm.fastPathMutex.Lock()
	rm.fastPathRoutes[route] = handler
	rm.fastPathMutex.Unlock()

	tlog.Info("logic route registered", "route", route)
}
