//go:build !windows

package main

// setProcessPriorityHigh 在非 Windows 平台为 no-op。
// Windows 的 SetPriorityClass/kernel32.dll 在 Linux/macOS 不可用，
// 优先级调整由 OS 调度器/容器 runtime class 处理。
func setProcessPriorityHigh() {}
