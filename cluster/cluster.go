package cluster

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/util/nacos"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// Cluster 集群管理器（基于 util/nacos）
// 功能：
//   - 网关节点注册为 Nacos 临时实例（委托 util/nacos.Registry 心跳保活）
//   - Leader 选举：同 zone 内按实例地址（ip:port）字典序排序，排名第一者为 Leader
//   - 节点下线/故障后临时实例过期消失，剩余节点自动接管（双机热备/自动容灾）
//
// 选举正确性：所有节点看到相同的实例列表（Nacos 最终一致），
// 排序结果一致 → 收敛到同一个 Leader，无需分布式锁竞争。
type Cluster struct {
	registry  *nacos.Registry
	discovery *nacos.Discovery
	cfg       config.ClusterConfig
	zone      string
	nodeID    string
	advertiseAddr string
	isLeader  atomic.Int32
	stopChan  chan struct{}
	stopOnce  sync.Once
	ttl       time.Duration
	renewInterval time.Duration
}

// NewCluster 创建集群管理器
// advertisePort 为本节点对外暴露的端口（用于生成 ip:port 实例地址，同机多实例靠端口区分）
func NewCluster(cfg config.ClusterConfig, nacosCfg nacos.Config, zone string, advertisePort int) *Cluster {
	nodeID := cfg.NodeID
	if nodeID == "" {
		hostname, _ := os.Hostname()
		nodeID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	ttl := 10 * time.Second
	if d, err := time.ParseDuration(cfg.LockTTL); err == nil && d > 0 {
		ttl = d
	}

	addr := fmt.Sprintf("%s:%d", localIP(), advertisePort)

	// 创建 Registry：负责注册 + 心跳续期
	registry := nacos.NewRegistry(nacos.RegistryConfig{
		Enabled: true,
		Nacos:   nacosCfg,
		Service: nacos.NamingConfig{
			ServiceName: config.DefaultClusterServiceName,
			Addr:        addr,
			Weight:      1,
			Zone:        zone,
			Metadata: map[string]string{
				"nodeID": nodeID,
				"zone":   zone,
			},
		},
		HeartbeatInterval: ttl / 3,
		HeartbeatTTL:      ttl,
	})

	// 创建 Discovery：负责拉取实例列表（用于 Leader 选举）
	discovery := nacos.NewDiscovery(nacos.DiscoveryConfig{
		Enabled: true,
		Nacos:   nacosCfg,
		Service: nacos.NamingConfig{
			ServiceName: config.DefaultClusterServiceName,
		},
		Zone: zone,
	})

	return &Cluster{
		registry:      registry,
		discovery:     discovery,
		cfg:           cfg,
		zone:          zone,
		nodeID:        nodeID,
		advertiseAddr: addr,
		stopChan:      make(chan struct{}),
		ttl:           ttl,
		renewInterval: ttl / 3,
	}
}

// Start 启动集群：注册节点 + Leader 选举
func (c *Cluster) Start(ctx context.Context) {
	// 启动 Registry（注册 + 心跳续期）
	if err := c.registry.Start(); err != nil {
		tlog.Error("cluster node register failed", "error", err)
	}

	// 启动 Discovery（拉取实例列表）
	if err := c.discovery.Start(); err != nil {
		tlog.Error("cluster discovery start failed", "error", err)
	}

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

// leaderElectionLoop Leader 选举循环
func (c *Cluster) leaderElectionLoop() {
	c.electOnce()
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.electOnce()
		}
	}
}

// electOnce 执行一次 Leader 选举
// fail-safe 原则：网络错误/拉取失败时主动丢主，宁可无主不可双主
func (c *Cluster) electOnce() {
	services := c.discovery.GetServices()
	if len(services) == 0 {
		return
	}

	// 过滤同 zone 的健康实例
	addresses := make([]string, 0, len(services))
	for _, svc := range services {
		instZone := svc.Metadata["zone"]
		if instZone == "" {
			instZone = "default"
		}
		if c.zone == "" {
			c.zone = "default"
		}
		if instZone != c.zone {
			continue
		}
		addresses = append(addresses, svc.Address)
	}
	if len(addresses) == 0 {
		return
	}

	// 按 ip:port 字典序排序，排名第一者为 Leader
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
		if c.discovery != nil {
			c.discovery.Destroy()
		}
		if c.registry != nil {
			c.registry.Destroy()
		}
		c.isLeader.Store(0)
		tlog.Info("cluster stopped", "nodeID", c.nodeID)
	})
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
