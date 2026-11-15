package logiclink

import (
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/dataplane"
)

// GatewayInterface 定义网关提供给后端（逻辑服客户端/gRPC 服务）的能力。
type GatewayInterface interface {
	GetConnectionManager() *dataplane.ConnectionManager
	GetGRPCConfig() config.GRPCConfig
	GetStreamConfig() config.StreamConfig
	GetGatewayID() string
	GetServerID() string
	AddPushedToClient(n int64)
	AddPushDroppedNoConn(n int64)
	GetLogicClient(serverID string) dataplane.LogicClientProvider
	GetShardedCoalescer() *dataplane.ShardedWriteCoalescer
	// KickByAccounts / KickByIP KickService 本地执行（单向 Kick 模型），返回踢中数
	KickByAccounts(accountIDs []string, reason string) int
	KickByIP(ip, reason string) int
}
