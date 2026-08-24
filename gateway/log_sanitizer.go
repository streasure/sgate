package gateway

import (
	"regexp"
	"strings"
	"sync"
)

// LogSanitizer 日志脱敏器
// 对手机号、身份证、邮箱、银行卡、IP、JWT 等敏感字段进行掩码
type LogSanitizer struct {
	mu       sync.RWMutex
	patterns []sanitizerPattern
}

type sanitizerPattern struct {
	re      *regexp.Regexp
	mask    string
	keepHead int
	keepTail int
}

// NewLogSanitizer 创建日志脱敏器
func NewLogSanitizer() *LogSanitizer {
	s := &LogSanitizer{}
	s.AddDefaults()
	return s
}

// AddDefaults 添加默认脱敏规则
func (s *LogSanitizer) AddDefaults() {
	// 手机号（中国大陆 11 位）
	s.Add(`1[3-9]\d{9}`, "*", 3, 4)
	// 身份证（18 位，最后一位 X）
	s.Add(`[1-9]\d{16}[\dXx]`, "*", 6, 4)
	// 邮箱
	s.Add(`[\w.\-]+@[\w.\-]+\.\w+`, "***", 0, 0)
	// 银行卡（16-19 位连续数字）
	s.Add(`\b\d{16,19}\b`, "*", 4, 4)
	// JWT（三段式）
	s.Add(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`, "eyJ***", 0, 0)
	// IP（保留前 2 段）
	s.Add(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`, "*.*", 0, 0)
}

// Add 添加脱敏规则
func (s *LogSanitizer) Add(pattern, mask string, keepHead, keepTail int) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patterns = append(s.patterns, sanitizerPattern{
		re:       re,
		mask:     mask,
		keepHead: keepHead,
		keepTail: keepTail,
	})
}

// Sanitize 对字符串进行脱敏
func (s *LogSanitizer) Sanitize(input string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := input
	for _, p := range s.patterns {
		out = p.re.ReplaceAllStringFunc(out, func(match string) string {
			return maskString(match, p.mask, p.keepHead, p.keepTail)
		})
	}
	return out
}

// SanitizeBytes 对字节切片脱敏
func (s *LogSanitizer) SanitizeBytes(input []byte) []byte {
	return []byte(s.Sanitize(string(input)))
}

func maskString(s, mask string, keepHead, keepTail int) string {
	if len(s) <= keepHead+keepTail {
		return strings.Repeat(mask, len(s))
	}
	head := s[:keepHead]
	tail := s[len(s)-keepTail:]
	maskedLen := len(s) - keepHead - keepTail
	return head + strings.Repeat(mask, maskedLen) + tail
}

// 全局单例（gateway 包内复用）
var globalLogSanitizer = NewLogSanitizer()

// SanitizeLog 全局便捷函数：对日志字符串脱敏
func SanitizeLog(s string) string {
	return globalLogSanitizer.Sanitize(s)
}

// SanitizeLogBytes 全局便捷函数：对日志字节脱敏
func SanitizeLogBytes(b []byte) []byte {
	return globalLogSanitizer.SanitizeBytes(b)
}
