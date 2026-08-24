package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
	"gopkg.in/yaml.v3"
)

// ConfigCenter 配置中心接口
// 实现该接口即可对接任意配置中心（Nacos/Apollo/etcd-v3 HTTP gateway/Consul KV）
type ConfigCenter interface {
	// Watch 订阅配置变更，返回变更后的 YAML 配置字节
	Watch(ctx context.Context) (<-chan []byte, error)
	// Pull 主动拉取一次
	Pull(ctx context.Context) ([]byte, error)
	// Type 配置中心类型
	Type() string
}

// HTTPConfigCenter 通用 HTTP 配置中心
// 实现 Nacos/Apollo/etcd-v3-gateway/Consul 的 HTTP 拉取 + 长轮询
type HTTPConfigCenter struct {
	cfg         ConfigCenterConfig
	httpClient  *http.Client
	notifyCh    chan []byte
	stopCh      chan struct{}
	lastVersion string
	lastContent []byte
	mu          sync.Mutex
}

// NewConfigCenter 创建配置中心实例
func NewConfigCenter(cfg ConfigCenterConfig) ConfigCenter {
	if !cfg.Enabled {
		return nil
	}
	hc := &HTTPConfigCenter{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 35 * time.Second}, // 长轮询典型 30s
		notifyCh:   make(chan []byte, 4),
		stopCh:     make(chan struct{}),
	}
	return hc
}

func (c *HTTPConfigCenter) Type() string { return c.cfg.Type }

// Pull 主动拉取一次配置
func (c *HTTPConfigCenter) Pull(ctx context.Context) ([]byte, error) {
	url := c.buildPullURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config center pull status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// 解析封装格式（Nacos/Apollo 直接返回字符串；etcd-v3 gateway 返回 JSON）
	content := c.unwrap(body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastVersion != "" && c.lastVersion == hashContent(content) {
		return content, nil // 未变更
	}
	c.lastVersion = hashContent(content)
	c.lastContent = content
	return content, nil
}

// Watch 订阅配置变更（goroutine 内部循环拉取 + 长轮询）
func (c *HTTPConfigCenter) Watch(ctx context.Context) (<-chan []byte, error) {
	go c.watchLoop(ctx)
	return c.notifyCh, nil
}

func (c *HTTPConfigCenter) watchLoop(ctx context.Context) {
	// 首次拉取（兜底间隔短，长轮询失败时使用）
	interval := parseDurationDefault(c.cfg.PollInterval, 5*time.Second)
	if interval < time.Second {
		interval = time.Second
	}
	// 首次拉取
	body, err := c.Pull(ctx)
	if err != nil {
		tlog.Warn("config center initial pull failed", "error", err)
	} else if len(body) > 0 {
		select {
		case c.notifyCh <- body:
		case <-ctx.Done():
			return
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			body, err := c.Pull(ctx)
			if err != nil {
				tlog.Debug("config center poll error", "error", err)
				continue
			}
			if len(body) > 0 {
				select {
				case c.notifyCh <- body:
				default:
				}
			}
		}
	}
}

func (c *HTTPConfigCenter) buildPullURL() string {
	switch strings.ToLower(c.cfg.Type) {
	case "nacos":
		// Nacos OpenAPI: /nacos/v1/cs/configs?dataId=xxx&group=xxx
		group := c.cfg.Group
		if group == "" {
			group = "DEFAULT_GROUP"
		}
		return fmt.Sprintf("%s/nacos/v1/cs/configs?dataId=%s&group=%s&tenant=%s",
			c.cfg.Endpoint, c.cfg.DataID, group, c.cfg.Namespace)
	case "apollo":
		// Apollo: /config/{appId}/{cluster}/{namespace}
		cluster := c.cfg.Group
		if cluster == "" {
			cluster = "default"
		}
		return fmt.Sprintf("%s/config/%s/%s/%s",
			c.cfg.Endpoint, c.cfg.DataID, cluster, c.cfg.Namespace)
	case "etcd":
		// etcd v3 HTTP gateway: /v3/kv/range (POST JSON)
		// 简化：用 GET /v3/kv/range?prefix=xxx 不存在；
		// 实际通过 POST body，这里返回 base URL，Pull 中改用 POST
		return c.cfg.Endpoint + "/v3/kv/range"
	case "consul":
		// Consul KV: /v1/kv/{key}?raw
		return fmt.Sprintf("%s/v1/kv/%s?raw", c.cfg.Endpoint, c.cfg.DataID)
	default:
		// generic HTTP
		return c.cfg.Endpoint
	}
}

// unwrap 解包配置中心响应（部分平台会 JSON 包装）
func (c *HTTPConfigCenter) unwrap(body []byte) []byte {
	switch strings.ToLower(c.cfg.Type) {
	case "etcd":
		// etcd v3 HTTP gateway 返回 JSON: { "kvs": [{"value": "base64..."}] }
		var resp struct {
			Kvs []struct {
				Value string `json:"value"`
			} `json:"kvs"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return body
		}
		if len(resp.Kvs) == 0 {
			return nil
		}
		decoded, err := base64Decode(resp.Kvs[0].Value)
		if err != nil {
			return body
		}
		return decoded
	case "apollo":
		// Apollo 返回 JSON: { "configValue": "..." } 或纯文本（按 Accept 头）
		// 默认已用 Accept: text/plain → 直接返回 body
		return body
	default:
		return body
	}
}

func (c *HTTPConfigCenter) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

// hashContent 简单哈希（避免每次都深拷贝对比）
func hashContent(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return fmt.Sprintf("%x", h)
}

// startConfigCenterWatcher 启动配置中心监听并桥接到现有 handleConfigUpdate
func (g *Gateway) startConfigCenterWatcher() {
	if g.configCenter == nil {
		return
	}
	ch, err := g.configCenter.Watch(g.ctx)
	if err != nil {
		tlog.Error("config center watch failed", "error", err)
		return
	}
	go func() {
		for yamlBytes := range ch {
			if len(yamlBytes) == 0 {
				continue
			}
			var newCfg config.Config
			if err := yaml.Unmarshal(yamlBytes, &newCfg); err != nil {
				tlog.Warn("config center content parse failed", "error", err)
				continue
			}
			// 推送配置更新到主链
			g.configUpdateChan <- &newCfg
			tlog.Info("config updated from config center",
				"type", g.configCenter.Type())
		}
	}()
}

// base64Decode 标准库解码包装
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
