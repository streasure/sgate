//go:build linux

package main

import (
	"syscall"

	"github.com/streasure/util/tlog"
)

func checkLinuxFDLimit(maxConnections int) {
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		tlog.Warn("无法获取文件描述符限制", "error", err)
		return
	}

	softLimit := int64(rlimit.Cur)
	hardLimit := int64(rlimit.Max)
	needed := int64(maxConnections + 1000) // 预留 1000 给系统和其他 fd

	tlog.Info("文件描述符限制",
		"softLimit", softLimit,
		"hardLimit", hardLimit,
		"needed", needed)

	if softLimit < needed {
		tlog.Warn("文件描述符软限制不足，建议提高",
			"current", softLimit,
			"recommended", needed,
			"hint", "ulimit -n 2000000 或修改 /etc/security/limits.conf")
	}
}
