package gateway

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// Cluster 集群管理器
// 功能：
//   - 基于 Redis 的 Leader 选举（双机热备/自动容灾）
//   - 节点注册与健康心跳
//   - 节点下线时流量自动切换（通过服务发现）
type Cluster struct {
	redisClient   *redis.Client
	cfg           config.ClusterConfig
	zone          string
	nodeID        string
	isLeader      atomic.Int32
	stopChan      chan struct{}
	stopOnce      sync.Once
	ttl           time.Duration
	renewInterval time.Duration
}

// NewCluster 创建集群管理器
func NewCluster(redisClient *redis.Client, cfg config.ClusterConfig, zone string) *Cluster {
	nodeID := cfg.NodeID
	if nodeID == "" {
		hostname, _ := os.Hostname()
		nodeID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	ttl := 10 * time.Second
	if d, err := time.ParseDuration(cfg.LockTTL); err == nil && d > 0 {
		ttl = d
	}

	return &Cluster{
		redisClient:   redisClient,
		cfg:           cfg,
		zone:          zone,
		nodeID:        nodeID,
		stopChan:      make(chan struct{}),
		ttl:           ttl,
		renewInterval: ttl / 3,
	}
}

// Start 启动集群：注册节点 + Leader 选举
func (c *Cluster) Start(ctx context.Context) {
	// 注册本节点到 Redis
	c.registerNode(ctx)

	// 启动心跳续期
	go c.heartbeatLoop()

	// 启动 Leader 选举（如果配置开启）
	if c.cfg.LeaderElection {
		go c.leaderElectionLoop()
	}

	tlog.Info("cluster started",
		"nodeID", c.nodeID,
		"zone", c.zone,
		"leaderElection", c.cfg.LeaderElection)
}

// registerNode 注册本节点到 Redis
func (c *Cluster) registerNode(ctx context.Context) {
	key := fmt.Sprintf("sgate:cluster:nodes:%s", c.nodeID)
	fields := map[string]interface{}{
		"nodeID":  c.nodeID,
		"zone":    c.zone,
		"addr":    c.getLocalAddr(),
		"started": time.Now().Unix(),
	}
	c.redisClient.HSet(ctx, key, fields)
	c.redisClient.Expire(ctx, key, c.ttl*2)

	// 加入 zone 节点集合
	setKey := fmt.Sprintf("sgate:cluster:zone:%s", c.zone)
	c.redisClient.SAdd(ctx, setKey, c.nodeID)
	c.redisClient.Expire(ctx, setKey, c.ttl*2)
}

// heartbeatLoop 心跳续期
func (c *Cluster) heartbeatLoop() {
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			key := fmt.Sprintf("sgate:cluster:nodes:%s", c.nodeID)
			c.redisClient.Expire(ctx, key, c.ttl*2)
			setKey := fmt.Sprintf("sgate:cluster:zone:%s", c.zone)
			c.redisClient.Expire(ctx, setKey, c.ttl*2)
			cancel()
		}
	}
}

// leaderElectionLoop Leader 选举循环
// 使用 Redis SET NX 实现：第一个获取锁的节点成为 Leader
func (c *Cluster) leaderElectionLoop() {
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()

	c.tryAcquireLeadership()

	for {
		select {
		case <-c.stopChan:
			c.releaseLeadership()
			return
		case <-ticker.C:
			c.tryAcquireLeadership()
		}
	}
}

// tryAcquireLeadership 尝试获取 Leader 身份
// fail-safe 原则：网络错误/续期失败时主动丢主，宁可无主不可双主
func (c *Cluster) tryAcquireLeadership() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ok, err := c.redisClient.SetNX(ctx, c.cfg.LockKey, c.nodeID, c.ttl).Result()
	if err != nil {
		// 网络错误：若是 Leader 主动丢主，避免双主风险
		if c.isLeader.CompareAndSwap(1, 0) {
			tlog.Warn("leader lock renew failed, releasing leadership (fail-safe)", "error", err)
		}
		return
	}

	if ok {
		if c.isLeader.CompareAndSwap(0, 1) {
			tlog.Info("acquired cluster leadership", "nodeID", c.nodeID)
		}
	} else {
		// SetNX 失败：锁已被其他节点持有
		if c.isLeader.Load() == 1 {
			val, err := c.redisClient.Get(ctx, c.cfg.LockKey).Result()
			if err == nil && val == c.nodeID {
				// 仍是自己的锁，续期
				if err := c.redisClient.Expire(ctx, c.cfg.LockKey, c.ttl).Err(); err != nil {
					// 续期失败：丢主，避免双主
					c.isLeader.Store(0)
					tlog.Warn("leader lock Expire failed, releasing leadership (fail-safe)", "error", err)
				}
			} else {
				// 锁已被其他节点抢占
				c.isLeader.Store(0)
				tlog.Info("lost cluster leadership", "nodeID", c.nodeID, "currentHolder", val)
			}
		}
	}
}

// releaseLeadership 释放 Leader 身份（优雅下线）
func (c *Cluster) releaseLeadership() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if c.isLeader.Load() == 1 {
		// 使用 Lua 脚本确保只删除自己持有的锁
		luaScript := `if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`
		c.redisClient.Eval(ctx, luaScript, []string{c.cfg.LockKey}, c.nodeID)
		c.isLeader.Store(0)
		tlog.Info("released cluster leadership", "nodeID", c.nodeID)
	}

	// 注销节点
	key := fmt.Sprintf("sgate:cluster:nodes:%s", c.nodeID)
	c.redisClient.Del(ctx, key)
	setKey := fmt.Sprintf("sgate:cluster:zone:%s", c.zone)
	c.redisClient.SRem(ctx, setKey, c.nodeID)
}

// IsLeader 返回当前节点是否为 Leader
func (c *Cluster) IsLeader() bool {
	return c.isLeader.Load() == 1
}

// GetNodeID 返回节点 ID
func (c *Cluster) GetNodeID() string {
	return c.nodeID
}

// getLocalAddr 获取本机地址（简化实现）
func (c *Cluster) getLocalAddr() string {
	return fmt.Sprintf("node:%s", c.nodeID)
}

// Stop 停止集群管理器（幂等，可重复调用）
func (c *Cluster) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
	c.releaseLeadership()
}
