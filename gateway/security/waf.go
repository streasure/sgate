package security

import (
	"regexp"
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// WAF Web 应用防火墙
// 功能：检测 SQL 注入、XSS 攻击、大 payload 拦截
type WAF struct {
	enabled        bool
	sqlPatterns    []*regexp.Regexp
	xssPatterns    []*regexp.Regexp
	maxPayloadSize int
	blockAction    string
	blockedCount   atomic.Int64
}

// 默认 SQL 注入特征
var defaultSQLPatterns = []string{
	`(?i)(\b(union)\b.*\b(select)\b)`,
	`(?i)(\b(select)\b.*\b(from)\b)`,
	`(?i)(\b(insert)\b.*\b(into)\b)`,
	`(?i)(\b(drop)\b.*\b(table)\b)`,
	`(?i)(\b(delete)\b.*\b(from)\b)`,
	`(?i)(\b(update)\b.*\b(set)\b)`,
	`(?i)('.*or.*'.*=.*'.*)`,
	`(?i)(--\s)`,
	`(?i)(/\*.*\*/)`,
	`(?i)(\bxp_cmdshell\b)`,
}

// 默认 XSS 特征
var defaultSSSPatterns = []string{
	`(?i)<script[^>]*>.*?</script>`,
	`(?i)javascript:`,
	`(?i)on(error|load|click|mouseover|focus|blur)\s*=`,
	`(?i)<iframe[^>]*>`,
	`(?i)<img[^>]+src[^>]+onerror`,
	`(?i)document\.cookie`,
	`(?i)eval\s*\(`,
	`(?i)expression\s*\(`,
}

// NewWAF 创建 WAF 实例
func NewWAF(cfg config.WAFConfig) *WAF {
	waf := &WAF{
		enabled:        cfg.Enabled,
		maxPayloadSize: cfg.MaxPayloadSize,
		blockAction:    cfg.BlockAction,
	}
	if waf.maxPayloadSize <= 0 {
		waf.maxPayloadSize = 1 * 1024 * 1024
	}
	if waf.blockAction == "" {
		waf.blockAction = "drop"
	}

	// 编译 SQL 注入特征
	// 若用户配置的 patterns 全部编译失败，fallback 到默认 patterns 避免静默失效
	patterns := cfg.SQLPatterns
	userSQLConfigured := len(patterns) > 0
	if !userSQLConfigured {
		patterns = defaultSQLPatterns
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			tlog.Warn("WAF: invalid SQL pattern, skipping", "pattern", p, "error", err)
			continue
		}
		waf.sqlPatterns = append(waf.sqlPatterns, re)
	}
	if userSQLConfigured && len(waf.sqlPatterns) == 0 {
		tlog.Warn("WAF: all user SQL patterns invalid, falling back to defaults")
		for _, p := range defaultSQLPatterns {
			if re, err := regexp.Compile(p); err == nil {
				waf.sqlPatterns = append(waf.sqlPatterns, re)
			}
		}
	}

	// 编译 XSS 特征
	xssPatterns := cfg.XSSPatterns
	userXSSConfigured := len(xssPatterns) > 0
	if !userXSSConfigured {
		xssPatterns = defaultSSSPatterns
	}
	for _, p := range xssPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			tlog.Warn("WAF: invalid XSS pattern, skipping", "pattern", p, "error", err)
			continue
		}
		waf.xssPatterns = append(waf.xssPatterns, re)
	}
	if userXSSConfigured && len(waf.xssPatterns) == 0 {
		tlog.Warn("WAF: all user XSS patterns invalid, falling back to defaults")
		for _, p := range defaultSSSPatterns {
			if re, err := regexp.Compile(p); err == nil {
				waf.xssPatterns = append(waf.xssPatterns, re)
			}
		}
	}

	tlog.Info("WAF initialized",
		"sqlPatterns", len(waf.sqlPatterns),
		"xssPatterns", len(waf.xssPatterns),
		"maxPayloadSize", waf.maxPayloadSize)

	return waf
}

// Inspect 检查 payload 是否包含攻击特征
// 返回 true 表示安全，false 表示检测到攻击
func (w *WAF) Inspect(data []byte) bool {
	if !w.enabled || len(data) == 0 {
		return true
	}

	// 大 payload 拦截
	if w.maxPayloadSize > 0 && len(data) > w.maxPayloadSize {
		w.blockedCount.Add(1)
		tlog.Warn("WAF: payload exceeds size limit", "size", len(data), "limit", w.maxPayloadSize)
		return false
	}

	// 快速检查：只在文本内容上做正则匹配
	// 对于二进制 protobuf 数据，跳过正则检查避免误报
	if !isLikelyText(data) {
		return true
	}

	str := string(data)

	// SQL 注入检查
	for _, re := range w.sqlPatterns {
		if re.MatchString(str) {
			w.blockedCount.Add(1)
			tlog.Warn("WAF: SQL injection detected", "pattern", re.String())
			return false
		}
	}

	// XSS 检查
	for _, re := range w.xssPatterns {
		if re.MatchString(str) {
			w.blockedCount.Add(1)
			tlog.Warn("WAF: XSS attack detected", "pattern", re.String())
			return false
		}
	}

	return true
}

// isLikelyText 判断数据是否为文本（非二进制 protobuf）
// protobuf 帧以 field tag 开头，文本则以可打印 ASCII/UTF-8 开头
func isLikelyText(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// 检查前 100 字节中可打印字符比例
	checkLen := len(data)
	if checkLen > 100 {
		checkLen = 100
	}
	printable := 0
	for i := 0; i < checkLen; i++ {
		b := data[i]
		if (b >= 0x20 && b <= 0x7E) || b == '\n' || b == '\r' || b == '\t' || b >= 0x80 {
			printable++
		}
	}
	return printable*100/checkLen > 80
}

// GetBlockedCount 获取拦截总数
func (w *WAF) GetBlockedCount() int64 {
	return w.blockedCount.Load()
}
