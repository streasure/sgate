// Package base provides the global Config and cached Options for the gateway.
// Config is loaded once; Options are derived lazily and cached in an atomic.Pointer
// so hot-reload is just a Store+Load.
package base

import (
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
)

var _config atomic.Pointer[config.Config]

// GetConfig returns the current global config (never nil after Load).
func GetConfig() *config.Config {
	return _config.Load()
}

// SetConfig stores a new config and refreshes the cached Options.
func SetConfig(cfg *config.Config) {
	_config.Store(cfg)
	RefreshOptions()
}
