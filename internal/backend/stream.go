package backend

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/connection"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

type StreamShard struct {
	stream      protoGw.GatewayStream_OnDataClient // gRPC 流客户端
	mu          sync.Mutex                         // 保护 stream 引用的互斥锁
	sendCh      chan *protoGw.StreamData           // 发送通道
	stopCh      chan struct{}                      // 停止信号通道
	stopOnce    sync.Once                          // 确保只关闭一次 stopCh
	ctx         context.Context                    // 流上下文
	cancel      context.CancelFunc                 // 取消函数
	index       int                                // 分片索引
	lc          *LogicClient                       // 所属的逻辑服客户端
	closed      atomic.Bool                        // 是否已关闭
	sendTimeout time.Duration                      // 发送超时
}

// StreamManager 流连接管理器，通过分片减少并发竞争
type StreamManager struct {
	shards      []*StreamShard // 分片数组
	sendTimeout time.Duration  // 发送超时
}

// NewStreamManager 创建流管理器，根据 CPU 核心数和配置初始化分片
func NewStreamManager(shardCount int, sendChannelSize int, sendTimeout time.Duration) *StreamManager {
	if shardCount <= 0 {
		shardCount = runtime.NumCPU() * 4
	}
	if sendChannelSize <= 0 {
		sendChannelSize = 65536
	}
	if sendTimeout <= 0 {
		sendTimeout = 200 * time.Millisecond
	}
	sm := &StreamManager{
		shards:      make([]*StreamShard, shardCount),
		sendTimeout: sendTimeout,
	}
	for i := range sm.shards {
		sm.shards[i] = &StreamShard{
			sendCh:      make(chan *protoGw.StreamData, sendChannelSize),
			stopCh:      make(chan struct{}),
			index:       i,
			sendTimeout: sendTimeout,
		}
	}
	return sm
}

// GetShard 根据连接 ID 的哈希值获取对应的分片
func (sm *StreamManager) GetShard(connectionID string) *StreamShard {
	h := fnv.New32a()
	h.Write([]byte(connectionID))
	return sm.shards[h.Sum32()%uint32(len(sm.shards))]
}

// markShardBroken 分片流失效后的统一处理：触发整体重连。
// 没有这一步，逻辑服重启或网络闪断后 shard.stream 永远为 nil，
// 正向消息会静默丢弃、反向推送归零，健康检查的 ping 也只会
// 塞进已失效的 sendCh，永远无法探测出故障。
func (s *StreamShard) markShardBroken() {
	s.mu.Lock()
	s.stream = nil
	s.mu.Unlock()
	if s.lc != nil {
		go s.lc.handleDisconnection()
	}
}

// startSendLoop 启动发送循环，批量消费 sendCh 中的消息并发送到流
func (s *StreamShard) startSendLoop() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "startSendLoop panic recovered shard=%d error=%v", s.index, fmt.Sprintf("%v", r))
		}
	}()

	const maxBatchCount = 256
	batch := make([]*protoGw.StreamData, 0, maxBatchCount)

	for {
		var msg *protoGw.StreamData
		select {
		case <-s.stopCh:
			return
		case msg = <-s.sendCh:
			if msg == nil {
				continue
			}
		}

		batch = batch[:0]
		batch = append(batch, msg)
		drained := true
		for drained {
			select {
			case m := <-s.sendCh:
				if m == nil {
					drained = false
					break
				}
				batch = append(batch, m)
				if len(batch) >= maxBatchCount {
					drained = false
				}
			default:
				drained = false
			}
		}

		// 获取整批消息共用的流引用。
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()

		if stream == nil {
			// 流不可用时，将所有消息归还到对象池。
			for _, m := range batch {
				PutStreamData(m)
			}
			continue
		}

		// 使用单个流引用发送整批消息。
		sendIdx := 0
		for sendIdx < len(batch) {
			if err := stream.Send(batch[sendIdx]); err != nil {
				tlog.Warn(context.TODO(), "shard send error, isolating shard shard=%d error=%v", s.index, err)
				s.markShardBroken()
				break
			}
			PutStreamData(batch[sendIdx])
			sendIdx++
		}
		// 将未发送的消息归还到对象池。
		for i := sendIdx; i < len(batch); i++ {
			PutStreamData(batch[i])
		}
	}
}

// SendMessage 向分片发送消息，支持快速路径和超时机制
func (s *StreamShard) SendMessage(msg *protoGw.StreamData) (err error) {
	// 快速路径：原子检查关闭标志，避免 defer/recover 开销。
	if s.closed.Load() {
		return ErrNotConnected
	}

	// 先尝试非阻塞发送（最常见情况）。
	select {
	case s.sendCh <- msg:
		return nil
	default:
		// 通道已满，尝试带超时的发送。
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("send on closed channel: %v", r)
			}
		}()
		timer := time.NewTimer(s.sendTimeout)
		defer timer.Stop()
		select {
		case s.sendCh <- msg:
			err = nil
		case <-s.stopCh:
			err = ErrNotConnected
		case <-timer.C:
			err = ErrSendTimeout
		}
	}()
	return
}

// stop 停止分片的发送和接收
func (s *StreamShard) stop() {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		close(s.stopCh)
	})
}

// connGroup 代表一条独立的 gRPC 连接及其 gRPC 客户端
func (s *StreamShard) receiveMessages(lc *LogicClient, shardIdx int) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(context.TODO(), "receiveMessages panic recovered error=%v", fmt.Sprintf("%v", r))
		}
	}()

	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()

	if stream == nil {
		if !lc.closing {
			s.markShardBroken()
		}
		return
	}

	batchPush := false
	if lc.gateway != nil {
		batchPush = lc.gateway.GetStreamConfig().BatchPush
	}

	// batchPush 模式：收集消息并以 PushBatch 形式刷新。
	// 批量推送模式：收集消息并以 PushBatch 形式批量刷新
	const batchFlushSize = 128
	batch := make([]*protoGw.StreamData, 0, batchFlushSize)

	flushBatch := func() {
		if len(batch) == 0 || lc.gateway == nil {
			batch = batch[:0]
			return
		}
		type connBatch struct {
			conn  *connection.Connection
			items []*protoGw.PushItem
		}
		connMap := make(map[string]*connBatch)
		for _, m := range batch {
			if m.SessionId == "" {
				continue
			}
			cb, ok := connMap[m.SessionId]
			if !ok {
				conn := lc.gateway.GetConnectionManager().GetConnection(m.SessionId)
				if conn == nil {
					lc.gateway.AddPushDroppedNoConn(1)
					continue
				}
				cb = &connBatch{conn: conn}
				connMap[m.SessionId] = cb
			}
			cb.items = append(cb.items, &protoGw.PushItem{
				SessionId: m.SessionId,
				Cmd:       m.Cmd,
				Data:      m.Data,
				SeqId:     m.SeqId,
			})
		}
		for _, cb := range connMap {
			batchMsg := &protoGw.PushBatch{Items: cb.items}
			batchData, err := proto.Marshal(batchMsg)
			if err != nil {
				lc.gateway.AddPushDroppedNoConn(int64(len(cb.items)))
				continue
			}
			responseData, err := routes.MarshalClientMessage(&protoGw.StreamData{
				Cmd:  int32(routes.CmdPushBatch),
				Data: batchData,
			})
			if err != nil {
				lc.gateway.AddPushDroppedNoConn(int64(len(cb.items)))
				continue
			}
			if lc.gateway.GetShardedCoalescer() != nil {
				lc.gateway.GetShardedCoalescer().AddMulti(cb.items[0].SessionId, responseData, cb.conn)
				lc.gateway.AddPushedToClient(int64(len(cb.items)))
			} else if sendErr := cb.conn.Send(responseData); sendErr != nil {
				tlog.Warn(context.TODO(), "batch push to client failed sessionID=%s error=%v", cb.items[0].SessionId, sendErr)
			} else {
				lc.gateway.AddPushedToClient(int64(len(cb.items)))
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		msg, err := stream.Recv()
		if err != nil {
			lc.mu.RLock()
			closing := lc.closing
			lc.mu.RUnlock()

			if batchPush {
				flushBatch()
			}

			if closing {
				return
			}

			tlog.Warn(context.TODO(), "shard receive error, triggering reconnect shard=%d error=%v", shardIdx, err)
			s.markShardBroken()
			return
		}

		if lc.gateway == nil {
			continue
		}

		// Broadcast：空 SessionId 表示发送到此网关上的所有连接。
		// 广播：空 SessionId 表示发送到此网关上的所有连接
		if msg.SessionId == "" {
			if batchPush {
				flushBatch()
			}
			lc.gateway.GetConnectionManager().ForEach(func(conn *connection.Connection) bool {
				respData, err := routes.MarshalClientMessage(msg)
				if err != nil {
					return true
				}
				if lc.gateway.GetShardedCoalescer() != nil {
					lc.gateway.GetShardedCoalescer().AddMulti(conn.ID(), respData, conn)
					lc.gateway.AddPushedToClient(1)
				} else if sendErr := conn.Send(respData); sendErr == nil {
					lc.gateway.AddPushedToClient(1)
				}
				return true
			})
			continue
		}

		if batchPush {
			if msg.UserKey != "" {
				lc.gateway.GetConnectionManager().UpdateConnectionUserUUID(msg.SessionId, msg.UserKey)
			}
			batch = append(batch, msg)
			if len(batch) >= batchFlushSize {
				flushBatch()
			}
		} else {
			conn := lc.gateway.GetConnectionManager().GetConnection(msg.SessionId)
			if conn != nil {
				if msg.UserKey != "" {
					lc.gateway.GetConnectionManager().UpdateConnectionUserUUID(msg.SessionId, msg.UserKey)
				}
				responseData, err := routes.MarshalClientMessage(msg)
				if err == nil {
					if lc.gateway.GetShardedCoalescer() != nil {
						lc.gateway.GetShardedCoalescer().AddMulti(msg.SessionId, responseData, conn)
						lc.gateway.AddPushedToClient(1)
					} else if sendErr := conn.Send(responseData); sendErr != nil {
						tlog.Warn(context.TODO(), "push to client failed sessionID=%s cmd=%d error=%v", msg.SessionId, msg.Cmd, sendErr)
					} else {
						lc.gateway.AddPushedToClient(1)
					}
				}
			} else {
				lc.gateway.AddPushDroppedNoConn(1)
			}
		}
	}
}

// handleDisconnection 处理断线事件，触发重连
