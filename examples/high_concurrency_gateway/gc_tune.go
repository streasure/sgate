package main

import (
	"runtime"
	"runtime/debug"
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
