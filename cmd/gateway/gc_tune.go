package main

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// debugSetGCPercent 设置 GCPercent（runtime/debug.SetGCPercent 各 Go 版本均支持）
// 用于调优 GC 频率，降低千万级吞吐下的 STW
func debugSetGCPercent(percent int) {
	if percent > 0 {
		runtime.GC() // 触发一次 GC，确保启动时清理
		old := debug.SetGCPercent(percent)
		_ = old
	}
}

// applyGOMEMLIMIT 显式将 GOMEMLIMIT 环境变量应用到 runtime。
//
// 设计原因：Go runtime 在启动时读取 GOMEMLIMIT 环境变量，但在某些场景下
// （例如 PowerShell $env:GOMEMLIMIT 设置后立即启动子进程、容器内 env 变量传递延迟），
// runtime 可能未读取到该值，导致软内存上限失效，heap 无限增长直至 VirtualAlloc OOM。
//
// 通过 debug.SetMemoryLimit() 在 main() 内显式设置，作为 env 方式的兜底保障。
// 同时支持 GiB/MiB/KiB 后缀格式解析（与 runtime 的解析规则一致）。
// 返回值：实际生效的字节数；返回 0 表示未设置或解析失败（runtime 维持默认无限）。
func applyGOMEMLIMIT() int64 {
	v := strings.TrimSpace(os.Getenv("GOMEMLIMIT"))
	if v == "" {
		return 0
	}
	// 复用与 runtime/memmetrics.go 相同的后缀规则
	multiplier := int64(1)
	numPart := v
	switch {
	case strings.HasSuffix(v, "GiB"):
		multiplier = 1 << 30
		numPart = strings.TrimSuffix(v, "GiB")
	case strings.HasSuffix(v, "MiB"):
		multiplier = 1 << 20
		numPart = strings.TrimSuffix(v, "MiB")
	case strings.HasSuffix(v, "KiB"):
		multiplier = 1 << 10
		numPart = strings.TrimSuffix(v, "KiB")
	case strings.HasSuffix(v, "G"):
		multiplier = 1 << 30
		numPart = strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		multiplier = 1 << 20
		numPart = strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		multiplier = 1 << 10
		numPart = strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil || n <= 0 {
		return 0
	}
	limit := int64(n * float64(multiplier))
	if limit <= 0 {
		return 0
	}
	// SetMemoryLimit(-1) 仅查询当前值，不变更
	previous := debug.SetMemoryLimit(limit)
	_ = previous
	return limit
}

func parseIntDefault(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}
