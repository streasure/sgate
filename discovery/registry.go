package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

// NacosNamingConfig Nacos 服务发现配置
type NacosNamingConfig struct {
	// Endpoint Nacos 控制台地址（用于登录认证，如 http://127.0.0.1:8080）
	Endpoint string
	// NamingEndpoint Nacos Server API 端口地址（用于实例注册/查询，如 http://127.0.0.1:8848）
	// 若为空则回退到 Endpoint
	NamingEndpoint string
	// Namespace 命名空间 ID（默认 public）
	Namespace string
	// Group 分组名（默认 DEFAULT_GROUP）
	Group string
	// Username 认证用户名
	Username string
	// Password 认证密码
	Password string
	// APIVersion API 版本：v3（默认）或 v1
	APIVersion string
}

// ServiceRegistry 服务注册器（基于 Nacos naming API）
// logic server 启动时创建，定期向 Nacos 注册实例并维持心跳
type ServiceRegistry struct {
	cfg               NacosNamingConfig
	httpClient        *http.Client
	serviceInfo       *ServiceInfo
	heartbeatInterval time.Duration
	heartbeatTTL      time.Duration
	stopCh            chan struct{}
	wg                sync.WaitGroup
	// Nacos 3.x 认证 token 缓存
	authToken  string
	authExpire time.Time
	authMu     sync.Mutex
}

// NewServiceRegistry 创建服务注册器
// heartbeatInterval/heartbeatTTL 控制 Nacos 临时实例的心跳节奏
func NewServiceRegistry(serviceInfo *ServiceInfo, heartbeatInterval, heartbeatTTL time.Duration) *ServiceRegistry {
	if heartbeatInterval <= 0 {
		heartbeatInterval = DefaultHeartbeat
	}
	if heartbeatTTL <= 0 {
		heartbeatTTL = DefaultKeyTTL
	}
	return &ServiceRegistry{
		cfg:               NacosNamingConfig{},
		httpClient:        &http.Client{Timeout: 10 * time.Second},
		serviceInfo:       serviceInfo,
		heartbeatInterval: heartbeatInterval,
		heartbeatTTL:      heartbeatTTL,
		stopCh:            make(chan struct{}),
	}
}

// SetNacosConfig 注入 Nacos naming 配置
func (sr *ServiceRegistry) SetNacosConfig(cfg NacosNamingConfig) {
	sr.cfg = cfg
	if sr.cfg.Group == "" {
		sr.cfg.Group = "DEFAULT_GROUP"
	}
	if sr.cfg.APIVersion == "" {
		sr.cfg.APIVersion = "v3"
	}
}

// Start 向 Nacos 注册服务实例并启动心跳循环
func (sr *ServiceRegistry) Start() error {
	if sr.cfg.Endpoint == "" {
		tlog.Warn("nacos endpoint empty, skip service registration")
		return nil
	}
	if err := sr.registerInstance(); err != nil {
		return fmt.Errorf("register instance to nacos: %w", err)
	}

	sr.wg.Add(1)
	go sr.heartbeatLoop()

	tlog.Info("service registry started (nacos)",
		"serviceID", sr.serviceInfo.ServiceID,
		"serviceName", sr.serviceInfo.ServiceName,
		"address", sr.serviceInfo.Address,
		"endpoint", sr.cfg.Endpoint,
		"heartbeatInterval", sr.heartbeatInterval,
	)
	return nil
}

// Stop 注销服务实例并停止心跳
func (sr *ServiceRegistry) Stop() {
	close(sr.stopCh)
	sr.wg.Wait()

	if err := sr.deregisterInstance(); err != nil {
		tlog.Warn("deregister instance failed", "error", err)
	}

	tlog.Info("service deregistered (nacos)",
		"serviceID", sr.serviceInfo.ServiceID,
		"address", sr.serviceInfo.Address,
	)
}

// heartbeatLoop 定期重新注册实例维持心跳
// Nacos 3.x HTTP 注册的实例有 TTL，需定期 re-register
func (sr *ServiceRegistry) heartbeatLoop() {
	defer sr.wg.Done()
	ticker := time.NewTicker(sr.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sr.stopCh:
			return
		case <-ticker.C:
			if err := sr.registerInstance(); err != nil {
				tlog.Error("heartbeat register failed", "error", err)
			}
		}
	}
}

// namingEndpoint 返回用于实例注册/查询的 Nacos 地址
// 优先使用 NamingEndpoint（主端口），否则回退到 Endpoint
func (sr *ServiceRegistry) namingEndpoint() string {
	if sr.cfg.NamingEndpoint != "" {
		return sr.cfg.NamingEndpoint
	}
	return sr.cfg.Endpoint
}

// registerInstance 向 Nacos 注册服务实例
// Nacos 3.x: POST {NamingEndpoint}/nacos/v3/client/ns/instance
// Nacos 2.x: POST {Endpoint}/nacos/v1/ns/instance
func (sr *ServiceRegistry) registerInstance() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := sr.ensureToken(ctx)
	if err != nil {
		// 认证失败时尝试无 token 注册（某些 Nacos 部署关闭了认证）
		tlog.Debug("nacos auth failed, trying without token", "error", err)
	}

	ip, port := sr.parseAddrPort()
	metadataJSON, _ := json.Marshal(sr.serviceInfo.Metadata)

	form := url.Values{}
	form.Set("serviceName", sr.serviceInfo.ServiceName)
	form.Set("ip", ip)
	form.Set("port", port)
	form.Set("weight", fmt.Sprintf("%d", sr.serviceInfo.Weight))
	form.Set("metadata", string(metadataJSON))
	form.Set("clusterName", "DEFAULT")
	form.Set("groupName", sr.cfg.Group)
	form.Set("namespaceId", sr.cfg.Namespace)
	form.Set("ephemeral", "true")
	form.Set("heartBeat", "false")

	var reqURL string
	if strings.ToLower(sr.cfg.APIVersion) == "v1" {
		// Nacos 2.x: /nacos/v1/ns/instance
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance", sr.cfg.Endpoint)
	} else {
		// Nacos 3.x: 客户端 API 走主端口 /nacos/v3/client/ns/instance
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance", sr.namingEndpoint())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := sr.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nacos register status %d", resp.StatusCode)
	}
	return nil
}

// deregisterInstance 从 Nacos 注销服务实例
// Nacos 3.x: DELETE {NamingEndpoint}/nacos/v3/client/ns/instance
// Nacos 2.x: DELETE {Endpoint}/nacos/v1/ns/instance
func (sr *ServiceRegistry) deregisterInstance() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, _ := sr.ensureToken(ctx)
	ip, port := sr.parseAddrPort()

	q := url.Values{}
	q.Set("serviceName", sr.serviceInfo.ServiceName)
	q.Set("ip", ip)
	q.Set("port", port)
	q.Set("groupName", sr.cfg.Group)
	q.Set("namespaceId", sr.cfg.Namespace)
	q.Set("clusterName", "DEFAULT")

	var reqURL string
	if strings.ToLower(sr.cfg.APIVersion) == "v1" {
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance?%s", sr.cfg.Endpoint, q.Encode())
	} else {
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance?%s", sr.namingEndpoint(), q.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := sr.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// parseAddrPort 从 address（host:port）中解析出 ip 和 port
func (sr *ServiceRegistry) parseAddrPort() (string, string) {
	addr := sr.serviceInfo.Address
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "0"
	}
	return addr[:idx], addr[idx+1:]
}

// ensureToken 获取 Nacos 3.x 认证 token（复用 config_center 的登录逻辑）
func (sr *ServiceRegistry) ensureToken(ctx context.Context) (string, error) {
	if sr.cfg.Username == "" || sr.cfg.Password == "" {
		return "", nil
	}
	sr.authMu.Lock()
	defer sr.authMu.Unlock()
	if sr.authToken != "" && time.Now().Before(sr.authExpire.Add(-60*time.Second)) {
		return sr.authToken, nil
	}

	loginURL := fmt.Sprintf("%s/v1/auth/users/login", sr.cfg.Endpoint)
	form := fmt.Sprintf("username=%s&password=%s", sr.cfg.Username, sr.cfg.Password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sr.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nacos login status %d", resp.StatusCode)
	}
	authHeader := resp.Header.Get("Authorization")
	if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		sr.authToken = strings.TrimPrefix(authHeader, "Bearer ")
		sr.authExpire = time.Now().Add(18000 * time.Second)
		return sr.authToken, nil
	}
	return "", fmt.Errorf("no token in login response")
}
