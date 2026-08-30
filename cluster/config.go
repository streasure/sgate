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

	"github.com/streasure/util/nacos"
	"github.com/streasure/sgate/util"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// ConfigCenter 配置中心接口
type ConfigCenter interface {
	Watch(ctx context.Context) (<-chan []byte, error)
	Pull(ctx context.Context) ([]byte, error)
	Type() string
	Stop()
}

// NewConfigCenter 创建配置中心实例
// Nacos 类型委托 util/nacos.ConfigCenter；其他类型走 HTTP 实现
func NewConfigCenter(cfg config.ConfigCenterConfig) ConfigCenter {
	if !cfg.Enabled {
		return nil
	}
	if strings.ToLower(cfg.Type) == "nacos" {
		return newNacosConfigCenter(cfg)
	}
	return newHTTPConfigCenter(cfg)
}

// --- Nacos 后端：委托 util/nacos.ConfigCenter ---

type nacosConfigCenter struct {
	inner *nacos.ConfigCenter
	cfg   config.ConfigCenterConfig
}

func newNacosConfigCenter(cfg config.ConfigCenterConfig) *nacosConfigCenter {
	inner := nacos.NewConfigCenter(nacos.ConfigCenterConfig{
		Enabled: true,
		Nacos: nacos.Config{
			Endpoint:   cfg.Endpoint,
			Namespace:  cfg.Namespace,
			DataID:     cfg.DataID,
			Group:      cfg.Group,
			Username:   cfg.Username,
			Password:   cfg.Password,
			APIVersion: cfg.APIVersion,
			PollInterval: cfg.PollInterval,
		},
	})
	return &nacosConfigCenter{inner: inner, cfg: cfg}
}

func (c *nacosConfigCenter) Type() string { return "nacos" }

func (c *nacosConfigCenter) Pull(ctx context.Context) ([]byte, error) {
	return c.inner.Pull()
}

func (c *nacosConfigCenter) Watch(ctx context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, 4)
	c.inner.OnConfigChange(func(data []byte) {
		select {
		case ch <- data:
		default:
		}
	})
	if err := c.inner.Start(); err != nil {
		return nil, err
	}
	return ch, nil
}

func (c *nacosConfigCenter) Stop() {
	c.inner.Destroy()
}

// --- 其他后端（Apollo/etcd/Consul/HTTP）：保留原有实现 ---

type httpConfigCenter struct {
	cfg         config.ConfigCenterConfig
	httpClient  *http.Client
	notifyCh    chan []byte
	stopCh      chan struct{}
	stopOnce    sync.Once
	lastVersion string
	lastContent []byte
	mu          sync.Mutex
	// Nacos 3.x 认证 token 缓存
	authToken  string
	authExpire time.Time
	authMu     sync.Mutex
}

func newHTTPConfigCenter(cfg config.ConfigCenterConfig) *httpConfigCenter {
	return &httpConfigCenter{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 35 * time.Second},
		notifyCh:   make(chan []byte, 4),
		stopCh:     make(chan struct{}),
	}
}

func (c *httpConfigCenter) Type() string { return c.cfg.Type }

func (c *httpConfigCenter) Pull(ctx context.Context) ([]byte, error) {
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
	content := c.unwrap(body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastVersion != "" && c.lastVersion == hashContent(content) {
		return content, nil
	}
	c.lastVersion = hashContent(content)
	c.lastContent = content
	return content, nil
}

func (c *httpConfigCenter) isNacosV3WithAuth() bool {
	return strings.ToLower(c.cfg.Type) == "nacos" &&
		strings.ToLower(c.cfg.APIVersion) != "v1" &&
		c.cfg.Username != "" && c.cfg.Password != ""
}

func (c *httpConfigCenter) ensureToken(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authToken != "" && time.Now().Before(c.authExpire.Add(-60*time.Second)) {
		return nil
	}
	return c.login(ctx)
}

func (c *httpConfigCenter) login(ctx context.Context) error {
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
	authHeader := resp.Header.Get("Authorization")
	if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		c.authToken = strings.TrimPrefix(authHeader, "Bearer ")
		c.authExpire = time.Now().Add(18000 * time.Second)
		return nil
	}
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

func (c *httpConfigCenter) getAuthToken() string {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.authToken
}

func (c *httpConfigCenter) Watch(ctx context.Context) (<-chan []byte, error) {
	go c.watchLoop(ctx)
	return c.notifyCh, nil
}

func (c *httpConfigCenter) watchLoop(ctx context.Context) {
	interval := util.ParseDurationDefault(c.cfg.PollInterval, 5*time.Second)
	if interval < time.Second {
		interval = time.Second
	}
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

func (c *httpConfigCenter) buildPullURL() string {
	switch strings.ToLower(c.cfg.Type) {
	case "nacos":
		group := c.cfg.Group
		if group == "" {
			group = "DEFAULT_GROUP"
		}
		if strings.ToLower(c.cfg.APIVersion) == "v1" {
			return fmt.Sprintf("%s/nacos/v1/cs/configs?dataId=%s&group=%s&tenant=%s",
				c.cfg.Endpoint, c.cfg.DataID, group, c.cfg.Namespace)
		}
		return fmt.Sprintf("%s/v3/console/cs/config?dataId=%s&groupName=%s&namespaceId=%s",
			c.cfg.Endpoint, c.cfg.DataID, group, c.cfg.Namespace)
	case "apollo":
		cluster := c.cfg.Group
		if cluster == "" {
			cluster = "default"
		}
		return fmt.Sprintf("%s/config/%s/%s/%s",
			c.cfg.Endpoint, c.cfg.DataID, cluster, c.cfg.Namespace)
	case "etcd":
		return c.cfg.Endpoint + "/v3/kv/range"
	case "consul":
		return fmt.Sprintf("%s/v1/kv/%s?raw", c.cfg.Endpoint, c.cfg.DataID)
	default:
		return c.cfg.Endpoint
	}
}

func (c *httpConfigCenter) unwrap(body []byte) []byte {
	switch strings.ToLower(c.cfg.Type) {
	case "nacos":
		if strings.ToLower(c.cfg.APIVersion) != "v1" {
			var resp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    struct {
					Content string `json:"content"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
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
		return body
	case "etcd":
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
		decoded, err := base64.StdEncoding.DecodeString(resp.Kvs[0].Value)
		if err != nil {
			return body
		}
		return decoded
	default:
		return body
	}
}

func (c *httpConfigCenter) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

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
