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
	GetShardedCoalescer() *connection.ShardedWriteCoalescer
	// KickByAccounts / KickByIP KickService 本地执行（单向 Kick 模型），返回踢中数
	KickByAccounts(accountIDs []string, reason string) int
	KickByIP(ip, reason string) int
}

var streamDataPool = sync.Pool{
	New: func() any {
		return &protoGw.StreamData{}
	},
}

// GetStreamData 从池中获取 StreamData。
func GetStreamData() *protoGw.StreamData {
	return streamDataPool.Get().(*protoGw.StreamData)
}

// PutStreamData 归还 StreamData 到池中。
func PutStreamData(msg *protoGw.StreamData) {
	if msg == nil {
		return
	}
	// 保留 Data 缓冲容量：GetStreamData 的调用方（消息管道热路径）
	// 通过 append(msg.Data[:0], ...) 复用，消除每消息的字节切片分配。
	// 安全性：grpc-go clientStream.SendMsg 在返回前同步走 prepareMsg →
	// codec.Marshal（proto.MarshalAppend 拷贝进独立缓冲），dataFrame 引用的
	// 是已拷贝的 payload，不再读源消息；故 SendMsg 返回后归还即安全。
	kept := msg.Data[:0]
	msg.Reset()
	msg.Data = kept
	streamDataPool.Put(msg)
}
