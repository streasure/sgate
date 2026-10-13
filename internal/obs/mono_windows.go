//go:build windows

package obs

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 高分辨率单调钟：裸 QPC（QueryPerformanceCounter）。
//
// 为什么不用 time.Now：本机实测 Go runtime.nanotime 粗化到 ~1.2µs
// （连续读 99.99% 相同），而裸 QPC 粒度 100ns——亚微秒级管道延迟会被
// time.Since 截成 0，/stats 的 P50/P95/P99 恒 0。
// Windows 文档保证 QPC 跨处理器一致且单调。
// 每次调用一次 syscall（~50ns），仅用于延迟测量等观测点。

var (
	monoOnce sync.Once
	monoQPC  *syscall.LazyProc
	monoFreq int64
	monoOK   bool
)

func monoInit() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	monoQPC = k32.NewProc("QueryPerformanceCounter")
	qpf := k32.NewProc("QueryPerformanceFrequency")
	var freq int64
	r1, _, _ := qpf.Call(uintptr(unsafe.Pointer(&freq)))
	if r1 != 0 && freq > 0 {
		monoFreq = freq
		var c int64
		r2, _, _ := monoQPC.Call(uintptr(unsafe.Pointer(&c)))
		monoOK = r2 != 0
	}
}

// MonoNowNs 返回单调纳秒时间戳（Windows: 裸 QPC；QPC 失败回退 time.Now）。
func MonoNowNs() int64 {
	monoOnce.Do(monoInit)
	if monoOK {
		var c int64
		monoQPC.Call(uintptr(unsafe.Pointer(&c)))
		return qpcToNS(c)
	}
	return time.Now().UnixNano()
}

// qpcToNS 将 QPC 计数转纳秒，拆秒避免 int64 溢出。
func qpcToNS(ticks int64) int64 {
	sec := ticks / monoFreq
	rem := ticks % monoFreq
	return sec*1_000_000_000 + rem*1_000_000_000/monoFreq
}
