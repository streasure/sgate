package logutil

import "github.com/streasure/util/tlog"

// Init loads the process-specific structured log configuration.
func Init(path string) func() {
	logger, err := tlog.New(path)
	if err != nil {
		panic(err)
	}
	return func() { _ = logger.Sync() }
}
