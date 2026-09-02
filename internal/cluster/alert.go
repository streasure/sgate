package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
)

// AlertLevel 告警级别
type AlertLevel string

const (
	AlertInfo  AlertLevel = "info"
	AlertWarn  AlertLevel = "warn"
	AlertError AlertLevel = "error"
	AlertFatal AlertLevel = "fatal"
)

// AlertEvent 告警事件
type AlertEvent struct {
	Level     AlertLevel             `json:"level"`
	Title     string                 `json:"title"`
	Content   string                 `json:"content"`
	Source    string                 `json:"source"`
	Timestamp int64                  `json:"timestamp"`
	Metrics   map[string]interface{} `json:"metrics,omitempty"`
}

// AlertWebhook 告警通知器
// 支持：企业微信群机器人、钉钉群机器人、通用 Webhook
type AlertWebhook struct {
	mu          sync.RWMutex
	webhooks    []webhookConfig
	httpClient  *http.Client
	enabled     atomic.Int32
	rateLimit   int // 每分钟最大告警数
	sent        atomic.Int64
	dropped     atomic.Int64
	lastSentMin atomic.Int64
}

type webhookConfig struct {
	name   string
	url    string
	typ    string // "wecom" / "dingtalk" / "generic"
	secret string // 钉钉加签密钥
}

// NewAlertWebhook 创建告警通知器
func NewAlertWebhook(cfg config.AlertWebhookConfig) *AlertWebhook {
	a := &AlertWebhook{
		httpClient: &http.Client{Timeout: 3 * time.Second},
		rateLimit:  cfg.RateLimit,
	}
	if cfg.Enabled {
		a.enabled.Store(1)
	}
	if a.rateLimit <= 0 {
		a.rateLimit = 30
	}
	for _, w := range cfg.Webhooks {
		a.webhooks = append(a.webhooks, webhookConfig{
			name:   w.Name,
			url:    w.URL,
			typ:    w.Type,
			secret: w.Secret,
		})
	}
	return a
}

// Send 发送告警
func (a *AlertWebhook) Send(ctx context.Context, event AlertEvent) error {
	if a.enabled.Load() == 0 {
		return nil
	}
	// 限流：每分钟 rateLimit 条
	min := time.Now().Unix() / 60
	last := a.lastSentMin.Load()
	if min != last {
		a.lastSentMin.Store(min)
		a.sent.Store(0)
	}
	if int(a.sent.Load()) >= a.rateLimit {
		a.dropped.Add(1)
		return fmt.Errorf("alert rate limited")
	}
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().Unix()
	}
	if event.Source == "" {
		event.Source = "sgate"
	}
	a.mu.RLock()
	hooks := a.webhooks
	a.mu.RUnlock()
	for _, h := range hooks {
		payload := a.buildPayload(h, event)
		go a.sendOne(h, payload)
	}
	a.sent.Add(1)
	return nil
}

func (a *AlertWebhook) sendOne(h webhookConfig, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		tlog.Warn("alert webhook send failed",
			"webhook", h.name,
			"error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		tlog.Warn("alert webhook non-2xx",
			"webhook", h.name,
			"status", resp.StatusCode)
	}
}

// buildPayload 按平台构造 payload
func (a *AlertWebhook) buildPayload(h webhookConfig, event AlertEvent) []byte {
	switch h.typ {
	case "wecom":
		// 企业微信群机器人：{ "msgtype": "markdown", "markdown": { "content": "..." } }
		color := "info"
		switch event.Level {
		case AlertWarn:
			color = "warning"
		case AlertError, AlertFatal:
			color = "warning"
		}
		content := fmt.Sprintf("## %s\n> **级别**: %s\n> **来源**: %s\n> **时间**: %s\n> **内容**: %s",
			event.Title, event.Level, event.Source,
			time.Unix(event.Timestamp, 0).Format("2006-01-02 15:04:05"),
			event.Content)
		body, _ := json.Marshal(map[string]interface{}{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": content},
		})
		_ = color
		return body
	case "dingtalk":
		// 钉钉群机器人：{ "msgtype": "markdown", "markdown": { "title": "...", "text": "..." } }
		text := fmt.Sprintf("### %s\n- 级别: %s\n- 来源: %s\n- 时间: %s\n- 内容: %s",
			event.Title, event.Level, event.Source,
			time.Unix(event.Timestamp, 0).Format("2006-01-02 15:04:05"),
			event.Content)
		body, _ := json.Marshal(map[string]interface{}{
			"msgtype":  "markdown",
			"markdown": map[string]string{"title": event.Title, "text": text},
		})
		return body
	default:
		// 通用 webhook
		body, _ := json.Marshal(event)
		return body
	}
}

// Stats 告警统计
func (a *AlertWebhook) Stats() (sent, dropped int64) {
	return a.sent.Load(), a.dropped.Load()
}
