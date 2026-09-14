package main

import (
	"runtime"

	"github.com/streasure/util/tlog"
)

// checkFDLimit 检查文件描述符/句柄限制，不足时打印告警。
// 在高并发场景下，百万连接需要 FD > 1M。
func checkFDLimit(maxConnections int) {
	if maxConnections <= 0 {
		// 未设置最大连接数限制时，提示用户关注 FD
		tlog.Info("未设置 maxConnections 限制，建议在生产环境配置以防止 OOM",
			"hint", "在 config.yaml 中设置 protection.maxConnections")
		return
	}

	if runtime.GOOS == "linux" {
		checkLinuxFDLimit(maxConnections)
	} else if runtime.GOOS == "windows" {
		tlog.Info("Windows 平台不检查 FD 限制，请确保系统 socket 句柄充足",
			"maxConnections", maxConnections)
	} else {
		tlog.Info("请确保系统文件描述符限制足够",
			"maxConnections", maxConnections,
			"hint", "ulimit -n 应 >= maxConnections + 1000")
	}
}
