package gateway

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// TestCloseWithRunningEngineNoDeadlock 回归 P0：Close 曾用 closeOnce 保护，
// 而 gnet 的 engine.Stop 需等事件循环回调 OnShutdown 返回后才置 inShutdown；
// OnShutdown 再进 closeOnce.Do 将与持有 Once 的 Stop 轮询互锁（优雅退出挂死）。
// 修复后 OnShutdown 不等待关闭流程，Close 必须在限时内完成。
func TestCloseWithRunningEngineNoDeadlock(t *testing.T) {
	// 预留一个空闲端口再释放，避免与并行测试冲突
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port failed: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	gw := newGateway(minimalConfig())

	runErr := make(chan error, 1)
	go func() {
		runErr <- gnet.Run(gw, fmt.Sprintf("tcp://127.0.0.1:%d", port), gnet.WithTicker(true))
	}()

	// 等待 OnBoot 注册 engine
	deadline := time.Now().Add(5 * time.Second)
	for {
		gw.enginesMu.Lock()
		n := len(gw.engines)
		gw.enginesMu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not boot in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		gw.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked with a running engine (OnShutdown re-entrant close)")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("gnet.Run returned error after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gnet.Run did not return after Close")
	}
}

// TestOnShutdownFirstTriggerNoDeadlock 覆盖另一方向：engine 自行关闭（无人先调
// Close）时 OnShutdown 作为首触发，须能在不阻塞事件循环的情况下完成整体关闭。
func TestOnShutdownFirstTriggerNoDeadlock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port failed: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	gw := newGateway(minimalConfig())

	runErr := make(chan error, 1)
	go func() {
		runErr <- gnet.Run(gw, fmt.Sprintf("tcp://127.0.0.1:%d", port), gnet.WithTicker(true))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		gw.enginesMu.Lock()
		engines := append([]gnet.Engine(nil), gw.engines...)
		gw.enginesMu.Unlock()
		if len(engines) > 0 {
			// 直接停 engine（不先调 Close），触发 OnShutdown 首触发路径
			_ = engines[0].Stop(context.Background())
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not boot in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-gw.closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("OnShutdown first-trigger close did not complete")
	}

	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("gnet.Run did not return after engine stop")
	}
}
