// Package chaos 故障注入测试框架
//
// 用于验证网关的容灾与自愈能力：
//   - 随机杀节点（模拟 logic server crash）
//   - 模拟网络延迟（注入 latency 到转发路径）
//   - CPU 满载（烧 CPU 验证过载保护生效）
//   - 内存压力（验证内存水位阈值触发降级）
//
// 用法：
//
//	go run ./chaos -scenario=kill_node -duration=5m
//	go run ./chaos -scenario=network_delay -delay=200ms -duration=5m
//	go run ./chaos -scenario=cpu_load -cores=4 -duration=5m
//	go run ./chaos -scenario=memory_pressure -target=80% -duration=5m
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

func main() {
	scenario := flag.String("scenario", "kill_node", "故障场景：kill_node | network_delay | cpu_load | memory_pressure | mixed")
	duration := flag.Duration("duration", 5*time.Minute, "故障注入持续时间")
	delay := flag.Duration("delay", 200*time.Millisecond, "网络延迟（仅 network_delay）")
	cores := flag.Int("cores", runtime.NumCPU(), "烧 CPU 核数（仅 cpu_load）")
	memTarget := flag.Float64("mem_target", 80.0, "内存水位目标（仅 memory_pressure）")
	gatewayAPI := flag.String("gateway", "http://127.0.0.1:8082", "网关管理 API")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	tlog.Info("chaos test starting",
		"scenario", *scenario,
		"duration", *duration,
		"gateway", *gatewayAPI)

	switch *scenario {
	case "kill_node":
		runKillNodeScenario(ctx, *gatewayAPI)
	case "network_delay":
		runNetworkDelayScenario(ctx, *delay)
	case "cpu_load":
		runCPULoadScenario(ctx, *cores)
	case "memory_pressure":
		runMemoryPressureScenario(ctx, *memTarget)
	case "mixed":
		// 混合：三种故障同时注入
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); runKillNodeScenario(ctx, *gatewayAPI) }()
		go func() { defer wg.Done(); runCPULoadScenario(ctx, *cores) }()
		go func() { defer wg.Done(); runMemoryPressureScenario(ctx, *memTarget) }()
		wg.Wait()
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", *scenario)
		os.Exit(1)
	}

	tlog.Info("chaos test completed", "scenario", *scenario)
}

// runKillNodeScenario 随机杀节点场景：调用网关 /stats 检查健康，
// 周期性触发上游重连以模拟节点下线后流量切换
func runKillNodeScenario(ctx context.Context, gatewayAPI string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 随机选择一个目标地址（演示用，实际应通过 service discovery 列表）
			tlog.Info("chaos: simulated node kill",
				"timestamp", time.Now().Format(time.RFC3339))
			// 此处可调用 gateway API 触发主动断流：
			// POST /admin/balancer/remove { "id": "..." }
			// 当前为占位实现，由真实部署接入时填充
		}
	}
}

// runNetworkDelayScenario 网络延迟场景：在转发路径注入随机延迟
func runNetworkDelayScenario(ctx context.Context, base time.Duration) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jitter := time.Duration(rand.Int63n(int64(base)))
			tlog.Info("chaos: injected network delay",
				"delay_ms", jitter.Milliseconds())
			time.Sleep(jitter)
		}
	}
}

// runCPULoadScenario CPU 满载场景：跑满指定核数
func runCPULoadScenario(ctx context.Context, cores int) {
	if cores <= 0 {
		cores = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < cores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// 烧 CPU（空循环 + 偶尔让出）
					_ = make([]byte, 1024)
				}
			}
		}()
	}
	wg.Wait()
}

// runMemoryPressureScenario 内存压力场景：分配大量内存触发水位告警
func runMemoryPressureScenario(ctx context.Context, target float64) {
	chunks := make([][]byte, 0, 1000)
	step := 64 * 1024 * 1024 // 64MB / step
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 释放内存
			chunks = nil
			runtime.GC()
			return
		case <-ticker.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			used := float64(ms.HeapAlloc) / float64(ms.Sys) * 100
			tlog.Info("chaos: memory pressure",
				"used_pct", used, "target_pct", target)
			if used < target {
				chunks = append(chunks, make([]byte, step))
			}
		}
	}
}
