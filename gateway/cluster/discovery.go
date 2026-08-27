package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/streasure/sgate/discovery"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// ServiceChangeCallback 服务变更回调函数类型
type ServiceChangeCallback func(event discovery.ServiceEvent)

// ServiceDiscovery 服务发现消费者（基于 Nacos naming API）
// 网关启动时创建，定期从 Nacos 拉取 logic server 实例列表并通知回调
type ServiceDiscovery struct {
	cfg        config.DiscoveryConfig
	nacosCfg   discovery.NacosNamingConfig
	httpClient *http.Client
	services   map[string]*discovery.ServiceInfo
	mu         sync.RWMutex
	callbacks  []ServiceChangeCallback
	stopCh     chan struct{}
	wg         sync.WaitGroup
	// Nacos 3.x 认证 token 缓存
	authToken  string
	authExpire time.Time
	authMu     sync.Mutex
}

// NewServiceDiscovery 创建服务发现消费者
func NewServiceDiscovery(cfg config.DiscoveryConfig) *ServiceDiscovery {
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = discovery.DefaultScan
	}
	if cfg.DeregisterDelay <= 0 {
		cfg.DeregisterDelay = discovery.DefaultDeregister
	}
	return &ServiceDiscovery{
		cfg:        cfg,
		services:   make(map[string]*discovery.ServiceInfo),
		callbacks:  make([]ServiceChangeCallback, 0),
		stopCh:     make(chan struct{}),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetNacosConfig 注入 Nacos naming 配置
func (sd *ServiceDiscovery) SetNacosConfig(cfg discovery.NacosNamingConfig) {
	sd.nacosCfg = cfg
	if sd.nacosCfg.Group == "" {
		sd.nacosCfg.Group = "DEFAULT_GROUP"
	}
	if sd.nacosCfg.APIVersion == "" {
		sd.nacosCfg.APIVersion = "v3"
	}
}

// Start 启动服务发现：立即拉取一次 + 定期轮询
func (sd *ServiceDiscovery) Start() error {
	if sd.nacosCfg.Endpoint == "" {
		return fmt.Errorf("nacos endpoint empty, service discovery disabled")
	}
	if err := sd.pullServices(); err != nil {
		tlog.Warn("initial service pull failed", "error", err)
	}
	sd.wg.Add(1)
	go sd.scanLoop()

	tlog.Info("service discovery started (nacos)",
		"serviceName", sd.cfg.ServiceName,
		"endpoint", sd.nacosCfg.Endpoint,
		"group", sd.nacosCfg.Group,
		"scanInterval", sd.cfg.ScanInterval,
	)
	return nil
}

// Stop 停止服务发现
func (sd *ServiceDiscovery) Stop() {
	close(sd.stopCh)
	sd.wg.Wait()
	tlog.Info("service discovery stopped")
}

// OnServiceChange 注册服务变更回调
func (sd *ServiceDiscovery) OnServiceChange(callback ServiceChangeCallback) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.callbacks = append(sd.callbacks, callback)
}

// GetServices 返回当前所有服务实例快照
func (sd *ServiceDiscovery) GetServices() []*discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	result := make([]*discovery.ServiceInfo, 0, len(sd.services))
	for _, svc := range sd.services {
		result = append(result, svc)
	}
	return result
}

// GetService 根据 serviceID 查询服务实例
func (sd *ServiceDiscovery) GetService(serviceID string) *discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	return sd.services[serviceID]
}

// GetServiceByAddress 根据地址查询服务实例
func (sd *ServiceDiscovery) GetServiceByAddress(address string) *discovery.ServiceInfo {
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, svc := range sd.services {
		if svc.Address == address {
			return svc
		}
	}
	return nil
}

// shouldAcceptZone 判断服务是否属于本网关 zone
func (sd *ServiceDiscovery) shouldAcceptZone(meta map[string]string) bool {
	if sd.cfg.Zone == "" {
		return true
	}
	svcZone := meta["zone"]
	if svcZone == "" {
		svcZone = "default"
	}
	return svcZone == sd.cfg.Zone
}

// scanLoop 定期拉取服务列表
func (sd *ServiceDiscovery) scanLoop() {
	defer sd.wg.Done()
	ticker := time.NewTicker(sd.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sd.stopCh:
			return
		case <-ticker.C:
			if err := sd.pullServices(); err != nil {
				tlog.Error("service pull failed", "error", err)
			}
		}
	}
}

// namingEndpoint 返回用于实例查询的 Nacos 地址
// 优先使用 NamingEndpoint（主端口），否则回退到 Endpoint
func (sd *ServiceDiscovery) namingEndpoint() string {
	if sd.nacosCfg.NamingEndpoint != "" {
		return sd.nacosCfg.NamingEndpoint
	}
	return sd.nacosCfg.Endpoint
}

// pullServices 从 Nacos 拉取服务实例列表并对比变更
// Nacos 3.x: GET {NamingEndpoint}/nacos/v3/client/ns/instance/list
// Nacos 2.x: GET {Endpoint}/nacos/v1/ns/instance/list
func (sd *ServiceDiscovery) pullServices() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, _ := sd.ensureToken(ctx)

	q := url.Values{}
	q.Set("serviceName", sd.cfg.ServiceName)
	q.Set("groupName", sd.nacosCfg.Group)
	q.Set("namespaceId", sd.nacosCfg.Namespace)

	var reqURL string
	if strings.ToLower(sd.nacosCfg.APIVersion) == "v1" {
		// Nacos 2.x
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance/list?%s",
			sd.nacosCfg.Endpoint, q.Encode())
	} else {
		// Nacos 3.x: 客户端 API 走主端口 /nacos/v3/client/ns/instance/list
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance/list?%s",
			sd.namingEndpoint(), q.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := sd.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nacos list status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	instances, err := sd.parseInstances(body)
	if err != nil {
		return err
	}

	sd.reconcile(instances)
	return nil
}

// nacosInstance Nacos 实例 JSON 结构
type nacosInstance struct {
	InstanceID string            `json:"instanceId"`
	IP         string            `json:"ip"`
	Port       int               `json:"port"`
	Weight     float64           `json:"weight"`
	Metadata   map[string]string `json:"metadata"`
	Healthy    bool              `json:"healthy"`
	Enabled    bool              `json:"enabled"`
}

// nacosListResponse Nacos 列表响应（兼容 v1/v3）
type nacosListResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// parseInstances 解析 Nacos 响应为 ServiceInfo 列表
// 兼容三种响应格式：
//   - Nacos 3.x 客户端 API: {"code":0,"data":[{instance}, ...]}      （data 为实例数组）
//   - Nacos 3.x 控制台 API: {"code":0,"data":{"pageItems":[...]}}    （data 为分页对象）
//   - Nacos 2.x API:        {"instances":[...]}                       （顶层 instances 数组）
func (sd *ServiceDiscovery) parseInstances(body []byte) ([]*discovery.ServiceInfo, error) {
	var wrapper nacosListResponse
	var instances []nacosInstance

	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Data) > 0 {
		// 情况 1：data 是实例数组（3.x 客户端 API）
		if arrErr := json.Unmarshal(wrapper.Data, &instances); arrErr == nil && instances != nil {
			// instances 已填充
		} else {
			// 情况 2：data 是带 pageItems 的分页对象（3.x 控制台 API）
			var paged struct {
				Instances []nacosInstance `json:"instances"`
				PageItems []nacosInstance `json:"pageItems"`
			}
			if err := json.Unmarshal(wrapper.Data, &paged); err != nil {
				return nil, fmt.Errorf("unmarshal data: %w", err)
			}
			instances = append(instances, paged.Instances...)
			instances = append(instances, paged.PageItems...)
		}
	} else {
		// 情况 3：v1 顶层 instances 数组
		var data struct {
			Instances []nacosInstance `json:"instances"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("unmarshal v1 response: %w", err)
		}
		instances = data.Instances
	}

	result := make([]*discovery.ServiceInfo, 0, len(instances))
	for _, inst := range instances {
		if !inst.Healthy || !inst.Enabled {
			continue
		}
		// 使用 IP:Port 作为唯一 ServiceID（Nacos 的 instanceId 在 v3 中可能为空）
		instanceID := inst.InstanceID
		if instanceID == "" {
			instanceID = fmt.Sprintf("%s:%d", inst.IP, inst.Port)
		}
		svc := &discovery.ServiceInfo{
			ServiceID:   instanceID,
			ServiceName: sd.cfg.ServiceName,
			Address:     fmt.Sprintf("%s:%d", inst.IP, inst.Port),
			Weight:      int(inst.Weight),
			Metadata:    inst.Metadata,
		}
		if !sd.shouldAcceptZone(svc.Metadata) {
			continue
		}
		result = append(result, svc)
	}
	return result, nil
}

// reconcile 对比新拉取的服务列表与本地缓存，触发注册/注销回调
func (sd *ServiceDiscovery) reconcile(newServices []*discovery.ServiceInfo) {
	activeMap := make(map[string]*discovery.ServiceInfo, len(newServices))
	for _, svc := range newServices {
		activeMap[svc.ServiceID] = svc
	}

	sd.mu.Lock()
	oldServices := make(map[string]*discovery.ServiceInfo, len(sd.services))
	for k, v := range sd.services {
		oldServices[k] = v
	}

	// 更新本地缓存为最新快照
	sd.services = make(map[string]*discovery.ServiceInfo, len(newServices))
	for id, svc := range activeMap {
		sd.services[id] = svc
	}
	sd.mu.Unlock()

	// 通知新增
	for id, svc := range activeMap {
		if _, existed := oldServices[id]; !existed {
			tlog.Info("service registered",
				"serviceID", svc.ServiceID,
				"address", svc.Address,
				"serviceName", svc.ServiceName,
			)
			sd.notifyCallbacks(discovery.ServiceEvent{
				Type:      discovery.EventRegister,
				Service:   *svc,
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}

	// 通知注销
	for id, svc := range oldServices {
		if _, exists := activeMap[id]; !exists {
			tlog.Warn("service deregistered",
				"serviceID", svc.ServiceID,
				"address", svc.Address,
			)
			sd.notifyCallbacks(discovery.ServiceEvent{
				Type:      discovery.EventDeregister,
				Service:   *svc,
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}
}

// notifyCallbacks 通知所有回调（panic 安全）
func (sd *ServiceDiscovery) notifyCallbacks(event discovery.ServiceEvent) {
	sd.mu.RLock()
	callbacks := make([]ServiceChangeCallback, len(sd.callbacks))
	copy(callbacks, sd.callbacks)
	sd.mu.RUnlock()

	for _, cb := range callbacks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					tlog.Error("notifyCallback panic recovered", "error", r)
				}
			}()
			cb(event)
		}()
	}
}

// ensureToken 获取 Nacos 3.x 认证 token
func (sd *ServiceDiscovery) ensureToken(ctx context.Context) (string, error) {
	if sd.nacosCfg.Username == "" || sd.nacosCfg.Password == "" {
		return "", nil
	}
	sd.authMu.Lock()
	defer sd.authMu.Unlock()
	if sd.authToken != "" && time.Now().Before(sd.authExpire.Add(-60*time.Second)) {
		return sd.authToken, nil
	}

	loginURL := fmt.Sprintf("%s/v1/auth/users/login", sd.nacosCfg.Endpoint)
	form := fmt.Sprintf("username=%s&password=%s", sd.nacosCfg.Username, sd.nacosCfg.Password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sd.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nacos login status %d", resp.StatusCode)
	}
	authHeader := resp.Header.Get("Authorization")
	if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		sd.authToken = strings.TrimPrefix(authHeader, "Bearer ")
		sd.authExpire = time.Now().Add(18000 * time.Second)
		return sd.authToken, nil
	}
	return "", fmt.Errorf("no token in login response")
}
