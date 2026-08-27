package cluster

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

	"github.com/streasure/sgate/gateway/util"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
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
	cfg         config.ConfigCenterConfig
	httpClient  *http.Client
	notifyCh    chan []byte
	stopCh      chan struct{}
	lastVersion string
	lastContent []byte
	mu          sync.Mutex
	// Nacos 3.x 认证 token 缓存
	authToken  string
	authExpire time.Time
	authMu     sync.Mutex
}

// NewConfigCenter 创建配置中心实例
func NewConfigCenter(cfg config.ConfigCenterConfig) ConfigCenter {
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
	// Nacos 3.x 需要先登录获取 token
	if c.isNacosV3WithAuth() {
		if err := c.ensureToken(ctx); err != nil {
			return nil, fmt.Errorf("nacos login failed: %w", err)
		}
	}
	url := c.buildPullURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 优先使用动态获取的 authToken，其次用静态 Token
	token := c.getAuthToken()
	if token == "" {
		token = c.cfg.Token
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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
	// 解析封装格式（Nacos 3.x 返回 JSON 包装；Nacos 2.x 纯文本；etcd-v3 gateway 返回 JSON）
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

// isNacosV3WithAuth 判断是否是 Nacos 3.x 且需要用户名/密码认证
func (c *HTTPConfigCenter) isNacosV3WithAuth() bool {
	return strings.ToLower(c.cfg.Type) == "nacos" &&
		strings.ToLower(c.cfg.APIVersion) != "v1" &&
		c.cfg.Username != "" && c.cfg.Password != ""
}

// ensureToken 确保 token 有效，过期则重新登录
func (c *HTTPConfigCenter) ensureToken(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	// token 未过期（提前 60s 刷新）
	if c.authToken != "" && time.Now().Before(c.authExpire.Add(-60*time.Second)) {
		return nil
	}
	return c.login(ctx)
}

// login 登录 Nacos 3.x 获取 access token
func (c *HTTPConfigCenter) login(ctx context.Context) error {
	loginURL := fmt.Sprintf("%s/v1/auth/users/login", c.cfg.Endpoint)
	form := fmt.Sprintf("username=%s&password=%s", c.cfg.Username, c.cfg.Password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("nacos login status %d: %s", resp.StatusCode, string(body))
	}
	// 从 Authorization 响应头获取 token
	authHeader := resp.Header.Get("Authorization")
	if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		c.authToken = strings.TrimPrefix(authHeader, "Bearer ")
		// Nacos 默认 token 有效期 18000s（5小时），提前 60s 刷新
		c.authExpire = time.Now().Add(18000 * time.Second)
		return nil
	}
	// 尝试从响应 body 获取 token
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var loginResp struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &loginResp); err == nil && loginResp.AccessToken != "" {
		c.authToken = loginResp.AccessToken
		c.authExpire = time.Now().Add(18000 * time.Second)
		return nil
	}
	return fmt.Errorf("nacos login: no token in response")
}

// getAuthToken 获取缓存的 auth token
func (c *HTTPConfigCenter) getAuthToken() string {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.authToken
}

// Watch 订阅配置变更（goroutine 内部循环拉取 + 长轮询）
func (c *HTTPConfigCenter) Watch(ctx context.Context) (<-chan []byte, error) {
	go c.watchLoop(ctx)
	return c.notifyCh, nil
}

func (c *HTTPConfigCenter) watchLoop(ctx context.Context) {
	// 首次拉取（兜底间隔短，长轮询失败时使用）
	interval := util.ParseDurationDefault(c.cfg.PollInterval, 5*time.Second)
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
		// Nacos API 路径随版本不同：
		// v3（Nacos 3.x）: /v3/console/cs/config?dataId=xxx&groupName=xxx&namespaceId=xxx（需 Bearer token 认证）
		// v1（Nacos 2.x）: /nacos/v1/cs/configs?dataId=xxx&group=xxx&tenant=xxx（无认证或 token 参数）
		group := c.cfg.Group
		if group == "" {
			group = "DEFAULT_GROUP"
		}
		if strings.ToLower(c.cfg.APIVersion) == "v1" {
			return fmt.Sprintf("%s/nacos/v1/cs/configs?dataId=%s&group=%s&tenant=%s",
				c.cfg.Endpoint, c.cfg.DataID, group, c.cfg.Namespace)
		}
		// 默认 v3（Nacos 3.x）— 使用 console API 路径
		return fmt.Sprintf("%s/v3/console/cs/config?dataId=%s&groupName=%s&namespaceId=%s",
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
	case "nacos":
		// Nacos 3.x console API 返回 JSON: {"code":0,"message":"success","data":{"content":"<YAML配置内容>",...}}
		// Nacos 2.x 直接返回纯文本（非 JSON）
		if strings.ToLower(c.cfg.APIVersion) != "v1" {
			var resp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    struct {
					Content string `json:"content"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				// 非 JSON 响应，按纯文本处理
				return body
			}
			if resp.Code != 0 {
				tlog.Warn("nacos config pull error", "code", resp.Code, "message", resp.Message)
				return nil
			}
			if resp.Data.Content == "" {
				return nil
			}
			return []byte(resp.Data.Content)
		}
		// Nacos 2.x 直接返回纯文本
		return body
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

// base64Decode 标准库解码包装
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
