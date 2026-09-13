package logutil

import "github.com/streasure/util/tlog"

// Init 加载进程级别的结构化日志配置
func Init(path string) func() {
	logger, err := tlog.New(path)
	if err != nil {
		panic(err)
	}
	return func() { _ = logger.Sync() }
}
