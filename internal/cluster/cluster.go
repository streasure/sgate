package cluster

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
)

// Cluster 保存网关身份和本地 Leader 状态，服务注册与发现由 ClusterComponent 负责。
type Cluster struct {
	nodeID     string
	serverID   string
	serverType string
	zone       string
	leader     atomic.Int32
}

func NewCluster(cfg config.ClusterConfig, serverID, serverType, zone string) *Cluster {
	nodeID := cfg.NodeID
	if nodeID == "" {
		host, _ := os.Hostname()
		nodeID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	c := &Cluster{nodeID: nodeID, serverID: serverID, serverType: serverType, zone: zone}
	c.leader.Store(1)
	return c
}

func (c *Cluster) Start()            {}
func (c *Cluster) Stop()             { c.leader.Store(0) }
func (c *Cluster) IsLeader() bool    { return c.leader.Load() == 1 }
func (c *Cluster) GetNodeID() string { return c.nodeID }
