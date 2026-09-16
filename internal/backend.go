package internal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/gateway"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// LogicConnectionState 逻辑服连接状态
type LogicConnectionState int32

const (
	LogicStateDisconnected LogicConnectionState = iota // 未连接
	LogicStateConnecting                               // 连接中
	LogicStateConnected                                // 已连接
	LogicStateReconnecting                             // 重连中
)

func (s LogicConnectionState) String() string {
	switch s {
	case LogicStateDisconnected:
		return "Disconnected"
	case LogicStateConnecting:
		return "Connecting"
	case LogicStateConnected:
		return "Connected"
	case LogicStateReconnecting:
		return "Reconnecting"
	default:
		return "Unknown"
	}
}

// 错误定义
var (
	ErrNotConnected      = errors.New("未连接到逻辑服")
	ErrConnectionClosing = errors.New("连接正在关闭")
	ErrQueueFull         = errors.New("发送队列已满")
	ErrSendTimeout       = errors.New("发送超时")
	ErrBackpressure      = errors.New("背压已激活")
)

// ReconnectConfig 重连配置，控制指数退避策略
type ReconnectConfig struct {
	InitialInterval time.Duration // 初始重连间隔
	MaxInterval     time.Duration // 最大重连间隔
	MaxAttempts     int           // 最大重连尝试次数（0表示无限）
	Multiplier      float64       // 退避倍数
}

// DefaultReconnectConfig 默认重连配置
var DefaultReconnectConfig = ReconnectConfig{
	InitialInterval: 1 * time.Second,
	MaxInterval:     30 * time.Second,
	MaxAttempts:     0,
	Multiplier:      2.0,
}

// HealthCheckConfig 健康检查配置
type HealthCheckConfig struct {
	Interval    time.Duration // 检查间隔
	Timeout     time.Duration // 超时时间
	MaxFailures int           // 最大失败次数，超过则触发重连
	// Enabled 控制是否对逻辑服做主动健康检查（ping）。
	// 默认 true：主动 ping 并在连续失败超阈值后重连，保障容灾切换。
	Enabled bool
}

// DefaultHealthCheckConfig 默认健康检查配置
var DefaultHealthCheckConfig = HealthCheckConfig{
	Interval:    5 * time.Second,
	Timeout:     3 * time.Second,
	MaxFailures: 3,
	Enabled:     true,
}

// StreamShard 单个流分片，封装一个 gRPC 流连接及其发送通道
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

// writeCoalescer 按连接合并后一次性 flush。
// 目的：减少 gnet AsyncWrite 调用次数。每次 AsyncWrite 向 event-loop channel 发送一个 task，
// 在 Windows 上向通道发送数据会竞争 runtime 互斥锁（runtime.lock2），94 个消息接收
// 协程同时发送时锁竞争达到 74% CPU。通过跨批次合并，将 N 次 SendMulti
// 降为 M 次（M 为不同连接数），减少通道发送约 10-50 倍。
//
// 内存优化：每个 entry 的 data buffer 从 coalescerBufPool 获取，在 AsyncWrite 完成后
// 通过回调归还到对象池，避免每帧分配导致 GC 压力（千万级 QPS 下 GC 无法跟上分配速度）。
type writeCoalescer struct {
	entries   []coalescedEntry // 每个连接一个 entry，存储累积的帧数据
	index     map[string]int   // connID -> entries 下标，避免重复 GetConnection
	count     int              // 累积消息总数（用于触发 flush）
	cm        *ConnectionManager
	lastFlush time.Time
}

// coalescedEntry 累积一个连接的帧数据。
// bufPtr 持有指向池化缓冲区的指针，在刷新后通过 AsyncWrite 回调归还。
type coalescedEntry struct {
	conn   *Connection
	data   []byte  // [4字节 len][payload] 重复格式，底层数组来自 coalescerBufPool
	bufPtr *[]byte // 指向 coalescerBufPool 中获取的 buffer，用于归还
}

// coalescerBufPool 复用 coalescer 的 data buffer，避免每帧 append 分配导致 GC 风暴。
// buffer 初始容量 4KB，可动态扩展。归还时保留扩展后的容量（上限 1MB）以复用。
var coalescerBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 4096)
		return &b
	},
}

const (
	coalesceFlushCount    = 50000                // 累积 5 万条消息后 flush，减少 event-loop 入队次数
	coalesceFlushInterval = 5 * time.Millisecond // 5ms 超时 flush，限制推送延迟
	coalescerMaxBufCap    = 1 << 20              // 1MB：归还到池的 buffer 容量上限，避免持有过大 buffer
)

// newWriteCoalescer 创建写合并器
func newWriteCoalescer(cm *ConnectionManager) *writeCoalescer {
	return &writeCoalescer{
		entries:   make([]coalescedEntry, 0, 64),
		index:     make(map[string]int, 64),
		cm:        cm,
		lastFlush: time.Now(),
	}
}

// getBuf 获取或复用一个 entry 的 data buffer
func (wc *writeCoalescer) getBuf(idx int) {
	if wc.entries[idx].bufPtr == nil {
		bufPtr := coalescerBufPool.Get().(*[]byte)
		wc.entries[idx].bufPtr = bufPtr
		wc.entries[idx].data = (*bufPtr)[:0]
	}
}

// addMulti 将 multi-conn 格式的一条消息加入 coalescer。
// payload 是已序列化的单条消息字节数据。
func (wc *writeCoalescer) addMulti(connID string, payload []byte) bool {
	idx, ok := wc.index[connID]
	if !ok {
		conn := wc.cm.GetConnection(connID)
		if conn == nil {
			return false
		}
		idx = len(wc.entries)
		wc.entries = append(wc.entries, coalescedEntry{conn: conn})
		wc.index[connID] = idx
	}
	wc.getBuf(idx)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	wc.entries[idx].data = append(wc.entries[idx].data, lenBuf[:]...)
	wc.entries[idx].data = append(wc.entries[idx].data, payload...)
	wc.count++
	return true
}

// addSingle 将单连接格式的整批数据加入写合并器。
// data 已是 [4字节 len][payload] 重复格式，直接追加。
func (wc *writeCoalescer) addSingle(connID string, data []byte, count int) bool {
	idx, ok := wc.index[connID]
	if !ok {
		conn := wc.cm.GetConnection(connID)
		if conn == nil {
			return false
		}
		idx = len(wc.entries)
		wc.entries = append(wc.entries, coalescedEntry{conn: conn})
		wc.index[connID] = idx
	}
	wc.getBuf(idx)
	wc.entries[idx].data = append(wc.entries[idx].data, data...)
	wc.count += count
	return true
}

// shouldFlush 判断是否应该触发刷新
func (wc *writeCoalescer) shouldFlush() bool {
	return wc.count >= coalesceFlushCount || time.Since(wc.lastFlush) >= coalesceFlushInterval
}

// flush 将所有连接的累积数据通过一次 SendMultiWithCallback 发送，然后重置。
// 缓冲区在 gnet AsyncWrite 完成后通过回调归还到 coalescerBufPool。
// 返回 pushed（成功推送的消息数）。
func (wc *writeCoalescer) flush() int64 {
	var pushed int64
	for i := range wc.entries {
		entry := &wc.entries[i]
		if len(entry.data) > 0 {
			bufPtr := entry.bufPtr
			err := entry.conn.SendMultiWithCallback(entry.data, func() {
				if bufPtr != nil && cap(*bufPtr) <= coalescerMaxBufCap {
					*bufPtr = (*bufPtr)[:0]
					coalescerBufPool.Put(bufPtr)
				}
			})
			if err != nil && bufPtr != nil && cap(*bufPtr) <= coalescerMaxBufCap {
				*bufPtr = (*bufPtr)[:0]
				coalescerBufPool.Put(bufPtr)
			}
		}
		entry.data = nil
		entry.bufPtr = nil
		entry.conn = nil
	}
	pushed = int64(wc.count)
	// 重置：保留 slice/map 底层数组以复用，避免重复分配
	wc.entries = wc.entries[:0]
	for k := range wc.index {
		delete(wc.index, k)
	}
	wc.count = 0
	wc.lastFlush = time.Now()
	return pushed
}

// GetShard 根据连接 ID 的哈希值获取对应的分片
func (sm *StreamManager) GetShard(connectionID string) *StreamShard {
	h := uint32(2166136261)
	for i := 0; i < len(connectionID); i++ {
		h ^= uint32(connectionID[i])
		h *= 16777619
	}
	return sm.shards[h%uint32(len(sm.shards))]
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
			tlog.Error("startSendLoop panic recovered", "shard", s.index, "error", fmt.Sprintf("%v", r))
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
				putStreamData(m)
			}
			continue
		}

		// 使用单个流引用发送整批消息。
		sendIdx := 0
		for sendIdx < len(batch) {
			if err := stream.Send(batch[sendIdx]); err != nil {
				tlog.Warn("shard send error, isolating shard", "shard", s.index, "error", err)
				s.markShardBroken()
				break
			}
			putStreamData(batch[sendIdx])
			sendIdx++
		}
		// 将未发送的消息归还到对象池。
		for i := sendIdx; i < len(batch); i++ {
			putStreamData(batch[i])
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

// LogicClient 逻辑服客户端，管理与单个逻辑服实例的连接和流通信
type LogicClient struct {
	client            protoGw.GatewayStreamClient // gRPC 流客户端
	conn              *grpc.ClientConn            // gRPC 连接
	mu                sync.RWMutex                // 保护状态和连接的读写锁
	state             int32                       // 连接状态（原子操作）
	address           string                      // 逻辑服地址
	streamManager     *StreamManager              // 流分片管理器
	streamCtx         context.Context             // 流上下文
	streamCancel      context.CancelFunc          // 取消流上下文
	reconnectConfig   ReconnectConfig             // 重连配置
	healthCheckConfig HealthCheckConfig           // 健康检查配置
	healthChecker     *HealthChecker              // 健康检查器
	reconnectManager  *ReconnectManager           // 重连管理器
	messageQueue      *StreamMessageQueue         // 断线期间的消息缓存队列
	gateway           GatewayInterface            // 网关接口引用
	closing           bool                        // 是否正在关闭
	closed            chan struct{}               // 关闭完成信号
	shardCount        int                         // 分片数量
	serverID          string                      // 逻辑服标识
}

// NewLogicClient 创建逻辑服客户端实例
func NewLogicClient(gateway GatewayInterface) *LogicClient {
	queuePolicy := config.StreamQueueConfig{}
	var sendTimeout time.Duration
	if gateway != nil {
		queuePolicy = gateway.GetStreamConfig().QueuePolicy
		if d, err := time.ParseDuration(queuePolicy.SendTimeout); err == nil && d > 0 {
			sendTimeout = d
		}
	}
	return &LogicClient{
		state:             int32(LogicStateDisconnected),
		reconnectConfig:   DefaultReconnectConfig,
		healthCheckConfig: DefaultHealthCheckConfig,
		streamManager:     NewStreamManager(0, 0, sendTimeout),
		gateway:           gateway,
		closing:           false,
		closed:            make(chan struct{}),
		shardCount:        runtime.NumCPU() * 8,
		messageQueue:      NewStreamMessageQueue(queuePolicy),
	}
}

func (lc *LogicClient) SetServerID(serverID string) { lc.serverID = serverID }

// getState 原子获取连接状态
func (lc *LogicClient) getState() LogicConnectionState {
	return LogicConnectionState(atomic.LoadInt32(&lc.state))
}

// setState 原子设置连接状态并通知状态变更
func (lc *LogicClient) setState(newState LogicConnectionState) {
	oldState := LogicConnectionState(atomic.LoadInt32(&lc.state))
	if oldState == newState {
		return
	}
	atomic.StoreInt32(&lc.state, int32(newState))
	lc.notifyStateChange(oldState, newState)
}

// notifyStateChange 记录连接状态变更日志
func (lc *LogicClient) notifyStateChange(oldState, newState LogicConnectionState) {
	tlog.Info("logic connection state changed",
		"oldState", oldState.String(),
		"newState", newState.String(),
	)
}

// Connect 连接到逻辑服
func (lc *LogicClient) Connect(address string) error {
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		return ErrConnectionClosing
	}
	lc.address = address
	lc.mu.Unlock()
	return lc.doConnect(false)
}

// doConnect 执行实际的连接/重连操作
func (lc *LogicClient) doConnect(isReconnect bool) error {
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		return ErrConnectionClosing
	}

	var oldState LogicConnectionState
	if isReconnect {
		oldState = LogicConnectionState(atomic.LoadInt32(&lc.state))
		atomic.StoreInt32(&lc.state, int32(LogicStateReconnecting))
	} else {
		oldState = LogicConnectionState(atomic.LoadInt32(&lc.state))
		atomic.StoreInt32(&lc.state, int32(LogicStateConnecting))
	}

	if lc.conn != nil {
		tlog.Info("doConnect closing old connection (reconnect)", "isReconnect", isReconnect)
		lc.conn.Close()
		lc.conn = nil
		lc.client = nil
	}
	lc.mu.Unlock()

	lc.notifyStateChange(oldState, LogicConnectionState(atomic.LoadInt32(&lc.state)))

	tlog.Info("connecting to logic server", "address", lc.address, "reconnect", isReconnect)

	windowSize := int32(524288)
	maxMsgSize := 4 * 1024 * 1024
	if lc.gateway != nil {
		grpcCfg := lc.gateway.GetGRPCConfig()
		windowSize = int32(grpcCfg.WindowSize)
		maxMsgSize = grpcCfg.MaxMessageSize
	}

	conn, err := grpc.Dial(lc.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(windowSize),
		grpc.WithInitialConnWindowSize(windowSize),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgSize),
			grpc.MaxCallSendMsgSize(maxMsgSize),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		tlog.Error("grpc.Dial failed", "error", err, "address", lc.address)
		lc.setState(LogicStateDisconnected)
		return err
	}

	// 在阻塞的 dial 之后重新检查关闭状态，dial 期间可能已调用 Close()。
	lc.mu.RLock()
	if lc.closing {
		lc.mu.RUnlock()
		conn.Close()
		lc.setState(LogicStateDisconnected)
		return ErrConnectionClosing
	}
	lc.mu.RUnlock()

	lc.mu.Lock()
	lc.conn = conn
	lc.client = protoGw.NewGatewayStreamClient(conn)
	lc.mu.Unlock()

	lc.mu.Lock()
	if lc.streamCancel != nil {
		lc.streamCancel()
	}
	lc.streamCtx, lc.streamCancel = context.WithCancel(context.Background())
	lc.mu.Unlock()

	// 关闭旧的流分片：置空流引用并关闭发送通道，使 startSendLoop 协程停止使用旧的（已关闭的）流。
	if lc.streamManager != nil {
		for i := 0; i < len(lc.streamManager.shards); i++ {
			if shard := lc.streamManager.shards[i]; shard != nil {
				shard.closed.Store(true)
				shard.mu.Lock()
				shard.stream = nil
				shard.mu.Unlock()
				shard.stop()
			}
		}
	}

	shardCount := lc.shardCount
	sendChannelSize := 0
	var sendTimeout time.Duration
	if lc.gateway != nil {
		streamCfg := lc.gateway.GetStreamConfig()
		sendChannelSize = streamCfg.SendChannelSize
		if streamCfg.ShardCount > 0 {
			shardCount = streamCfg.ShardCount
		}
		if d, err := time.ParseDuration(streamCfg.QueuePolicy.SendTimeout); err == nil && d > 0 {
			sendTimeout = d
		}
	}
	lc.streamManager = NewStreamManager(shardCount, sendChannelSize, sendTimeout)

	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i := 0; i < shardCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			lc.mu.RLock()
			client := lc.client
			ctx := lc.streamCtx
			lc.mu.RUnlock()

			if client == nil || ctx == nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("client or context is nil") })
				return
			}

			if lc.gateway != nil {
				ctx = metadata.AppendToOutgoingContext(ctx, "sgate-gateway-id", lc.gateway.GetGatewayID())
			}
			stream, err := client.OnData(ctx)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				tlog.Error("failed to establish stream shard", "shard", idx, "error", err)
				return
			}

			shard := lc.streamManager.shards[idx]
			shard.mu.Lock()
			shard.stream = stream
			shard.ctx = ctx
			shard.mu.Unlock()

			tlog.Info("stream shard established", "shard", idx)
			// 流分片建立成功
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		tlog.Error("failed to establish all stream shards", "error", firstErr)
		for i := 0; i < shardCount; i++ {
			shard := lc.streamManager.shards[i]
			shard.mu.Lock()
			if shard.stream != nil {
				shard.stream.CloseSend()
				shard.stream = nil
			}
			shard.mu.Unlock()
		}
		lc.mu.Lock()
		if lc.conn != nil {
			lc.conn.Close()
			lc.conn = nil
			lc.client = nil
		}
		lc.mu.Unlock()
		lc.setState(LogicStateDisconnected)
		return firstErr
	}

	tlog.Info("all stream shards established", "count", shardCount)

	lc.setState(LogicStateConnected)

	for i := 0; i < shardCount; i++ {
		lc.streamManager.shards[i].lc = lc
		go lc.streamManager.shards[i].startSendLoop()
		go lc.streamManager.shards[i].receiveMessages(lc, i)
	}

	// 冲刷断线期间缓存的消息。
	if lc.messageQueue != nil {
		go lc.messageQueue.Flush(lc)
	}

	if lc.reconnectManager == nil {
		lc.reconnectManager = NewReconnectManager(lc, lc.reconnectConfig)
		if lc.gateway != nil {
			lc.reconnectManager.SetLookupAddress(lc.gateway.LookupLogicAddress)
		}
		go lc.reconnectManager.Run()
	}

	lc.startHealthChecker()

	tlog.Info("successfully connected to logic server", "address", lc.address, "shards", shardCount, "isReconnect", isReconnect)

	return nil
}

// Close 关闭逻辑服客户端，释放所有资源
func (lc *LogicClient) Close() {
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		return
	}
	lc.closing = true
	if lc.streamCancel != nil {
		lc.streamCancel()
		lc.streamCtx = nil
		lc.streamCancel = nil
	}

	if lc.streamManager != nil {
		for i := 0; i < len(lc.streamManager.shards); i++ {
			if shard := lc.streamManager.shards[i]; shard != nil {
				shard.closed.Store(true)
				shard.stop()
			}
		}
	}
	lc.mu.Unlock()

	lc.setState(LogicStateDisconnected)

	if lc.healthChecker != nil {
		lc.healthChecker.Stop()
	}
	if lc.reconnectManager != nil {
		lc.reconnectManager.Stop()
	}

	lc.mu.Lock()
	if lc.conn != nil {
		lc.conn.Close()
		lc.conn = nil
		lc.client = nil
	}
	lc.mu.Unlock()

	close(lc.closed)
	tlog.Info("closed logic server connection")
}

// receiveMessages 从流中接收消息并推送到客户端连接
func (s *StreamShard) receiveMessages(lc *LogicClient, shardIdx int) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("receiveMessages panic recovered", "error", fmt.Sprintf("%v", r))
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
			conn  *Connection
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
			responseData, err := marshalClientMessage(&protoGw.StreamData{
				Cmd:  int32(gateway.CmdPushBatch),
				Data: batchData,
			})
			if err != nil {
				lc.gateway.AddPushDroppedNoConn(int64(len(cb.items)))
				continue
			}
			if sendErr := cb.conn.Send(responseData); sendErr != nil {
				tlog.Warn("batch push to client failed", "sessionID", cb.items[0].SessionId, "error", sendErr)
			} else {
				lc.gateway.AddPushedToClient(int64(len(cb.items)))
			}
		}
		batch = batch[:0]
	}

	for {
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

			tlog.Warn("shard receive error, triggering reconnect", "shard", shardIdx, "error", err)
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
			lc.gateway.GetConnectionManager().connections.Range(func(_ string, conn *Connection) bool {
				respData, err := marshalClientMessage(msg)
				if err != nil {
					return true
				}
				if sendErr := conn.Send(respData); sendErr == nil {
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
				responseData, err := marshalClientMessage(msg)
				if err == nil {
					if sendErr := conn.Send(responseData); sendErr != nil {
						tlog.Warn("push to client failed", "sessionID", msg.SessionId, "cmd", msg.Cmd, "error", sendErr)
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
func (lc *LogicClient) handleDisconnection() {
	lc.mu.RLock()
	closing := lc.closing
	state := lc.getState()
	lc.mu.RUnlock()

	if closing {
		return
	}

	if state == LogicStateConnecting || state == LogicStateReconnecting {
		return
	}

	lc.setState(LogicStateDisconnected)

	if lc.reconnectManager != nil {
		lc.reconnectManager.NotifyDisconnection()
	} else {
		tlog.Info("logic server disconnected, attempting reconnect in 5s...")
		time.Sleep(5 * time.Second)
		if lc.closing {
			return
		}
		err := lc.doConnect(true)
		if err != nil {
			tlog.Error("reconnect failed", "error", err)
		} else {
			tlog.Info("reconnect succeeded")
		}
	}
}

// SendMessage 发送消息到逻辑服，支持断线缓存
func (lc *LogicClient) SendMessage(msg *protoGw.StreamData) error {
	// 快速路径：无锁原子检查连接状态。
	// 快速路径：原子检查状态，无需加锁
	if lc.getState() != LogicStateConnected {
		lc.mu.RLock()
		closing := lc.closing
		lc.mu.RUnlock()
		if closing {
			return ErrConnectionClosing
		}
		if lc.messageQueue != nil {
			if err := lc.messageQueue.Enqueue(msg); err != nil {
				return err
			}
		}
		return ErrNotConnected
	}

	shard := lc.streamManager.GetShard(msg.SessionId)
	err := shard.SendMessage(msg)
	if err != nil {
		if lc.messageQueue != nil {
			if qErr := lc.messageQueue.Enqueue(msg); qErr != nil {
				return qErr
			}
		}
		return err
	}

	return nil
}

// SendMessageDirect 直接发送消息，不经过断线缓存队列
func (lc *LogicClient) SendMessageDirect(msg *protoGw.StreamData) error {
	lc.mu.RLock()
	state := lc.getState()
	closing := lc.closing
	lc.mu.RUnlock()

	if closing {
		return ErrConnectionClosing
	}

	if state != LogicStateConnected {
		return ErrNotConnected
	}

	shard := lc.streamManager.GetShard(msg.SessionId)
	return shard.SendMessage(msg)
}

func (lc *LogicClient) IsConnected() bool {
	return lc.getState() == LogicStateConnected
}

func (lc *LogicClient) GetState() LogicConnectionState {
	return lc.getState()
}

// HealthChecker 健康检查器，定期向逻辑服发送心跳探测
type HealthChecker struct {
	lc          *LogicClient   // 逻辑服客户端引用
	interval    time.Duration  // 检查间隔
	timeout     time.Duration  // 超时时间
	maxFailures int            // 最大允许失败次数
	failCount   int            // 当前连续失败次数
	enabled     bool           // 是否启用主动健康检查
	stopCh      chan struct{}  // 停止信号
	wg          sync.WaitGroup // 等待检查循环退出
}

// NewHealthChecker 创建健康检查器实例
func NewHealthChecker(lc *LogicClient, config HealthCheckConfig) *HealthChecker {
	return &HealthChecker{
		lc:          lc,
		interval:    config.Interval,
		timeout:     config.Timeout,
		maxFailures: config.MaxFailures,
		enabled:     config.Enabled,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动健康检查循环
func (hc *HealthChecker) Start() {
	hc.wg.Add(1)
	go hc.checkLoop()
}

// Stop 停止健康检查循环并等待退出
func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	hc.wg.Wait()
}

// checkLoop 健康检查定时循环
func (hc *HealthChecker) checkLoop() {
	defer hc.wg.Done()
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-hc.stopCh:
			return
		case <-ticker.C:
			hc.doCheck()
		}
	}
}

// doCheck 执行一次健康检查，发送心跳包
func (hc *HealthChecker) doCheck() {
	hc.lc.mu.RLock()
	state := hc.lc.getState()
	closing := hc.lc.closing
	hc.lc.mu.RUnlock()

	if closing || state != LogicStateConnected {
		return
	}

	// enabled=false 时跳过主动健康检查（可选关闭）。
	if !hc.enabled {
		return
	}

	pingMsg := &protoGw.StreamData{
		Cmd: int32(gateway.CmdHeartbeatReq),
	}

	err := hc.lc.SendMessageDirect(pingMsg)
	if err != nil {
		hc.failCount++
		tlog.Warn("health check failed", "failCount", hc.failCount, "maxFailures", hc.maxFailures, "error", err)
		if hc.failCount >= hc.maxFailures {
			tlog.Error("too many health check failures, triggering reconnect", "failCount", hc.failCount)
			hc.lc.handleDisconnection()
		}
	} else {
		hc.failCount = 0
	}
}

// startHealthChecker 启动健康检查器
func (lc *LogicClient) startHealthChecker() {
	if lc.healthChecker != nil {
		lc.healthChecker.Stop()
	}
	lc.healthChecker = NewHealthChecker(lc, lc.healthCheckConfig)
	lc.healthChecker.Start()
}

// ReconnectManager 重连管理器，处理断线后的自动重连逻辑
type ReconnectManager struct {
	lc            *LogicClient                 // 逻辑服客户端引用
	config        ReconnectConfig              // 重连配置
	stopCh        chan struct{}                // 停止信号
	doneCh        chan struct{}                // 运行完成信号
	disconnectCh  chan struct{}                // 断线通知通道
	lookupAddress func(serverID string) string // 可选：通过 etcd 查询替换地址
}

// NewReconnectManager 创建重连管理器
func NewReconnectManager(lc *LogicClient, config ReconnectConfig) *ReconnectManager {
	return &ReconnectManager{
		lc:           lc,
		config:       config,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		disconnectCh: make(chan struct{}, 1),
	}
}

// SetLookupAddress 设置地址查询函数，用于 etcd 服务发现
func (rm *ReconnectManager) SetLookupAddress(fn func(serverID string) string) {
	rm.lookupAddress = fn
}

// Run 运行重连管理器事件循环
func (rm *ReconnectManager) Run() {
	defer close(rm.doneCh)
	for {
		select {
		case <-rm.stopCh:
			return
		case <-rm.disconnectCh:
			rm.doReconnect()
		}
	}
}

// Stop 停止重连管理器
func (rm *ReconnectManager) Stop() {
	close(rm.stopCh)
	<-rm.doneCh
}

// NotifyDisconnection 通知发生断线，触发重连
func (rm *ReconnectManager) NotifyDisconnection() {
	select {
	case rm.disconnectCh <- struct{}{}:
	default:
	}
}

// doReconnect 执行重连逻辑，支持指数退避和 etcd 服务发现
func (rm *ReconnectManager) doReconnect() {
	interval := rm.config.InitialInterval
	attempt := 0
	originalAddress := rm.lc.address

	for {
		select {
		case <-rm.stopCh:
			return
		default:
		}

		if rm.config.MaxAttempts > 0 && attempt >= rm.config.MaxAttempts {
			tlog.Error("max reconnect attempts reached, trying etcd discovery",
				"maxAttempts", rm.config.MaxAttempts, "serverID", rm.lc.serverID)

			// 尝试通过 etcd 服务发现查找替代节点。
			// 尝试通过 etcd 发现替代节点
			if rm.lookupAddress != nil {
				if newAddr := rm.lookupAddress(rm.lc.serverID); newAddr != "" && newAddr != originalAddress {
					tlog.Info("discovered replacement address from etcd",
						"serverID", rm.lc.serverID, "oldAddress", originalAddress, "newAddress", newAddr)
					rm.lc.mu.Lock()
					rm.lc.address = newAddr
					rm.lc.mu.Unlock()
					if err := rm.lc.doConnect(true); err == nil {
						tlog.Info("reconnect to replacement node succeeded", "address", newAddr)
						return
					}
					tlog.Warn("reconnect to replacement node failed", "address", newAddr)
				}
			}
			return
		}

		attempt++
		tlog.Info("attempting reconnect", "attempt", attempt, "address", rm.lc.address)

		select {
		case <-rm.stopCh:
			return
		case <-time.After(interval):
		}

		err := rm.lc.doConnect(true)
		if err == nil {
			tlog.Info("reconnect successful", "attempt", attempt)
			return
		}

		tlog.Warn("reconnect failed", "attempt", attempt, "error", err)

		interval = time.Duration(float64(interval) * rm.config.Multiplier)
		if interval > rm.config.MaxInterval {
			interval = rm.config.MaxInterval
		}
	}
}

// StreamMessageQueue 流消息队列，断线期间缓存消息，重连后冲刷
type StreamMessageQueue struct {
	queue                 []*protoGw.StreamData // 消息队列
	mu                    sync.Mutex            // 互斥锁
	cond                  *sync.Cond            // 条件变量，用于阻塞等待
	maxSize               int                   // 队列最大容量
	policy                config.QueuePolicy    // 队列策略
	blockTimeout          time.Duration         // 阻塞超时
	backpressureThreshold float64               // 背压阈值
}

// NewStreamMessageQueue 创建消息队列
func NewStreamMessageQueue(cfg config.StreamQueueConfig) *StreamMessageQueue {
	maxSize := cfg.MaxSize
	if maxSize <= 0 {
		maxSize = 100000
	}
	blockTimeout := 500 * time.Millisecond
	if cfg.BlockTimeout != "" {
		if d, err := time.ParseDuration(cfg.BlockTimeout); err == nil {
			blockTimeout = d
		}
	}
	threshold := cfg.BackpressureThreshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	mq := &StreamMessageQueue{
		queue:                 make([]*protoGw.StreamData, 0),
		maxSize:               maxSize,
		policy:                cfg.Policy,
		blockTimeout:          blockTimeout,
		backpressureThreshold: threshold,
	}
	mq.cond = sync.NewCond(&mq.mu)
	return mq
}

// Enqueue 将消息入队，根据策略选择阻塞/超时/背压/丢弃
func (mq *StreamMessageQueue) Enqueue(msg *protoGw.StreamData) error {
	mq.mu.Lock()
	switch mq.policy {
	case config.QueuePolicyBlock:
		for len(mq.queue) >= mq.maxSize {
			mq.cond.Wait()
		}
		mq.queue = append(mq.queue, msg)
		mq.mu.Unlock()
		return nil

	case config.QueuePolicyTimeout:
		deadline := time.Now().Add(mq.blockTimeout)
		for len(mq.queue) >= mq.maxSize {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				mq.mu.Unlock()
				return ErrQueueFull
			}
			mq.mu.Unlock()
			time.Sleep(time.Millisecond)
			mq.mu.Lock()
			if len(mq.queue) < mq.maxSize {
				break
			}
		}
		mq.queue = append(mq.queue, msg)
		mq.mu.Unlock()
		return nil

	case config.QueuePolicyBackpressure:
		if float64(len(mq.queue))/float64(mq.maxSize) >= mq.backpressureThreshold {
			mq.mu.Unlock()
			return ErrBackpressure
		}
		if len(mq.queue) >= mq.maxSize {
			mq.queue = mq.queue[1:]
		}
		mq.queue = append(mq.queue, msg)
		mq.cond.Signal()
		mq.mu.Unlock()
		return nil

	default: // config.QueuePolicyDrop 或未知策略
		if len(mq.queue) >= mq.maxSize {
			mq.queue = mq.queue[1:]
		}
		mq.queue = append(mq.queue, msg)
		mq.cond.Signal()
		mq.mu.Unlock()
		return nil
	}
}

// Dequeue 从队列头部取出一条消息
func (mq *StreamMessageQueue) Dequeue() (*protoGw.StreamData, bool) {
	mq.mu.Lock()
	if len(mq.queue) == 0 {
		mq.mu.Unlock()
		return nil, false
	}
	msg := mq.queue[0]
	mq.queue = mq.queue[1:]
	mq.mu.Unlock()
	return msg, true
}

// Flush 冲刷队列中的消息，重连后调用以恢复转发
func (mq *StreamMessageQueue) Flush(lc *LogicClient) {
	maxRetries := 100
	for i := 0; i < maxRetries; i++ {
		msg, ok := mq.Dequeue()
		if !ok {
			return
		}

		lc.mu.RLock()
		state := lc.getState()
		closing := lc.closing
		lc.mu.RUnlock()

		if closing {
			return
		}

		if state == LogicStateConnected {
			err := lc.SendMessageDirect(msg)
			if err != nil {
				_ = mq.Enqueue(msg)
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			_ = mq.Enqueue(msg)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// GatewayInterface 网关接口，定义后端网关提供的能力
type GatewayInterface interface {
	GetConnectionManager() *ConnectionManager
	GetGRPCConfig() config.GRPCConfig
	GetStreamConfig() config.StreamConfig
	GetGatewayID() string
	GetServerID() string
	AddPushedToClient(n int64)
	AddPushDroppedNoConn(n int64)
	GetLogicClient(serverID string) LogicClientProvider
	LookupLogicAddress(serverID string) string
	GetGatewayClient(serverID string) GatewayClientProvider
}

// GRPCServer gRPC 服务端，处理逻辑服的流式推送和 RPC 请求
type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer
	gateway GatewayInterface
	mu      sync.Mutex
}

// NewGRPCServer 创建 gRPC 服务端实例
func NewGRPCServer(gateway GatewayInterface) *GRPCServer {
	return &GRPCServer{
		gateway: gateway,
	}
}

// OnData 处理来自逻辑服的流式数据（服务端流 RPC）
func (s *GRPCServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	connectionID := generateConnectionID()

	ctx := map[string]interface{}{
		"connection_id": connectionID,
		"stream":        stream,
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		s.handleGRPCMessage(connectionID, msg, func(response interface{}) {
			if protoMsg, ok := response.(*protoGw.StreamData); ok {
				stream.Send(protoMsg)
			} else if errorMsg, ok := response.(*commonstruct.ErrorResponse); ok {
				responseMsg := &protoGw.StreamData{
					Data: []byte(errorMsg.Error.Message),
				}
				stream.Send(responseMsg)
			}
		}, ctx)
	}
}

// connection 根据会话 ID 获取客户端连接
func (s *GRPCServer) connection(sessionID string) (*Connection, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	conn := s.gateway.GetConnectionManager().GetConnection(sessionID)
	if conn == nil {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	return conn, nil
}

// CloseSession 关闭指定会话的客户端连接
func (s *GRPCServer) CloseSession(_ context.Context, req *protoGw.CloseSessionReq) (*protoGw.CloseSessionAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return &protoGw.CloseSessionAck{}, nil
}

// KickSession 踢出会话，强制关闭客户端连接
func (s *GRPCServer) KickSession(ctx context.Context, req *protoGw.KickSessionReq) (*protoGw.KickSessionAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return &protoGw.KickSessionAck{}, nil
}

// SendToClient 向指定会话推送消息
func (s *GRPCServer) SendToClient(_ context.Context, req *protoGw.SendToClientReq) (*protoGw.SendToClientAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Send(encodePushMessage(req.GetCmd(), req.GetData())); err != nil {
		return nil, err
	}
	s.gateway.AddPushedToClient(1)
	return &protoGw.SendToClientAck{}, nil
}

// Broadcast 向指定分组广播消息
func (s *GRPCServer) Broadcast(_ context.Context, req *protoGw.BroadcastReq) (*protoGw.BroadcastAck, error) {
	var totalSent, totalFailed int
	for _, groupID := range req.GetGroupId() {
		sent, failed := s.broadcastGroup(groupID, req.GetCmd(), req.GetData())
		totalSent += sent
		totalFailed += failed
	}
	if totalSent == 0 && totalFailed > 0 {
		return nil, fmt.Errorf("broadcast failed: all %d sessions unreachable", totalFailed)
	}
	if totalFailed > 0 {
		tlog.Warn("broadcast partial success", "sent", totalSent, "failed", totalFailed)
	}
	return &protoGw.BroadcastAck{}, nil
}

// BroadcastAll 向所有客户端广播消息
func (s *GRPCServer) BroadcastAll(_ context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	var firstErr error
	s.gateway.GetConnectionManager().connections.Range(func(_ string, conn *Connection) bool {
		if err := conn.Send(encodePushMessage(req.GetCmd(), req.GetData())); err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			s.gateway.AddPushedToClient(1)
		}
		return true
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return &protoGw.BroadcastAllAck{}, nil
}

// JoinGroup 将会话加入指定分组
func (s *GRPCServer) JoinGroup(_ context.Context, req *protoGw.JoinGroupReq) (*protoGw.JoinGroupAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	serverID := conn.GetServerID()
	userUUID := conn.GetUserUUID()
	counts := make([]int32, len(req.GetGroupId()))
	for i, groupID := range req.GetGroupId() {
		s.gateway.GetConnectionManager().AddUserToGroup(groupID, serverID, userUUID)
		counts[i] = int32(s.gateway.GetConnectionManager().GetGroupMemberCount(groupID))
	}
	return &protoGw.JoinGroupAck{Code: 0, MemberCount: counts}, nil
}

// LeaveGroup 将会话移出指定分组
func (s *GRPCServer) LeaveGroup(_ context.Context, req *protoGw.LeaveGroupReq) (*protoGw.LeaveGroupAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	serverID := conn.GetServerID()
	userUUID := conn.GetUserUUID()
	counts := make([]int32, len(req.GetGroupId()))
	for i, groupID := range req.GetGroupId() {
		s.gateway.GetConnectionManager().RemoveUserFromGroup(groupID, serverID, userUUID)
		counts[i] = int32(s.gateway.GetConnectionManager().GetGroupMemberCount(groupID))
	}
	return &protoGw.LeaveGroupAck{Code: 0, MemberCount: counts}, nil
}

// GetGroupInfo 获取分组信息，包括成员数量和会话列表
func (s *GRPCServer) GetGroupInfo(_ context.Context, req *protoGw.GetGroupInfoReq) (*protoGw.GetGroupInfoAck, error) {
	cm := s.gateway.GetConnectionManager()
	return &protoGw.GetGroupInfoAck{
		GroupId:     req.GetGroupId(),
		MemberCount: int32(cm.GetGroupMemberCount(req.GetGroupId())),
		SessionIds:  cm.GetGroupSessions(req.GetGroupId()),
	}, nil
}

// broadcastGroup 向指定分组的所有成员推送消息
func (s *GRPCServer) broadcastGroup(groupID string, cmd int32, data []byte) (sent int, failed int) {
	if groupID == "" {
		return 0, 1
	}
	cm := s.gateway.GetConnectionManager()
	sessions := cm.GetGroupSessions(groupID)
	for _, sessionID := range sessions {
		conn := cm.GetConnection(sessionID)
		if conn == nil {
			failed++
			tlog.Warn("group push: session disappeared", "groupID", groupID, "sessionID", sessionID)
			continue
		}
		msg := encodePushMessage(cmd, data)
		if err := conn.Send(msg); err != nil {
			failed++
			tlog.Warn("group push: send failed", "groupID", groupID, "sessionID", sessionID, "error", err)
			continue
		}
		sent++
		s.gateway.AddPushedToClient(1)
	}
	if failed > 0 {
		tlog.Warn("group push completed with failures", "groupID", groupID, "total", len(sessions), "sent", sent, "failed", failed)
	}
	return sent, failed
}

// encodePushMessage 编码推送消息为字节流
func encodePushMessage(cmd int32, data []byte) []byte {
	msg, _ := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: data})
	return msg
}

// SendMessage 处理来自逻辑服的单条消息请求（Unary RPC）
func (s *GRPCServer) SendMessage(ctx context.Context, msg *protoGw.StreamData) (*protoGw.StreamData, error) {
	connectionID := generateConnectionID()

	grpcCtx := map[string]interface{}{
		"connection_id": connectionID,
		"context":       ctx,
	}

	var response *protoGw.StreamData
	var wg sync.WaitGroup
	wg.Add(1)

	s.handleGRPCMessage(connectionID, msg, func(resp interface{}) {
		defer wg.Done()
		if protoMsg, ok := resp.(*protoGw.StreamData); ok {
			response = protoMsg
		} else if errorMsg, ok := resp.(*commonstruct.ErrorResponse); ok {
			response = &protoGw.StreamData{
				Data: []byte(errorMsg.Error.Message),
			}
		}
	}, grpcCtx)

	wg.Wait()
	return response, nil
}

// handleGRPCMessage 处理 gRPC 消息，返回错误提示（网关不直接处理命令）
func (s *GRPCServer) handleGRPCMessage(connectionID string, msg *protoGw.StreamData, callback func(interface{}), ctx map[string]interface{}) {
	if msg.Cmd == 0 {
		callback(newErrorResponse("error", "Missing cmd", "", ""))
		return
	}
	callback(newErrorResponse("error", "Gateway does not handle commands locally, forward to logic server", "", ""))
}

// StartGRPCServer 启动 gRPC 服务器，监听指定端口
func StartGRPCServer(gw GatewayInterface, port string, maxMsgSize int, windowSize int) (*grpc.Server, error) {
	if maxMsgSize <= 0 {
		maxMsgSize = 4 * 1024 * 1024
	}
	if windowSize <= 0 {
		windowSize = 524288
	}
	tlog.Info("creating gRPC server")
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
		grpc.InitialWindowSize(int32(windowSize)),
		grpc.InitialConnWindowSize(int32(windowSize)),
	)
	tlog.Info("registering GatewayService")
	grpcService := NewGRPCServer(gw)
	protoGw.RegisterGatewayStreamServer(server, grpcService)
	protoGw.RegisterGatewayServer(server, grpcService)

	tlog.Info("listening on port", "port", port)
	listener, err := net.Listen("tcp", port)
	if err != nil {
		tlog.Error("failed to listen on port", "error", err, "port", port)
		return nil, err
	}

	go func() {
		if err := server.Serve(listener); err != nil {
			tlog.Error("gRPC server failed", "error", err)
		}
	}()

	tlog.Info("gRPC server started", "port", port)
	return server, nil
}

// LogicClientPool 逻辑服客户端池，管理多个逻辑服实例的连接
type LogicClientPool struct {
	clients    map[string]*LogicClient     // 逻辑服客户端映射（serverID -> client）
	ordered    []string                    // 有序的服务 ID 列表，用于确定性轮询
	mu         sync.RWMutex                // 读写锁
	gateway    GatewayInterface            // 网关接口引用
	discovery  *uetcd.Component            // 服务发现组件
	balancer   *cluster.Balancer           // 负载均衡器
	stopCh     chan struct{}               // 停止信号
	wg         sync.WaitGroup              // 等待协程退出
	rrIndex    uint64                      // 轮询索引（原子操作）
	fastClient atomic.Pointer[LogicClient] // 快速路径：单客户端时的原子指针
	addressMap map[string]string           // 地址映射（serverID -> address，来自 etcd）
}

// RegisterClient 注册逻辑服客户端到池中
func (pool *LogicClientPool) RegisterClient(serverID string, client *LogicClient) {
	if serverID == "" || client == nil {
		return
	}
	client.SetServerID(serverID)
	pool.mu.Lock()
	pool.clients[serverID] = client
	if !containsString(pool.ordered, serverID) {
		pool.ordered = append(pool.ordered, serverID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()
}

// GetClient 根据 serverID 获取已连接的逻辑服客户端
func (pool *LogicClientPool) GetClient(serverID string) LogicClientProvider {
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	return client
}

// NewLogicClientPool 创建逻辑服客户端池
func NewLogicClientPool(gateway GatewayInterface) *LogicClientPool {
	return &LogicClientPool{
		clients:    make(map[string]*LogicClient),
		addressMap: make(map[string]string),
		gateway:    gateway,
		stopCh:     make(chan struct{}),
	}
}

// updateFastClient 更新快速路径指针，必须在持有 pool.mu 时调用。
// 当且仅当有 1 个客户端连接时设置 fastClient，否则置 nil。
func (pool *LogicClientPool) updateFastClient() {
	if len(pool.clients) == 1 {
		for _, c := range pool.clients {
			pool.fastClient.Store(c)
			return
		}
	}
	pool.fastClient.Store(nil)
}

// LookupAddress 从 etcd 维护的映射中获取指定 serverID 的地址
func (pool *LogicClientPool) LookupAddress(serverID string) string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.addressMap[serverID]
}

// SetDiscovery 设置服务发现组件并监听服务变更。
// 注册回调后立即重放已知服务，避免因 discovery 启动先于回调注册而丢失初始快照。
func (pool *LogicClientPool) SetDiscovery(discovery *uetcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
	svcs := discovery.ServiceSet()
	tlog.Info("SetDiscovery: replaying known services", "count", len(svcs))
	for fullKey, address := range svcs {
		instanceID := fullKey[strings.LastIndex(fullKey, "/")+1:]
		tlog.Info("SetDiscovery: replaying service", "instanceID", instanceID, "address", address)
		pool.handleServiceRegister(uetcd.ServiceEvent{
			Type:       uetcd.EventRegister,
			ServiceID:  discovery.ServiceID(),
			InstanceID: instanceID,
			Address:    address,
		})
	}
}

// SetBalancer 设置负载均衡器
func (pool *LogicClientPool) SetBalancer(balancer *cluster.Balancer) {
	pool.balancer = balancer
}

// handleServiceChange 处理服务注册/注销事件
func (pool *LogicClientPool) handleServiceChange(event uetcd.ServiceEvent) {
	switch event.Type {
	case uetcd.EventRegister:
		pool.handleServiceRegister(event)
	case uetcd.EventDeregister:
		pool.handleServiceDeregister(event)
	}
}

// handleServiceRegister 处理服务注册事件，创建新的逻辑服客户端连接
func (pool *LogicClientPool) handleServiceRegister(event uetcd.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	// 已存在的客户端已经负责连接或重连，避免初始快照和 watch 事件重复创建。
	if exists && existing != nil {
		return
	}

	client := NewLogicClient(pool.gateway)
	client.SetServerID(event.InstanceID)
	client.shardCount = runtime.NumCPU() * 8

	go func() {
		tlog.Info("connecting to discovered logic service",
			"serviceID", event.InstanceID,
			"address", event.Address,
		)
		for attempt := 1; ; attempt++ {
			if err := client.Connect(event.Address); err == nil {
				tlog.Info("connected to discovered logic service",
					"serviceID", event.InstanceID,
					"address", event.Address,
				)
				return
			} else {
				client.mu.RLock()
				closing := client.closing
				client.mu.RUnlock()
				if closing {
					return
				}
				if attempt == 1 || attempt%10 == 0 {
					tlog.Warn("logic service connection failed, retrying",
						"serviceID", event.InstanceID,
						"address", event.Address,
						"attempt", attempt,
						"error", err,
					)
				}
				time.Sleep(time.Second)
			}
		}
	}()

	pool.mu.Lock()
	pool.clients[event.InstanceID] = client
	pool.addressMap[event.InstanceID] = event.Address
	if !containsString(pool.ordered, event.InstanceID) {
		pool.ordered = append(pool.ordered, event.InstanceID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()

	if pool.balancer != nil {
		pool.balancer.AddNode(event.InstanceID, event.Address, 1)
	}

	tlog.Info("logic client added to pool",
		"serviceID", event.InstanceID,
		"address", event.Address,
		"totalClients", pool.ClientCount(),
	)
}

// handleServiceDeregister 处理服务注销事件
// 注意：不立即删除和关闭连接，避免 etcd 租约过期但 gRPC 连接仍可用时的误判
// 让健康检查器和 gRPC 流自身错误检测来处理真正的连接断开
func (pool *LogicClientPool) handleServiceDeregister(event uetcd.ServiceEvent) {
	// 服务发现租约可能在现有 gRPC 连接仍可用时过期。
	// 不立即从连接池删除和关闭连接，避免误判导致转发中断。
	// 让 HealthChecker 和 gRPC 流自身错误检测来处理真正的连接断开。
	pool.mu.RLock()
	client, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && client != nil {
		if !client.IsConnected() {
			// gRPC 连接已断开，安全清理
			pool.mu.Lock()
			delete(pool.clients, event.InstanceID)
			delete(pool.addressMap, event.InstanceID)
			pool.ordered = removeString(pool.ordered, event.InstanceID)
			pool.updateFastClient()
			pool.mu.Unlock()
			if pool.balancer != nil {
				pool.balancer.RemoveNode(event.InstanceID)
			}
			go client.Close()
			tlog.Warn("logic service offline and connection already disconnected, cleaning up",
				"serviceID", event.InstanceID,
				"address", event.Address,
			)
		} else {
			// gRPC 连接仍存活，保留连接，等服务重新注册或 HealthChecker 检测到断开
			tlog.Warn("logic service deregistered from etcd, keeping gRPC connection (still connected)",
				"serviceID", event.InstanceID,
				"address", event.Address,
			)
		}
	}

	tlog.Warn("logic client deregister event processed",
		"serviceID", event.InstanceID,
		"address", event.Address,
		"totalClients", pool.ClientCount(),
	)
}

// SendMessage 发送消息到逻辑服，优先使用快速路径
func (pool *LogicClientPool) SendMessage(msg *protoGw.StreamData) error {
	// 快速路径：只有一个客户端，无需加锁。
	// 快速路径：单客户端时无需加锁
	if c := pool.fastClient.Load(); c != nil {
		return c.SendMessage(msg)
	}
	return pool.RoundRobinSendMessage(msg)
}

// SendMessageTo 向指定逻辑服发送会话绑定消息
// 不会回退到轮询，因为那样可能跨服务器分片
func (pool *LogicClientPool) SendMessageTo(serverID string, msg *protoGw.StreamData) error {
	if serverID == "" {
		return ErrNotConnected
	}
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return ErrNotConnected
	}
	return client.SendMessage(msg)
}

// RoundRobinSendMessage 使用轮询方式发送消息到逻辑服
func (pool *LogicClientPool) RoundRobinSendMessage(msg *protoGw.StreamData) error {
	pool.mu.RLock()
	n := len(pool.ordered)
	if n == 0 {
		pool.mu.RUnlock()
		return ErrNotConnected
	}

	idx := atomic.AddUint64(&pool.rrIndex, 1) % uint64(n)
	serviceID := pool.ordered[idx]
	client := pool.clients[serviceID]
	pool.mu.RUnlock()

	if client == nil || !client.IsConnected() {
		return ErrNotConnected
	}
	return client.SendMessage(msg)
}

// Close 关闭客户端池中所有连接
func (pool *LogicClientPool) Close() {
	close(pool.stopCh)
	pool.wg.Wait()

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for id, client := range pool.clients {
		client.Close()
		delete(pool.clients, id)
	}
	pool.ordered = pool.ordered[:0]
}

// ClientCount 获取客户端池中的客户端数量
func (pool *LogicClientPool) ClientCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return len(pool.clients)
}

// RemoveService 移除指定服务的客户端
func (pool *LogicClientPool) RemoveService(serviceID string) {
	pool.mu.Lock()
	client, exists := pool.clients[serviceID]
	if exists {
		delete(pool.clients, serviceID)
		pool.ordered = removeString(pool.ordered, serviceID)
		pool.updateFastClient()
	}
	pool.mu.Unlock()

	if exists {
		if pool.balancer != nil {
			pool.balancer.RemoveNode(serviceID)
		}
		if client != nil {
			go client.Close()
		}
	}
}

// IsConnected 检查池中是否有已连接的客户端
func (pool *LogicClientPool) IsConnected() bool {
	// 快速路径：只有一个客户端，无需加锁。
	// 快速路径：单客户端时无需加锁
	if c := pool.fastClient.Load(); c != nil {
		return c.IsConnected()
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	for _, client := range pool.clients {
		if client.IsConnected() {
			return true
		}
	}
	return false
}

// containsString 检查字符串切片是否包含指定字符串
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// removeString 从字符串切片中移除指定字符串
func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

// ---------------------------------------------------------------------------
// GatewayClient / GatewayClientPool – 网关间 gRPC 客户端池
// ---------------------------------------------------------------------------

// GatewayClient 封装到另一个网关实例的单个 gRPC 连接
type GatewayClient struct {
	client     protoGw.GatewayClient // gRPC 客户端
	conn       *grpc.ClientConn      // gRPC 连接
	address    string                // 目标网关地址
	serverID   string                // 目标网关标识
	mu         sync.RWMutex          // 读写锁
	closing    bool                  // 是否正在关闭
	connecting bool                  // 是否正在连接中（Connect 期间）
}

// NewGatewayClient 创建网关客户端实例
func NewGatewayClient(serverID, address string) *GatewayClient {
	return &GatewayClient{
		address:  address,
		serverID: serverID,
	}
}

// Connect 建立到目标网关的 gRPC 连接
func (gc *GatewayClient) Connect() error {
	// 标记正在连接中，阻止 Close() 在连接过程中直接关闭
	gc.mu.Lock()
	gc.connecting = true
	gc.mu.Unlock()

	defer func() {
		gc.mu.Lock()
		gc.connecting = false
		wasClosing := gc.closing
		gc.mu.Unlock()
		// 如果 Connect 期间有 Close() 被调用，这里完成实际关闭
		if wasClosing && gc.conn != nil {
			gc.conn.Close()
		}
	}()

	conn, err := grpc.NewClient(gc.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("dial gateway %s (%s): %w", gc.serverID, gc.address, err)
	}

	gc.mu.Lock()
	if gc.closing {
		gc.mu.Unlock()
		conn.Close()
		// 连接期间被关闭属于正常生命周期事件，不返回错误
		return nil
	}
	gc.conn = conn
	gc.client = protoGw.NewGatewayClient(conn)
	gc.mu.Unlock()

	tlog.Info("网关客户端已创建", "serverID", gc.serverID, "address", gc.address)
	return nil
}

// IsConnected 检查网关客户端连接状态
func (gc *GatewayClient) IsConnected() bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.conn != nil && !gc.closing && gc.conn.GetState() != connectivity.Shutdown
}

// Client 获取底层 gRPC 客户端
func (gc *GatewayClient) Client() protoGw.GatewayClient {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.client
}

// Close 关闭网关客户端连接
func (gc *GatewayClient) Close() {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.closing {
		return
	}
	gc.closing = true
	// 如果正在连接中，由 Connect() 的 defer 完成实际关闭
	if gc.connecting {
		return
	}
	if gc.conn != nil {
		gc.conn.Close()
	}
}

// GatewayClientPool 网关客户端池，管理通过 etcd 发现的其他网关实例的连接
// 结构类似于 LogicClientPool，但更简单，因为网关间通信使用 Unary RPC（无流）
type GatewayClientPool struct {
	clients    map[string]*GatewayClient // 网关客户端映射
	gens       map[string]uint64         // 每个 serverID 的注册代次，防止 deregister 误关新 client
	mu         sync.RWMutex              // 读写锁
	discovery  *uetcd.Component          // 服务发现组件
	addressMap map[string]string         // 地址映射（serverID → address）
	selfID     string                    // 本实例 ID（排除自身）
	nextGen    uint64                    // 全局递增代次计数器
}

func NewGatewayClientPool(gateway GatewayInterface) *GatewayClientPool {
	return &GatewayClientPool{
		clients:    make(map[string]*GatewayClient),
		gens:       make(map[string]uint64),
		addressMap: make(map[string]string),
		selfID:     gateway.GetServerID(),
	}
}

func (pool *GatewayClientPool) LoadEvents(events []uetcd.ServiceEvent) {
	for _, event := range events {
		pool.handleServiceChange(event)
	}
}

func (pool *GatewayClientPool) GetClient(serverID string) GatewayClientProvider {
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	return client
}

func (pool *GatewayClientPool) LookupAddress(serverID string) string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.addressMap[serverID]
}

func (pool *GatewayClientPool) SetDiscovery(discovery *uetcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *GatewayClientPool) handleServiceChange(event uetcd.ServiceEvent) {
	// 专用发现组件监听 Gateway:{zone}，忽略当前网关实例自身。
	if event.InstanceID == pool.selfID {
		return
	}
	switch event.Type {
	case uetcd.EventRegister:
		pool.handleRegister(event)
	case uetcd.EventDeregister:
		pool.handleDeregister(event)
	}
}

func (pool *GatewayClientPool) handleRegister(event uetcd.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && existing != nil && existing.IsConnected() && existing.address == event.Address {
		return
	}

	// 清理已经失效的旧条目。
	if exists && existing != nil && !existing.IsConnected() {
		pool.mu.Lock()
		delete(pool.clients, event.InstanceID)
		delete(pool.addressMap, event.InstanceID)
		delete(pool.gens, event.InstanceID)
		pool.mu.Unlock()
		go existing.Close()
	}

	// 递增代次，后续 deregister 事件只能关闭此代次之前的 client
	pool.mu.Lock()
	pool.nextGen++
	gen := pool.nextGen
	pool.gens[event.InstanceID] = gen
	pool.mu.Unlock()

	client := NewGatewayClient(event.InstanceID, event.Address)
	go func() {
		tlog.Info("正在连接已发现的网关", "serverID", event.InstanceID, "address", event.Address)
		if err := client.Connect(); err != nil {
			tlog.Error("连接已发现的网关失败",
				"serverID", event.InstanceID, "address", event.Address, "error", err)
			return
		}
		tlog.Info("已发现的网关连接就绪", "serverID", event.InstanceID, "address", event.Address)
	}()

	pool.mu.Lock()
	// 如果代次已被更新（新的 register 已到来），不再覆盖
	if pool.gens[event.InstanceID] == gen {
		pool.clients[event.InstanceID] = client
		pool.addressMap[event.InstanceID] = event.Address
	}
	pool.mu.Unlock()

	tlog.Info("网关客户端已加入池",
		"serverID", event.InstanceID, "address", event.Address, "gen", gen, "totalClients", pool.ClientCount())
}

func (pool *GatewayClientPool) handleDeregister(event uetcd.ServiceEvent) {
	// 取出当前注册的 client 和代次
	pool.mu.Lock()
	client, exists := pool.clients[event.InstanceID]
	gen := pool.gens[event.InstanceID]
	if exists {
		delete(pool.clients, event.InstanceID)
		delete(pool.addressMap, event.InstanceID)
		delete(pool.gens, event.InstanceID)
	}
	// 记录当前全局代次，用于判断是否有新的 register 已到来
	currentGen := pool.nextGen
	pool.mu.Unlock()

	if client != nil {
		if !client.IsConnected() {
			go client.Close()
			tlog.Warn("网关下线且连接已断开，安全清理",
				"serverID", event.InstanceID, "address", event.Address)
		} else if gen < currentGen {
			// 此 client 对应的代次已过时（有新的 register 已到来），关闭旧 client
			go client.Close()
			tlog.Warn("网关代次已更新，关闭旧连接",
				"serverID", event.InstanceID, "address", event.Address, "gen", gen, "currentGen", currentGen)
		} else {
			tlog.Warn("网关已从 etcd 注销，但 gRPC 连接仍存活，保留连接",
				"serverID", event.InstanceID, "address", event.Address)
		}
	}

	tlog.Warn("网关注销事件处理完成",
		"serverID", event.InstanceID, "address", event.Address, "totalClients", pool.ClientCount())
}

func (pool *GatewayClientPool) ClientCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return len(pool.clients)
}

func (pool *GatewayClientPool) IsConnected() bool {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, c := range pool.clients {
		if c.IsConnected() {
			return true
		}
	}
	return false
}

func (pool *GatewayClientPool) Close() {
	pool.mu.Lock()
	clients := make([]*GatewayClient, 0, len(pool.clients))
	for _, c := range pool.clients {
		clients = append(clients, c)
	}
	pool.clients = make(map[string]*GatewayClient)
	pool.addressMap = make(map[string]string)
	pool.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
}
