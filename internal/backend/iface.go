package backend

import (
	"sync"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
)

// GatewayInterface 定义网关提供给后端（逻辑服客户端/gRPC 服务）的能力。
type GatewayInterface interface {
	GetConnectionManager() *connection.ConnectionManager
	GetGRPCConfig() config.GRPCConfig
	GetStreamConfig() config.StreamConfig
	GetGatewayID() string
	GetServerID() string
	AddPushedToClient(n int64)
	AddPushDroppedNoConn(n int64)
	GetLogicClient(serverID string) connection.LogicClientProvider
	LookupLogicAddress(serverID string) string
	GetGatewayClient(serverID string) GatewayClientProvider
	GetShardedCoalescer() *connection.ShardedWriteCoalescer
}

// GatewayClientProvider 定义网关客户端的连接状态能力。
type GatewayClientProvider interface {
	IsConnected() bool
}

var streamDataPool = sync.Pool{
	New: func() interface{} {
		return &protoGw.StreamData{}
	},
}

// GetStreamData 从池中获取 StreamData。
func GetStreamData() *protoGw.StreamData {
	return streamDataPool.Get().(*protoGw.StreamData)
}

// PutStreamData 归还 StreamData 到池中。
func PutStreamData(msg *protoGw.StreamData) {
	msg.Reset()
	streamDataPool.Put(msg)
}
