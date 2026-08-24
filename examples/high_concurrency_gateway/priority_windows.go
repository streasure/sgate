//go:build windows

package main

import "syscall"

var (
	modkernel32          = syscall.NewLazyDLL("kernel32.dll")
	procSetPriorityClass = modkernel32.NewProc("SetPriorityClass")
)

func setProcessPriorityHigh() {
	handle, _ := syscall.GetCurrentProcess()
	procSetPriorityClass.Call(uintptr(handle), 0x00000080) // HIGH_PRIORITY_CLASS
	_ = handle
}
