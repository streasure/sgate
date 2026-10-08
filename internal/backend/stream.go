package backend

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/util/hashutil"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// dataStream 数据流抽象：兼容旧模式（sgate 拨出的客户端流）与
// flip 模式（logic 主动拨入、sgate 侧的服务端流），二者均有 Send/Recv。
type dataStream interface {
	Send(*protoGw.StreamData) error
	Recv() (*protoGw.StreamData, error)
}

type StreamShard struct {
	stream      dataStream               // gRPC 流（接入后由握手填充）
	mu          sync.Mutex               // 保护 stream 引用的互斥锁
	sendCh      chan *protoGw.StreamData // 发送通道
	stopCh      chan struct{}            // 停止信号通道
	stopOnce    sync.Once                // 确保只关闭一次 stopCh
	index       int                      // 分片索引
	lc          *LogicClient             // 所属的逻辑服客户端
	closed      atomic.Bool              // 是否已关闭
	sendTimeout time.Duration            // 发送超时
	// batchUpstream 为 true 时多条 StreamData 合并为单个 StreamBatch gRPC 帧发送
	// （两端必须同时开启 stream.batchUpstream，见 config.StreamConfig）。
	batchUpstream bool
}

// StreamManager 流连接管理器，通过分片减少并发竞争
type StreamManager struct {
	shards      []*StreamShard // 分片数组
	sendTimeout time.Duration  // 发送超时
}

// NewStreamManager 创建流管理器，根据 CPU 核心数和配置初始化分片
func NewStreamManager(shardCount int, sendChannelSize int, sendTimeout time.Duration, batchUpstream bool) *StreamManager {
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
			sendCh:        make(chan *protoGw.StreamData, sendChannelSize),
			stopCh:        make(chan struct{}),
			index:         i,
			sendTimeout:   sendTimeout,
			batchUpstream: batchUpstream,
		}
	}
	return sm
}

// GetShard 根据连接 ID 的哈希值获取对应的分片
func (sm *StreamManager) GetShard(connectionID string) *StreamShard {
	return sm.shards[hashutil.FNV1a32(connectionID)%uint32(len(sm.shards))]
}

// detach 从发送失败侧解除本分片与指定流的绑定（幂等）。
// 重连/重接入由 logic 拨入方负责：sgate 只维护状态与计数。
func (s *StreamShard) detach(stream dataStream) {
	if s.lc != nil {
		s.lc.detachShard(s, stream)
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
			// 流不可用（分片尚未接入或已断开）：状态机已把消息切给
			// 断线队列，这里只可能收到在途残余，归还对象池并统计丢弃。
			if s.lc != nil && s.lc.gateway != nil {
				s.lc.gateway.AddPushDroppedNoConn(int64(len(batch)))
			}
			for _, m := range batch {
				PutStreamData(m)
			}
			continue
		}

		// 使用单个流引用发送整批消息。
		// StreamBatch 合帧：开启合帧时整批（含单条）压成单个 gRPC 帧，
		// 接收端固定按 StreamBatch 解码，绝不能在同一股流上混发裸 StreamData。
		sendIdx := 0
		if s.batchUpstream {
			if ms, ok := stream.(interface{ SendMsg(m any) error }); ok {
				if err := ms.SendMsg(&protoGw.StreamBatch{Items: batch}); err != nil {
					tlog.Warn(context.TODO(), "shard send error, isolating shard shard=%d error=%v", s.index, err)
					s.detach(stream)
				} else {
					for _, m := range batch {
						PutStreamData(m)
					}
					sendIdx = len(batch)
				}
			}
		}
		for sendIdx < len(batch) {
			if err := stream.Send(batch[sendIdx]); err != nil {
				tlog.Warn(context.TODO(), "shard send error, isolating shard shard=%d error=%v", s.index, err)
				s.detach(stream)
				break
			}
			PutStreamData(batch[sendIdx])
			sendIdx++
		}
		// 将未发送的消息归还到对象池并计数。
		if sendIdx < len(batch) && s.lc != nil && s.lc.gateway != nil {
			s.lc.gateway.AddPushDroppedNoConn(int64(len(batch) - sendIdx))
		}
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
		return
	}

	batchPush := false
	batchUpstream := false
	if lc.gateway != nil {
		streamCfg := lc.gateway.GetStreamConfig()
		batchPush = streamCfg.BatchPush
		batchUpstream = streamCfg.BatchUpstream
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

	// processOne 分发单条上游消息（心跳/广播/按会话推送）。
	// 收到与停止/错误路径由调用方处理（含 batchPush 冲刷）。
	processOne := func(msg *protoGw.StreamData) {
		if lc.gateway == nil {
			return
		}

		// 内置心跳：flip 模式下 logic 主动拨入后由本侧消费，无需应答
		// （与 logic 的 builtinCommand 对称），绝不能落入空 SessionId 广播分支。
		if msg.Cmd == int32(routes.CmdHeartbeatReq) {
			return
		}

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
			return
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

	// StreamBatch 合帧接收：两端开启 stream.batchUpstream 时对端按批发送。
	var batchRecv interface{ RecvMsg(m any) error }
	if batchUpstream {
		if mr, ok := stream.(interface{ RecvMsg(m any) error }); ok {
			batchRecv = mr
		}
	}

	handleRecvErr := func(err error) {
		lc.mu.RLock()
		closing := lc.closing
		lc.mu.RUnlock()

		if batchPush {
			flushBatch()
		}

		if closing {
			return
		}

		// 分片解绑由接收循环外层（OnData handler 的 defer）统一执行。
		tlog.Warn(context.TODO(), "shard receive error shard=%d error=%v", shardIdx, err)
	}

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		if batchRecv != nil {
			sb := &protoGw.StreamBatch{}
			if err := batchRecv.RecvMsg(sb); err != nil {
				handleRecvErr(err)
				return
			}
			for _, m := range sb.Items {
				if m == nil {
					continue
				}
				processOne(m)
			}
			continue
		}

		msg, err := stream.Recv()
		if err != nil {
			handleRecvErr(err)
			return
		}
		processOne(msg)
	}
}
