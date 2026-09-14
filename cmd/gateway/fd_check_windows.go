//go:build windows

package main

// Windows 平台无 ulimit，仅在 fd_check.go 中打印提示。
func checkLinuxFDLimit(maxConnections int) {}
