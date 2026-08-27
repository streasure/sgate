package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/discovery"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// Cluster 集群管理器（基于 Nacos）
// 功能：
//   - 网关节点注册为 Nacos 临时实例（心跳维持存活）
//   - Leader 选举：同 zone 内按实例地址（ip:port）字典序排序，排名第一者为 Leader
//   - 节点下线/故障后临时实例过期消失，剩余节点自动接管（双机热备/自动容灾）
//
// 选举正确性：所有节点看到相同的实例列表（Nacos 最终一致），
// 排序结果一致 → 收敛到同一个 Leader，无需分布式锁竞争。
type Cluster struct {
	nacosCfg      discovery.NacosNamingConfig
	httpClient    *http.Client
	cfg           config.ClusterConfig
	zone          string
	nodeID        string
	advertiseAddr string // 本节点注册地址 ip:port
	isLeader      atomic.Int32
	stopChan      chan struct{}
	stopOnce      sync.Once
	ttl           time.Duration
	renewInterval time.Duration
	// Nacos 3.x 认证 token 缓存
	authToken  string
	authExpire time.Time
	authMu     sync.Mutex
}

// NewCluster 创建集群管理器
// advertisePort 为本节点对外暴露的端口（用于生成 ip:port 实例地址，同机多实例靠端口区分）
func NewCluster(cfg config.ClusterConfig, nacosCfg discovery.NacosNamingConfig, zone string, advertisePort int) *Cluster {
	nodeID := cfg.NodeID
	if nodeID == "" {
		hostname, _ := os.Hostname()
		nodeID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	if nacosCfg.Group == "" {
		nacosCfg.Group = "DEFAULT_GROUP"
	}
	if nacosCfg.APIVersion == "" {
		nacosCfg.APIVersion = "v3"
	}

	ttl := 10 * time.Second
	if d, err := time.ParseDuration(cfg.LockTTL); err == nil && d > 0 {
		ttl = d
	}

	return &Cluster{
		nacosCfg:      nacosCfg,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		cfg:           cfg,
		zone:          zone,
		nodeID:        nodeID,
		advertiseAddr: fmt.Sprintf("%s:%d", localIP(), advertisePort),
		stopChan:      make(chan struct{}),
		ttl:           ttl,
		renewInterval: ttl / 3,
	}
}

// Start 启动集群：注册节点 + 心跳续期 + Leader 选举
func (c *Cluster) Start(ctx context.Context) {
	if err := c.registerNode(); err != nil {
		tlog.Error("cluster node register failed", "error", err)
	}

	// 启动心跳续期
	go c.heartbeatLoop()

	// 启动 Leader 选举（如果配置开启）
	if c.cfg.LeaderElection {
		go c.leaderElectionLoop()
	}

	tlog.Info("cluster started (nacos)",
		"nodeID", c.nodeID,
		"zone", c.zone,
		"address", c.advertiseAddr,
		"leaderElection", c.cfg.LeaderElection,
	)
}

// registerNode 将本节点注册为 Nacos 临时实例
// Nacos 3.x: POST {NamingEndpoint}/nacos/v3/client/ns/instance
// Nacos 2.x: POST {Endpoint}/nacos/v1/ns/instance
func (c *Cluster) registerNode() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := c.ensureToken(ctx)
	if err != nil {
		// 认证失败时尝试无 token 注册（某些 Nacos 部署关闭了认证）
		tlog.Debug("nacos auth failed, trying without token", "error", err)
	}

	ip, port := c.parseAddrPort()
	metadata := map[string]string{
		"nodeID": c.nodeID,
		"zone":   c.zone,
	}
	metadataJSON, _ := json.Marshal(metadata)

	form := url.Values{}
	form.Set("serviceName", config.DefaultClusterServiceName)
	form.Set("ip", ip)
	form.Set("port", port)
	form.Set("weight", "1")
	form.Set("metadata", string(metadataJSON))
	form.Set("clusterName", "DEFAULT")
	form.Set("groupName", c.nacosCfg.Group)
	form.Set("namespaceId", c.nacosCfg.Namespace)
	form.Set("ephemeral", "true")
	form.Set("heartBeat", "false")

	var reqURL string
	if strings.ToLower(c.nacosCfg.APIVersion) == "v1" {
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance", c.nacosCfg.Endpoint)
	} else {
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance", c.namingEndpoint())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nacos register status %d", resp.StatusCode)
	}
	return nil
}

// deregisterNode 从 Nacos 注销本节点实例
func (c *Cluster) deregisterNode() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, _ := c.ensureToken(ctx)
	ip, port := c.parseAddrPort()

	q := url.Values{}
	q.Set("serviceName", config.DefaultClusterServiceName)
	q.Set("ip", ip)
	q.Set("port", port)
	q.Set("groupName", c.nacosCfg.Group)
	q.Set("namespaceId", c.nacosCfg.Namespace)
	q.Set("clusterName", "DEFAULT")

	var reqURL string
	if strings.ToLower(c.nacosCfg.APIVersion) == "v1" {
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance?%s", c.nacosCfg.Endpoint, q.Encode())
	} else {
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance?%s", c.namingEndpoint(), q.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// heartbeatLoop 定期重新注册实例维持心跳
// Nacos HTTP 注册的临时实例有 TTL，需定期 re-register
func (c *Cluster) heartbeatLoop() {
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			if err := c.registerNode(); err != nil {
				tlog.Error("cluster heartbeat register failed", "error", err)
			}
		}
	}
}

// leaderElectionLoop Leader 选举循环
func (c *Cluster) leaderElectionLoop() {
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()

	c.electOnce()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.electOnce()
		}
	}
}

// clusterInstance Nacos 实例 JSON 结构（集群节点）
type clusterInstance struct {
	IP       string            `json:"ip"`
	Port     int               `json:"port"`
	Metadata map[string]string `json:"metadata"`
	Healthy  bool              `json:"healthy"`
	Enabled  bool              `json:"enabled"`
}

// electOnce 执行一次 Leader 选举
// fail-safe 原则：网络错误/拉取失败时主动丢主，宁可无主不可双主
func (c *Cluster) electOnce() {
	instances, err := c.listInstances()
	if err != nil {
		// 网络错误：若是 Leader 主动丢主，避免网络分区期间双主
		if c.isLeader.CompareAndSwap(1, 0) {
			tlog.Warn("leader election failed, releasing leadership (fail-safe)", "error", err)
		}
		return
	}

	// 过滤同 zone 的健康实例（zone 为空视为 default）
	addresses := make([]string, 0, len(instances))
	for _, inst := range instances {
		if !inst.Healthy || !inst.Enabled {
			continue
		}
		instZone := inst.Metadata["zone"]
		if instZone == "" {
			instZone = "default"
		}
		if c.zone == "" {
			c.zone = "default"
		}
		if instZone != c.zone {
			continue
		}
		addresses = append(addresses, fmt.Sprintf("%s:%d", inst.IP, inst.Port))
	}
	if len(addresses) == 0 {
		return
	}

	// 按 ip:port 字典序排序，排名第一者为 Leader
	// 所有节点看到相同列表 → 排序一致 → 收敛到同一 Leader
	sort.Strings(addresses)
	leader := addresses[0]

	if leader == c.advertiseAddr {
		if c.isLeader.CompareAndSwap(0, 1) {
			tlog.Info("acquired cluster leadership", "nodeID", c.nodeID, "address", c.advertiseAddr)
		}
	} else if c.isLeader.CompareAndSwap(1, 0) {
		tlog.Info("lost cluster leadership", "nodeID", c.nodeID, "currentLeader", leader)
	}
}

// listInstances 从 Nacos 拉取集群节点实例列表
// Nacos 3.x: GET {NamingEndpoint}/nacos/v3/client/ns/instance/list
// Nacos 2.x: GET {Endpoint}/nacos/v1/ns/instance/list
func (c *Cluster) listInstances() ([]clusterInstance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, _ := c.ensureToken(ctx)

	q := url.Values{}
	q.Set("serviceName", config.DefaultClusterServiceName)
	q.Set("groupName", c.nacosCfg.Group)
	q.Set("namespaceId", c.nacosCfg.Namespace)

	var reqURL string
	if strings.ToLower(c.nacosCfg.APIVersion) == "v1" {
		reqURL = fmt.Sprintf("%s/nacos/v1/ns/instance/list?%s", c.nacosCfg.Endpoint, q.Encode())
	} else {
		reqURL = fmt.Sprintf("%s/nacos/v3/client/ns/instance/list?%s", c.namingEndpoint(), q.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("nacos list status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 兼容三种响应格式：
	//   - Nacos 3.x 客户端 API: {"code":0,"data":[{instance}, ...]}
	//   - Nacos 3.x 控制台 API: {"code":0,"data":{"pageItems":[...]}}
	//   - Nacos 2.x API:        {"instances":[...]}
	var instances []clusterInstance
	var wrapper struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Data) > 0 {
		if arrErr := json.Unmarshal(wrapper.Data, &instances); arrErr != nil || instances == nil {
			var paged struct {
				Instances []clusterInstance `json:"instances"`
				PageItems []clusterInstance `json:"pageItems"`
			}
			if err := json.Unmarshal(wrapper.Data, &paged); err != nil {
				return nil, fmt.Errorf("unmarshal data: %w", err)
			}
			instances = append(instances, paged.Instances...)
			instances = append(instances, paged.PageItems...)
		}
	} else {
		var data struct {
			Instances []clusterInstance `json:"instances"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("unmarshal v1 response: %w", err)
		}
		instances = data.Instances
	}
	return instances, nil
}

// IsLeader 返回当前节点是否为 Leader
func (c *Cluster) IsLeader() bool {
	return c.isLeader.Load() == 1
}

// GetNodeID 返回节点 ID
func (c *Cluster) GetNodeID() string {
	return c.nodeID
}

// Stop 停止集群管理器（幂等，可重复调用）
func (c *Cluster) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
	if err := c.deregisterNode(); err != nil {
		tlog.Warn("cluster node deregister failed", "error", err)
	}
	c.isLeader.Store(0)
	tlog.Info("cluster stopped", "nodeID", c.nodeID)
}

// namingEndpoint 返回用于实例注册/查询的 Nacos 地址
// 优先使用 NamingEndpoint（主端口），否则回退到 Endpoint
func (c *Cluster) namingEndpoint() string {
	if c.nacosCfg.NamingEndpoint != "" {
		return c.nacosCfg.NamingEndpoint
	}
	return c.nacosCfg.Endpoint
}

// parseAddrPort 从 advertiseAddr（host:port）中解析出 ip 和 port
func (c *Cluster) parseAddrPort() (string, string) {
	idx := strings.LastIndex(c.advertiseAddr, ":")
	if idx < 0 {
		return c.advertiseAddr, "0"
	}
	return c.advertiseAddr[:idx], c.advertiseAddr[idx+1:]
}

// ensureToken 获取 Nacos 3.x 认证 token（复用 config_center 的登录逻辑）
func (c *Cluster) ensureToken(ctx context.Context) (string, error) {
	if c.nacosCfg.Username == "" || c.nacosCfg.Password == "" {
		return "", nil
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authToken != "" && time.Now().Before(c.authExpire.Add(-60*time.Second)) {
		return c.authToken, nil
	}

	loginURL := fmt.Sprintf("%s/v1/auth/users/login", c.nacosCfg.Endpoint)
	form := fmt.Sprintf("username=%s&password=%s", c.nacosCfg.Username, c.nacosCfg.Password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nacos login status %d", resp.StatusCode)
	}
	authHeader := resp.Header.Get("Authorization")
	if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		c.authToken = strings.TrimPrefix(authHeader, "Bearer ")
		c.authExpire = time.Now().Add(18000 * time.Second)
		return c.authToken, nil
	}
	return "", fmt.Errorf("no token in login response")
}

// localIP 获取本机出网 IP（UDP dial 不发实际包，失败回退 127.0.0.1）
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
