// base 包提供网关进程级配置和派生选项的全局存取能力。
// 配置通过原子指针保存，选项按需派生并缓存，热更新只需要原子存储和读取。
package base

import (
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
)

var _config atomic.Pointer[config.Config]

// GetConfig 返回当前全局配置。完成初始化后该配置不会为空。
func GetConfig() *config.Config {
	return _config.Load()
}

// SetConfig 保存新的全局配置，并立即刷新派生选项缓存。
func SetConfig(cfg *config.Config) {
	_config.Store(cfg)
	RefreshOptions()
}
