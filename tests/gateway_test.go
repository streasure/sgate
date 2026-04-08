package gateway

import (
	"context"
	"testing"
)

// TestGateway_NewGateway 测试创建网关实例
func TestGateway_NewGateway(t *testing.T) {
	gw := NewGateway()
	if gw == nil {
		t.Fatalf("期望创建网关实例，实际为nil")
	}
}

// TestConnectionManager 测试连接管理器
func TestConnectionManager(t *testing.T) {
	cm := NewConnectionManager(nil, context.Background())

	// 测试连接数
	if cm.GetConnectionCount() != 0 {
		t.Errorf("期望初始连接数为0，实际为%d", cm.GetConnectionCount())
	}

	// 测试关闭所有连接
	cm.CloseAllConnections()
	if cm.GetConnectionCount() != 0 {
		t.Errorf("期望关闭后连接数为0，实际为%d", cm.GetConnectionCount())
	}
}

// TestRouteManager 测试路由管理器
func TestRouteManager(t *testing.T) {
	rm := NewRouteManager()

	// 测试获取路由
	routes := rm.GetRoutes()
	if len(routes) == 0 {
		t.Errorf("期望路由数大于0，实际为%d", len(routes))
	}

	// 测试注册和注销路由
	testRoute := "test_route"
	rm.RegisterRoute(testRoute, func(connectionID string, payload map[string]interface{}, callback func(map[string]interface{})) {
		callback(map[string]interface{}{"route": "test_response"})
	})

	routes = rm.GetRoutes()
	found := false
	for _, route := range routes {
		if route == testRoute {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("期望找到路由'%s'，实际未找到", testRoute)
	}

	rm.UnregisterRoute(testRoute)
	routes = rm.GetRoutes()
	found = false
	for _, route := range routes {
		if route == testRoute {
			found = true
			break
		}
	}
	if found {
		t.Errorf("期望未找到路由'%s'，实际找到", testRoute)
	}
}

// TestGateway_Broadcast 测试广播功能
func TestGateway_Broadcast(t *testing.T) {
	gw := NewGateway()

	// 测试广播消息
	gw.Broadcast("test broadcast message")
	// 由于没有实际连接，这里只测试方法是否能正常调用
}

// TestGateway_Close 测试关闭网关
func TestGateway_Close(t *testing.T) {
	gw := NewGateway()

	// 测试关闭网关
	gw.Close()
	// 这里只测试方法是否能正常调用
}
