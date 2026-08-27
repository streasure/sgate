// Package util 提供 gateway 各子包共享的工具函数。
package util

import (
	"fmt"
	"sync/atomic"
	"time"
)

var connIDCounter uint64

// GenerateConnectionID 生成唯一连接 ID
func GenerateConnectionID() string {
	n := atomic.AddUint64(&connIDCounter, 1)
	return fmt.Sprintf("%d%06d", time.Now().UnixMilli(), n%1000000)
}

// GetString 从 map 中安全取 string 值
func GetString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetInt 从 map 中安全取 int 值
func GetInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case int:
			return x
		case float64:
			return int(x)
		}
	}
	return 0
}

// GetFloat 从 map 中安全取 float64 值
func GetFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return float64(x)
		}
	}
	return 0
}

// ParseDurationDefault 解析时间字符串，失败返回默认值
func ParseDurationDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

// SimpleHash FNV-1a 哈希
func SimpleHash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// CopyMap 浅拷贝 map
func CopyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// MaxInt 返回较大值
func MaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
