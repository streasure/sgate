//go:build !windows

package obs

import "time"

// MonoNowNs 非 Windows 平台回退 time.Now（Linux vDSO 为 ns 级粒度，无粗化问题）。
func MonoNowNs() int64 {
	return time.Now().UnixNano()
}
