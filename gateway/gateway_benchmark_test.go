package gateway

import (
	"testing"
	"time"
)

// BenchmarkGatewayGnetStart 测试 GatewayGnet 启动性能
func BenchmarkGatewayGnetStart(b *testing.B) {
	for i := 0; i < b.N; i++ {
		gw := NewGatewayGnet()
		go func() {
			gw.Start("127.0.0.1:0")
		}()
		time.Sleep(100 * time.Millisecond)
		gw.Close()
	}
}

// BenchmarkGatewayGnetBroadcast 测试 GatewayGnet 广播性能
func BenchmarkGatewayGnetBroadcast(b *testing.B) {
	gw := NewGatewayGnet()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 测试广播性能
		gw.Broadcast("test message")
	}

	gw.Close()
}


