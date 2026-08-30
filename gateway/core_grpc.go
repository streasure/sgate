package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/util/nacos"
	"github.com/streasure/sgate/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

type LogicConnectionState int32

const (
	StateDisconnected LogicConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
)

func (s LogicConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "Disconnected"
	case StateConnecting:
		return "Connecting"
	case StateConnected:
		return "Connected"
	case StateReconnecting:
		return "Reconnecting"
	default:
		return "Unknown"
	}
}

type LogicConnectionStateCallback func(oldState, newState LogicConnectionState)

var (
	ErrNotConnected      = errors.New("not connected to logic server")
	ErrConnectionClosing = errors.New("connection is closing")
)

// marshalBufPool 复用 proto 序列化缓冲区，消除 startSendLoop 热路径上的 []byte 分配
var marshalBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 4096)
		return &b
	},
}

type ReconnectConfig struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxAttempts     int
	Multiplier      float64
}

var DefaultReconnectConfig = ReconnectConfig{
	InitialInterval: 1 * time.Second,
	MaxInterval:     30 * time.Second,
	MaxAttempts:     0,
	Multiplier:      2.0,
}

type HealthCheckConfig struct {
	Interval    time.Duration
	Timeout     time.Duration
	MaxFailures int
	// Enabled 控制是否对逻辑服做主动健康检查（ping）。
	// 默认 true：主动 ping 并在连续失败超阈值后重连，保障容灾切换。
	Enabled bool
}

var DefaultHealthCheckConfig = HealthCheckConfig{
	Interval:    5 * time.Second,
	Timeout:     3 * time.Second,
	MaxFailures: 3,
	Enabled:     true,
}

type StreamShard struct {
	stream protobuf.GatewayService_StreamMessagesClient
	mu     sync.Mutex
	sendCh chan *protobuf.Message
	ctx    context.Context
	cancel context.CancelFunc
	index  int
	lc     *LogicClient
	// closed is set atomically when the shard's sendCh is closed.
	// Allows SendMessage to skip the defer/recover overhead in the fast path.
	closed atomic.Bool
}

type StreamManager struct {
	shards []*StreamShard
}

func NewStreamManager(shardCount int, sendChannelSize int) *StreamManager {
	if shardCount <= 0 {
		shardCount = runtime.NumCPU() * 4
	}
	if sendChannelSize <= 0 {
		sendChannelSize = 65536
	}
	sm := &StreamManager{
		shards: make([]*StreamShard, shardCount),
	}
	for i := range sm.shards {
		sm.shards[i] = &StreamShard{
			sendCh: make(chan *protobuf.Message, sendChannelSize),
			index:  i,
		}
	}
	return sm
}

// writeCoalescer 跨多个 RouteBatch 累积反向推送数据，按连接合并后一次性 flush。
// 目的：减少 gnet AsyncWrite 调用次数。每次 AsyncWrite 向 event-loop channel 发送一个 task，
// 在 Windows 上 channel send 竞争 runtime 互斥锁（runtime.lock2），94 个 receiveMessages
// goroutine 同时发送时 lock 竞争达 74% CPU。通过跨 batch 合并，将 N 次 SendMulti
// 降为 M 次（M=不同连接数），减少 channel send 约 10-50 倍。
//
// 内存优化：每个 entry 的 data buffer 从 coalescerBufPool 获取，在 AsyncWrite 完成后
// 通过 callback 归还到池，避免每帧分配导致 GC 压力（千万级 QPS 下 GC 无法跟上分配速度）。
type writeCoalescer struct {
	entries   []coalescedEntry // 每个连接一个 entry，存储累积的帧数据
	index     map[string]int   // connID -> entries 下标，避免重复 GetConnection
	count     int              // 累积消息总数（用于触发 flush）
	cm        *ConnectionManager
	lastFlush time.Time
}

// coalescedEntry 累积一个连接的帧数据。
// bufPtr 持有指向池化 buffer 的指针，在 flush 后通过 AsyncWrite callback 归还。
type coalescedEntry struct {
	conn   *Connection
	data   []byte // [4字节 len][payload] 重复格式，底层数组来自 coalescerBufPool
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
	coalesceFlushCount    = 50000                 // 累积 5 万条消息后 flush，减少 event-loop 入队次数
	coalesceFlushInterval = 5 * time.Millisecond // 5ms 超时 flush，限制推送延迟
	coalescerMaxBufCap    = 1 << 20              // 1MB：归还到池的 buffer 容量上限，避免持有过大 buffer
)

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
// payload 是已序列化的单条消息 bytes。
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

// addSingle 将 single-conn 格式的整个 batch data 加入 coalescer。
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

func (wc *writeCoalescer) shouldFlush() bool {
	return wc.count >= coalesceFlushCount || time.Since(wc.lastFlush) >= coalesceFlushInterval
}

// flush 将所有连接的累积数据通过一次 SendMultiWithCallback 发送，然后重置。
// buffer 在 gnet AsyncWrite 完成后通过 callback 归还到 coalescerBufPool。
// 返回 pushed（成功推送的消息数）。
func (wc *writeCoalescer) flush() int64 {
	var pushed int64
	for i := range wc.entries {
		entry := &wc.entries[i]
		if len(entry.data) > 0 {
			bufPtr := entry.bufPtr
			// SendMultiWithCallback 在 gnet 写完成后调用 callback，
			// callback 归还 buffer 到池，消除每帧分配导致的 GC 压力
			entry.conn.SendMultiWithCallback(entry.data, func() {
				if bufPtr != nil && cap(*bufPtr) <= coalescerMaxBufCap {
					*bufPtr = (*bufPtr)[:0]
					coalescerBufPool.Put(bufPtr)
				}
			})
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

func (sm *StreamManager) GetShard(connectionID string) *StreamShard {
	h := uint32(2166136261)
	for i := 0; i < len(connectionID); i++ {
		h ^= uint32(connectionID[i])
		h *= 16777619
	}
	return sm.shards[h%uint32(len(sm.shards))]
}

// markShardBroken 分片流失效后的统一处理：触发整体重连。
// 没有这一步，logic 重启/网络闪断后 shard.stream 永远为 nil，
// 正向消息静默丢弃、反向推送归零，且 health check 的 ping 也只会
// 塞进已死的 sendCh 而永远探测不出故障。
func (s *StreamShard) markShardBroken() {
	s.mu.Lock()
	s.stream = nil
	s.mu.Unlock()
	if s.lc != nil {
		go s.lc.handleDisconnection()
	}
}

func (s *StreamShard) startSendLoop() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "startSendLoop shard %d panic recovered: %v\n", s.index, r)
		}
	}()
	const maxBatchCount = 256
	batch := make([]*protobuf.Message, 0, maxBatchCount)
	for {
		msg, ok := <-s.sendCh
		if !ok {
			return
		}

		// Pre-batched RouteBatch messages from handleBatchTraffic: send directly
		// to avoid double-batching overhead. These messages already contain
		// multiple frames packed into Data with ConnectionId on the outer message.
		// This is the hot path for high-throughput forwarding (gnet-level batching).
		if msg.Route == protobuf.RouteBatch {
			s.mu.Lock()
			stream := s.stream
			s.mu.Unlock()
			if stream != nil {
				if err := safeStreamSend(stream, msg); err != nil {
					tlog.Warn("shard send error, isolating shard", "shard", s.index, "error", err)
					s.markShardBroken()
				}
			}
			continue
		}

		batch = batch[:0]
		batch = append(batch, msg)
		drained := true
		for drained {
			select {
			case m, ok := <-s.sendCh:
				if !ok {
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

		func() {
			s.mu.Lock()
			stream := s.stream
			s.mu.Unlock()

			if stream == nil {
				return
			}

			// Fast path: single message, send directly to avoid batch overhead
			if len(batch) == 1 {
				if err := safeStreamSend(stream, batch[0]); err != nil {
					tlog.Warn("shard send error, isolating shard", "shard", s.index, "error", err)
					s.markShardBroken()
				}
				return
			}

			// Batch path: serialize multiple messages into a single RouteBatch
			// to reduce gRPC stream.Send calls by up to maxBatchCount times.
			// Format: [4-byte payloadLen][payload] repeated
			bufPtr := marshalBufPool.Get().(*[]byte)
			buf := (*bufPtr)[:0]
			count := 0
			for _, m := range batch {
				data, err := proto.Marshal(m)
				if err != nil {
					continue
				}
				var lenBuf [4]byte
				binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
				buf = append(buf, lenBuf[:]...)
				buf = append(buf, data...)
				count++
			}
			if count > 0 {
				batchMsg := &protobuf.Message{
					Route: protobuf.RouteBatch,
					Data:  buf,
					Cmd:   int32(count),
				}
				if err := safeStreamSend(stream, batchMsg); err != nil {
					tlog.Warn("shard batch send error, isolating shard", "shard", s.index, "error", err)
					s.markShardBroken()
				}
			}
			// Return buffer to pool (cap-based reuse, avoid holding large bufs)
			if cap(buf) <= 4<<20 {
				*bufPtr = buf
				marshalBufPool.Put(bufPtr)
			}
		}()
	}
}

// safeStreamSend wraps stream.Send() to recover from panics caused by
// concurrent close operations on the gRPC stream.
func safeStreamSend(stream protobuf.GatewayService_StreamMessagesClient, msg *protobuf.Message) (err error) {	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("stream send panic: %v", r)
		}
	}()
	return stream.Send(msg)
}

func (s *StreamShard) SendMessage(msg *protobuf.Message) (err error) {
	// Fast path: check closed flag atomically, skip defer/recover overhead
	if s.closed.Load() {
		return ErrNotConnected
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("send on closed channel: %v", r)
			}
		}()
		select {
		case s.sendCh <- msg:
			err = nil
		default:
			err = ErrNotConnected
		}
	}()
	return
}

type LogicClient struct {
	client            protobuf.GatewayServiceClient
	conn              *grpc.ClientConn
	mu                sync.RWMutex
	state             int32
	address           string
	streamManager     *StreamManager
	streamCtx         context.Context
	streamCancel      context.CancelFunc
	reconnectConfig   ReconnectConfig
	healthCheckConfig HealthCheckConfig
	healthChecker     *HealthChecker
	reconnectManager  *ReconnectManager
	messageQueue      *StreamMessageQueue
	stateCallbacks    []LogicConnectionStateCallback
	gateway           GatewayInterface
	closing           bool
	closed            chan struct{}
	shardCount        int
}

func NewLogicClient(gateway GatewayInterface) *LogicClient {
	return &LogicClient{
		state:             int32(StateDisconnected),
		reconnectConfig:   DefaultReconnectConfig,
		healthCheckConfig: DefaultHealthCheckConfig,
		streamManager:     NewStreamManager(0, 0),
		gateway:           gateway,
		closing:           false,
		closed:            make(chan struct{}),
		shardCount:        runtime.NumCPU() * 8,
		messageQueue:      NewStreamMessageQueue(),
	}
}

func NewLogicClientWithConfig(gateway GatewayInterface, reconnectConfig ReconnectConfig, healthCheckConfig HealthCheckConfig) *LogicClient {
	return &LogicClient{
		state:             int32(StateDisconnected),
		reconnectConfig:   reconnectConfig,
		healthCheckConfig: healthCheckConfig,
		streamManager:     NewStreamManager(0, 0),
		gateway:           gateway,
		closing:           false,
		closed:            make(chan struct{}),
		shardCount:        runtime.NumCPU() * 8,
		messageQueue:      NewStreamMessageQueue(),
	}
}

func (lc *LogicClient) RegisterStateCallback(callback LogicConnectionStateCallback) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.stateCallbacks = append(lc.stateCallbacks, callback)
}

func (lc *LogicClient) getState() LogicConnectionState {
	return LogicConnectionState(atomic.LoadInt32(&lc.state))
}

func (lc *LogicClient) setState(newState LogicConnectionState) {
	oldState := LogicConnectionState(atomic.LoadInt32(&lc.state))
	if oldState == newState {
		return
	}
	atomic.StoreInt32(&lc.state, int32(newState))
	lc.notifyStateChange(oldState, newState)
}

func (lc *LogicClient) notifyStateChange(oldState, newState LogicConnectionState) {
	lc.mu.RLock()
	callbacks := make([]LogicConnectionStateCallback, len(lc.stateCallbacks))
	copy(callbacks, lc.stateCallbacks)
	lc.mu.RUnlock()
	for _, callback := range callbacks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					tlog.Error("notifyStateChange callback panic recovered", "error", r)
				}
			}()
			callback(oldState, newState)
		}()
	}
}

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

func (lc *LogicClient) doConnect(isReconnect bool) error {
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		return ErrConnectionClosing
	}

	var oldState LogicConnectionState
	if isReconnect {
		oldState = LogicConnectionState(atomic.LoadInt32(&lc.state))
		atomic.StoreInt32(&lc.state, int32(StateReconnecting))
	} else {
		oldState = LogicConnectionState(atomic.LoadInt32(&lc.state))
		atomic.StoreInt32(&lc.state, int32(StateConnecting))
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
		lc.setState(StateDisconnected)
		return err
	}

	lc.mu.Lock()
	lc.conn = conn
	lc.client = protobuf.NewGatewayServiceClient(conn)
	lc.mu.Unlock()

	lc.mu.Lock()
	if lc.streamCancel != nil {
		lc.streamCancel()
	}
	lc.streamCtx, lc.streamCancel = context.WithCancel(context.Background())
	lc.mu.Unlock()

	// Shut down old stream shards: nil out the stream reference and close send channels
	// so that startSendLoop goroutines stop using the old (now-closed) streams.
	if lc.streamManager != nil {
		for i := 0; i < len(lc.streamManager.shards); i++ {
			if shard := lc.streamManager.shards[i]; shard != nil {
				shard.closed.Store(true)
				shard.mu.Lock()
				shard.stream = nil
				shard.mu.Unlock()
				close(shard.sendCh)
			}
		}
	}

	shardCount := lc.shardCount
	sendChannelSize := 0
	if lc.gateway != nil {
		streamCfg := lc.gateway.GetStreamConfig()
		sendChannelSize = streamCfg.SendChannelSize
		if streamCfg.ShardCount > 0 {
			shardCount = streamCfg.ShardCount
		}
	}
	lc.streamManager = NewStreamManager(shardCount, sendChannelSize)

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

			stream, err := client.StreamMessages(ctx)
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
		lc.setState(StateDisconnected)
		return firstErr
	}

	tlog.Info("all stream shards established", "count", shardCount)

	lc.setState(StateConnected)

	for i := 0; i < shardCount; i++ {
		lc.streamManager.shards[i].lc = lc
		go lc.streamManager.shards[i].startSendLoop()
		go lc.streamManager.shards[i].receiveMessages(lc, i)
	}

	// Flush buffered messages from disconnect period
	if lc.messageQueue != nil {
		go lc.messageQueue.Flush(lc)
	}

	if lc.reconnectManager == nil {
		lc.reconnectManager = NewReconnectManager(lc, lc.reconnectConfig)
		go lc.reconnectManager.Run()
	}

	lc.startHealthChecker()

	tlog.Info("successfully connected to logic server", "address", lc.address, "shards", shardCount, "isReconnect", isReconnect)

	return nil
}

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
				close(shard.sendCh)
			}
		}
	}
	lc.mu.Unlock()

	lc.setState(StateDisconnected)

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

func (s *StreamShard) receiveMessages(lc *LogicClient, shardIdx int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "receiveMessages panic recovered: %v\n", r)
		}
	}()

	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()

	if stream == nil {
		// 分片已被 sendLoop 隔离（stream 断开）：必须触发重连，否则该分片永久失效
		if !lc.closing {
			s.markShardBroken()
		}
		return
	}

	// 每个 shard 维护一个 writeCoalescer，跨多个 RouteBatch 累积反向推送数据。
	// flush 时每个连接只调用一次 SendMulti（= 一次 gnet AsyncWrite = 一次 event-loop channel send），
	// 将 channel send 次数从 ~250/batch 降至 ~M/flush（M=不同连接数），减少 runtime lock 竞争。
	var wc *writeCoalescer
	if lc.gateway != nil {
		wc = newWriteCoalescer(lc.gateway.GetConnectionManager())
	}

	// 退出时 flush 残留数据
	defer func() {
		if wc != nil && wc.count > 0 {
			pushed := wc.flush()
			if pushed > 0 && lc.gateway != nil {
				lc.gateway.AddPushedToClient(pushed)
			}
		}
	}()

	for {
		select {
		case <-lc.closed:
			return
		case <-s.ctx.Done():
			return
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			lc.mu.RLock()
			closing := lc.closing
			lc.mu.RUnlock()

			if closing {
				return
			}

			tlog.Warn("shard receive error, triggering reconnect", "shard", shardIdx, "error", err)
			s.markShardBroken()
			return
		}

		// 批量消息：解包逐条分发（logic->sgate 反向链路优化）
		// 两种格式：
		//   single-conn (msg.ConnectionId 非空): Data = [4字节 payloadLen][payload] 重复
		//     → 只需一次 GetConnection，所有 payload 发送到同一连接
		//   multi-conn (msg.ConnectionId 为空): Data = [2字节 connIDLen][connID][4字节 payloadLen][payload] 重复
		//     → 每条消息单独查找连接
		if lc.gateway != nil && msg.Route == protobuf.RouteBatch {
			data := msg.Data

			if msg.ConnectionId != "" {
				// single-conn 快速路径：data 已是 [4字节 len][payload] 格式，直接追加到 coalescer
				if !wc.addSingle(msg.ConnectionId, data, int(msg.Cmd)) {
					lc.gateway.AddPushDroppedNoConn(int64(msg.Cmd))
				}
			} else {
				// multi-conn 路径：逐条解析 connID，加入 coalescer（不再逐条 flush）
				var dropped int64
				for len(data) >= 6 {
					connIDLen := int(binary.BigEndian.Uint16(data[:2]))
					if connIDLen == 0 || len(data) < 2+connIDLen+4 {
						break
					}
					connID := string(data[2 : 2+connIDLen])
					payloadLen := int(binary.BigEndian.Uint32(data[2+connIDLen : 6+connIDLen]))
					if len(data) < 6+connIDLen+payloadLen {
						break
					}
					payload := data[6+connIDLen : 6+connIDLen+payloadLen]
					data = data[6+connIDLen+payloadLen:]

					if !wc.addMulti(connID, payload) {
						dropped++
					}
				}
				if dropped > 0 {
					lc.gateway.AddPushDroppedNoConn(dropped)
				}
			}

			// 累积到阈值或时间到达时 flush，减少 AsyncWrite 调用
			if wc.shouldFlush() {
				pushed := wc.flush()
				if pushed > 0 {
					lc.gateway.AddPushedToClient(pushed)
				}
			}
			continue
		}

		// 非 RouteBatch 消息：先 flush 累积数据，保持消息顺序
		if wc.count > 0 {
			pushed := wc.flush()
			if pushed > 0 {
				lc.gateway.AddPushedToClient(pushed)
			}
		}
		lc.handleReceivedMessage(msg)
	}
}

// handleReceivedMessage 处理单条来自 logic 的消息（正向转发或 server.* 路由）
func (lc *LogicClient) handleReceivedMessage(msg *protobuf.Message) {
	if lc.gateway == nil {
		return
	}
	// 快速路径：非 server.* 路由直接序列化转发，避免遍历所有 RouteServer* 分支
	route := msg.Route
	if len(route) < 7 || route[:7] != "server." {
		if msg.ConnectionId == "" {
			tlog.Warn("received message with empty ConnectionId", "route", route)
			return
		}
		conn := lc.gateway.GetConnectionManager().GetConnection(msg.ConnectionId)
		if conn == nil {
			lc.gateway.AddPushDroppedNoConn(1)
			return
		}
		// 注意: 不能使用 pooled buffer，因为 gnet Writev 可能异步传递 slice 引用
		responseData, err := proto.Marshal(msg)
		if err == nil {
			conn.Send(responseData)
			lc.gateway.AddPushedToClient(1)
		}
		return
	}

	// server.* 路由：部分指令不需要 ConnectionId（broadcast）
	if route == protobuf.RouteServerBroadcast {
		pushMsg := &protobuf.Message{
			Route:   "broadcast",
			Payload: msg.Payload,
		}
		lc.gateway.GetConnectionManager().Broadcast(pushMsg)
		return
	}

	if route == protobuf.RouteServerSendToUser {
		userUUID := msg.Payload["userUUID"]
		if userUUID != "" {
			responseData, _ := proto.Marshal(&protobuf.Message{
				Route:   msg.Payload["route"],
				Payload: msg.Payload,
			})
			if responseData != nil {
				lc.gateway.GetConnectionManager().SendToUser(userUUID, responseData)
			}
		}
		return
	}

	// 以下 server.* 指令需要 ConnectionId
	if msg.ConnectionId == "" {
		tlog.Warn("server.* message requires ConnectionId", "route", route)
		return
	}
	conn := lc.gateway.GetConnectionManager().GetConnection(msg.ConnectionId)
	if conn == nil {
		return
	}

	if route == protobuf.RouteServerKick {
		reason := ""
		if msg.Payload != nil {
			reason = msg.Payload["reason"]
		}
		responseData, _ := proto.Marshal(msg)
		conn.Send(responseData)
		tlog.Info("kicking connection by logic server", "connectionID", msg.ConnectionId, "reason", reason)
		lc.gateway.GetConnectionManager().RemoveConnection(msg.ConnectionId)
		if conn.Conn != nil {
			conn.Conn.Close()
		}
	} else if route == protobuf.RouteServerJoinGroup {
		groupID := msg.Payload["groupID"]
		connID := msg.ConnectionId
		if groupID != "" && connID != "" {
			conn := lc.gateway.GetConnectionManager().GetConnection(connID)
			if conn != nil {
				serverID := conn.ServerID
				userUUID := conn.UserUUID
				lc.gateway.GetConnectionManager().AddUserToGroup(groupID, serverID, userUUID)
				tlog.Debug("connection joined group by logic server", "connectionID", connID, "groupID", groupID, "serverID", serverID, "userUUID", userUUID)
			}
		}
		conn := lc.gateway.GetConnectionManager().GetConnection(connID)
		if conn != nil {
			responseData, _ := proto.Marshal(msg)
			conn.Send(responseData)
		}
	} else if route == protobuf.RouteServerLeaveGroup {
		groupID := msg.Payload["groupID"]
		connID := msg.ConnectionId
		if groupID != "" && connID != "" {
			conn := lc.gateway.GetConnectionManager().GetConnection(connID)
			if conn != nil {
				serverID := conn.ServerID
				userUUID := conn.UserUUID
				lc.gateway.GetConnectionManager().RemoveUserFromGroup(groupID, serverID, userUUID)
			}
		}
	} else if route == protobuf.RouteServerJoinGroupByUser {
		groupID := msg.Payload["groupID"]
		serverID := msg.Payload["serverID"]
		userUUID := msg.Payload["userUUID"]
		if groupID != "" && serverID != "" && userUUID != "" {
			lc.gateway.GetConnectionManager().AddUserToGroup(groupID, serverID, userUUID)
			tlog.Debug("user joined group by user key", "groupID", groupID, "serverID", serverID, "userUUID", userUUID)
		}
	} else if route == protobuf.RouteServerLeaveGroupByUser {
		groupID := msg.Payload["groupID"]
		serverID := msg.Payload["serverID"]
		userUUID := msg.Payload["userUUID"]
		if groupID != "" && serverID != "" && userUUID != "" {
			lc.gateway.GetConnectionManager().RemoveUserFromGroup(groupID, serverID, userUUID)
			tlog.Debug("user left group by user key", "groupID", groupID, "serverID", serverID, "userUUID", userUUID)
		}
	} else if route == protobuf.RouteServerCreateGroup {
		groupID := msg.Payload["groupID"]
		groupName := msg.Payload["groupName"]
		if groupID != "" {
			lc.gateway.GetConnectionManager().CreateGroup(groupID, groupName)
			tlog.Debug("group created by logic server", "groupID", groupID, "groupName", groupName)
		}
	} else if route == protobuf.RouteServerDeleteGroup {
		groupID := msg.Payload["groupID"]
		if groupID != "" {
			lc.gateway.GetConnectionManager().DeleteGroup(groupID)
			tlog.Debug("group deleted by logic server", "groupID", groupID)
		}
	} else if route == protobuf.RouteServerSendToGroup {
		groupID := msg.Payload["groupID"]
		originalRoute := msg.Payload["route"]
		if groupID != "" {
			sendMsg := &protobuf.Message{
				Route:   originalRoute,
				Payload: msg.Payload,
			}
			lc.gateway.GetConnectionManager().SendToGroup(groupID, sendMsg)
			tlog.Debug("message sent to group", "groupID", groupID, "route", originalRoute)
		}
	} else if route == protobuf.RouteServerGetGroupInfo {
		groupID := msg.Payload["groupID"]
		if groupID != "" {
			memberCount := lc.gateway.GetConnectionManager().GetGroupMemberCount(groupID)
			groupName := lc.gateway.GetConnectionManager().GetGroupName(groupID)
			users := lc.gateway.GetConnectionManager().GetGroupUsers(groupID)
			tlog.Debug("group info requested", "groupID", groupID, "groupName", groupName, "memberCount", memberCount, "users", users)
		}
	} else {
		responseData, err := proto.Marshal(msg)
		if err == nil {
			conn.Send(responseData)
		}
	}
}

func (lc *LogicClient) handleDisconnection() {
	lc.mu.RLock()
	closing := lc.closing
	state := lc.getState()
	lc.mu.RUnlock()

	if closing {
		return
	}

	if state == StateConnecting || state == StateReconnecting {
		return
	}

	lc.setState(StateDisconnected)

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

func (lc *LogicClient) SendMessage(msg *protobuf.Message) error {
	// Fast path: check state atomically without lock
	if lc.getState() != StateConnected {
		lc.mu.RLock()
		closing := lc.closing
		lc.mu.RUnlock()
		if closing {
			return ErrConnectionClosing
		}
		if lc.messageQueue != nil {
			lc.messageQueue.Enqueue(msg)
		}
		return ErrNotConnected
	}

	shard := lc.streamManager.GetShard(msg.ConnectionId)
	err := shard.SendMessage(msg)
	if err != nil {
		if lc.messageQueue != nil {
			lc.messageQueue.Enqueue(msg)
		}
		return err
	}

	return nil
}

func (lc *LogicClient) SendMessageDirect(msg *protobuf.Message) error {
	lc.mu.RLock()
	state := lc.getState()
	closing := lc.closing
	lc.mu.RUnlock()

	if closing {
		return ErrConnectionClosing
	}

	if state != StateConnected {
		return ErrNotConnected
	}

	shard := lc.streamManager.GetShard(msg.ConnectionId)
	return shard.SendMessage(msg)
}

func (lc *LogicClient) IsConnected() bool {
	return lc.getState() == StateConnected
}

func (lc *LogicClient) GetState() LogicConnectionState {
	return lc.getState()
}

type HealthChecker struct {
	lc          *LogicClient
	interval    time.Duration
	timeout     time.Duration
	maxFailures int
	failCount   int
	enabled     bool
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

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

func (hc *HealthChecker) Start() {
	hc.wg.Add(1)
	go hc.checkLoop()
}

func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	hc.wg.Wait()
}

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

func (hc *HealthChecker) doCheck() {
	hc.lc.mu.RLock()
	state := hc.lc.getState()
	closing := hc.lc.closing
	hc.lc.mu.RUnlock()

	if closing || state != StateConnected {
		return
	}

	// enabled=false 时跳过主动健康检查（可选关闭）。
	if !hc.enabled {
		return
	}

	pingMsg := &protobuf.Message{
		Route:   protobuf.RoutePing,
		Payload: map[string]string{"type": "health_check"},
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

func (lc *LogicClient) startHealthChecker() {
	if lc.healthChecker != nil {
		lc.healthChecker.Stop()
	}
	lc.healthChecker = NewHealthChecker(lc, lc.healthCheckConfig)
	lc.healthChecker.Start()
}

type ReconnectManager struct {
	lc           *LogicClient
	config       ReconnectConfig
	stopCh       chan struct{}
	doneCh       chan struct{}
	disconnectCh chan struct{}
}

func NewReconnectManager(lc *LogicClient, config ReconnectConfig) *ReconnectManager {
	return &ReconnectManager{
		lc:           lc,
		config:       config,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		disconnectCh: make(chan struct{}, 1),
	}
}

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

func (rm *ReconnectManager) Stop() {
	close(rm.stopCh)
	<-rm.doneCh
}

func (rm *ReconnectManager) NotifyDisconnection() {
	select {
	case rm.disconnectCh <- struct{}{}:
	default:
	}
}

func (rm *ReconnectManager) doReconnect() {
	interval := rm.config.InitialInterval
	attempt := 0

	for {
		select {
		case <-rm.stopCh:
			return
		default:
		}

		if rm.config.MaxAttempts > 0 && attempt >= rm.config.MaxAttempts {
			tlog.Error("max reconnect attempts reached", "maxAttempts", rm.config.MaxAttempts)
			return
		}

		attempt++
		tlog.Info("attempting reconnect", "attempt", attempt, "interval", interval)

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

type StreamMessageQueue struct {
	queue   []*protobuf.Message
	mu      sync.Mutex
	cond    *sync.Cond
	maxSize int
}

func NewStreamMessageQueue() *StreamMessageQueue {
	mq := &StreamMessageQueue{
		queue:   make([]*protobuf.Message, 0),
		maxSize: 100000,
	}
	mq.cond = sync.NewCond(&mq.mu)
	return mq
}

func (mq *StreamMessageQueue) Enqueue(msg *protobuf.Message) {
	mq.mu.Lock()
	if len(mq.queue) >= mq.maxSize {
		mq.queue = mq.queue[1:]
	}
	mq.queue = append(mq.queue, msg)
	mq.cond.Signal()
	mq.mu.Unlock()
}

func (mq *StreamMessageQueue) Dequeue() (*protobuf.Message, bool) {
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

		if state == StateConnected {
			err := lc.SendMessageDirect(msg)
			if err != nil {
				mq.Enqueue(msg)
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			mq.Enqueue(msg)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

type GatewayInterface interface {
	GetConnectionManager() *ConnectionManager
	GetGRPCConfig() config.GRPCConfig
	GetStreamConfig() config.StreamConfig
	AddPushedToClient(n int64)
	AddPushDroppedNoConn(n int64)
}

type GRPCServer struct {
	protobuf.UnimplementedGatewayServiceServer
	gateway GatewayInterface
	mu      sync.Mutex
}

func NewGRPCServer(gateway GatewayInterface) *GRPCServer {
	return &GRPCServer{
		gateway: gateway,
	}
}

func (s *GRPCServer) StreamMessages(stream protobuf.GatewayService_StreamMessagesServer) error {
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
			if protoMsg, ok := response.(*protobuf.Message); ok {
				stream.Send(protoMsg)
			} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
				responseMsg := &protobuf.Message{
					Route: protobuf.RouteError,
					Payload: map[string]string{
						"message": errorMsg.Error.Message,
						"code":    errorMsg.Error.Code,
						"details": errorMsg.Error.Details,
					},
				}
				stream.Send(responseMsg)
			}
		}, ctx)
	}
}

func (s *GRPCServer) SendMessage(ctx context.Context, msg *protobuf.Message) (*protobuf.Message, error) {
	connectionID := generateConnectionID()

	grpcCtx := map[string]interface{}{
		"connection_id": connectionID,
		"context":       ctx,
	}

	var response *protobuf.Message
	var wg sync.WaitGroup
	wg.Add(1)

	s.handleGRPCMessage(connectionID, msg, func(resp interface{}) {
		defer wg.Done()
		if protoMsg, ok := resp.(*protobuf.Message); ok {
			response = protoMsg
		} else if errorMsg, ok := resp.(*protobuf.ErrorResponse); ok {
			response = &protobuf.Message{
				Route: protobuf.RouteError,
				Payload: map[string]string{
					"message": errorMsg.Error.Message,
					"code":    errorMsg.Error.Code,
					"details": errorMsg.Error.Details,
				},
			}
		}
	}, grpcCtx)

	wg.Wait()
	return response, nil
}

func (s *GRPCServer) handleGRPCMessage(connectionID string, msg *protobuf.Message, callback func(interface{}), ctx map[string]interface{}) {
	if msg.Route == "" {
		callback(NewErrorMessage("error", "Missing route", "", ""))
		return
	}
	callback(NewErrorMessage("error", "Gateway does not handle routes locally, forward to logic server", "", ""))
}

func StartGRPCServer(gateway GatewayInterface, port string, maxMsgSize int, windowSize int) (*grpc.Server, error) {
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
	protobuf.RegisterGatewayServiceServer(server, NewGRPCServer(gateway))

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

type LogicClientPool struct {
	clients   map[string]*LogicClient
	ordered   []string // deterministic round-robin: ordered list of service IDs
	mu        sync.RWMutex
	gateway   GatewayInterface
	discovery *nacos.Discovery
	balancer  *cluster.Balancer
	stopCh    chan struct{}
	wg        sync.WaitGroup
	rrIndex   uint64
	// fastClient caches the single connected client for lock-free fast path.
	// Updated atomically when clients are added/removed. Nil when 0 or >1 clients.
	fastClient atomic.Pointer[LogicClient]
}

func NewLogicClientPool(gateway GatewayInterface) *LogicClientPool {
	return &LogicClientPool{
		clients: make(map[string]*LogicClient),
		gateway: gateway,
		stopCh:  make(chan struct{}),
	}
}

// updateFastClient must be called while holding pool.mu.
// Sets fastClient to the single client when exactly 1 client is connected, nil otherwise.
func (pool *LogicClientPool) updateFastClient() {
	if len(pool.clients) == 1 {
		for _, c := range pool.clients {
			pool.fastClient.Store(c)
			return
		}
	}
	pool.fastClient.Store(nil)
}

func (pool *LogicClientPool) SetDiscovery(discovery *nacos.Discovery) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *LogicClientPool) SetBalancer(balancer *cluster.Balancer) {
	pool.balancer = balancer
}

func (pool *LogicClientPool) handleServiceChange(event nacos.ServiceEvent) {
	switch event.Type {
	case nacos.EventRegister:
		pool.handleServiceRegister(event)
	case nacos.EventDeregister:
		pool.handleServiceDeregister(event)
	}
}

func (pool *LogicClientPool) handleServiceRegister(event nacos.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.Service.ServiceID]
	pool.mu.RUnlock()

	// 已存在且连接正常，跳过
	if exists && existing != nil && existing.IsConnected() {
		return
	}

	// 已存在但连接已断开，先清理旧 client
	if exists && existing != nil && !existing.IsConnected() {
		pool.mu.Lock()
		delete(pool.clients, event.Service.ServiceID)
		pool.updateFastClient()
		pool.mu.Unlock()
		go existing.Close()
	}

	client := NewLogicClient(pool.gateway)
	client.shardCount = runtime.NumCPU() * 8

	go func() {
		tlog.Info("connecting to discovered logic service",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
		)
		if err := client.Connect(event.Service.Address); err != nil {
			tlog.Error("failed to connect to discovered logic service",
				"serviceID", event.Service.ServiceID,
				"address", event.Service.Address,
				"error", err,
			)
			return
		}
		tlog.Info("connected to discovered logic service",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
		)
	}()

	pool.mu.Lock()
	pool.clients[event.Service.ServiceID] = client
	if !containsString(pool.ordered, event.Service.ServiceID) {
		pool.ordered = append(pool.ordered, event.Service.ServiceID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()

	if pool.balancer != nil {
		pool.balancer.AddNode(event.Service.ServiceID, event.Service.Address, 1)
	}

	tlog.Info("logic client added to pool",
		"serviceID", event.Service.ServiceID,
		"address", event.Service.Address,
		"totalClients", pool.ClientCount(),
	)
}

func (pool *LogicClientPool) handleServiceDeregister(event nacos.ServiceEvent) {
	// 高负载下 Nacos 心跳可能超时导致实例被摘除，但 gRPC 连接可能仍可用。
	// 不立即从 pool 删除和关闭连接，避免误判导致转发中断。
	// 让 HealthChecker 和 gRPC 流自身错误检测来处理真正的连接断开。
	pool.mu.RLock()
	client, exists := pool.clients[event.Service.ServiceID]
	pool.mu.RUnlock()

	if exists && client != nil {
		if !client.IsConnected() {
			// gRPC 连接已断开，安全清理
			pool.mu.Lock()
			delete(pool.clients, event.Service.ServiceID)
			pool.ordered = removeString(pool.ordered, event.Service.ServiceID)
			pool.updateFastClient()
			pool.mu.Unlock()
			if pool.balancer != nil {
				pool.balancer.RemoveNode(event.Service.ServiceID)
			}
			go client.Close()
			tlog.Warn("logic service offline and connection already disconnected, cleaning up",
				"serviceID", event.Service.ServiceID,
				"address", event.Service.Address,
			)
		} else {
			// gRPC 连接仍存活，保留连接，等服务重新注册或 HealthChecker 检测到断开
			tlog.Warn("logic service deregistered from nacos, keeping gRPC connection (still connected)",
				"serviceID", event.Service.ServiceID,
				"address", event.Service.Address,
			)
		}
	}

	tlog.Warn("logic client deregister event processed",
		"serviceID", event.Service.ServiceID,
		"address", event.Service.Address,
		"totalClients", pool.ClientCount(),
	)
}

func (pool *LogicClientPool) SendMessage(msg *protobuf.Message) error {
	// Fast path: single client, no lock needed
	if c := pool.fastClient.Load(); c != nil {
		return c.SendMessage(msg)
	}
	return pool.RoundRobinSendMessage(msg)
}

func (pool *LogicClientPool) SendMessageByServiceID(serviceID string, msg *protobuf.Message) error {
	pool.mu.RLock()
	client, ok := pool.clients[serviceID]
	pool.mu.RUnlock()

	if !ok {
		return fmt.Errorf("service %s not found in pool", serviceID)
	}
	return client.SendMessage(msg)
}

func (pool *LogicClientPool) RoundRobinSendMessage(msg *protobuf.Message) error {
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

func (pool *LogicClientPool) ClientCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return len(pool.clients)
}

func (pool *LogicClientPool) ConnectedCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	count := 0
	for _, client := range pool.clients {
		if client.IsConnected() {
			count++
		}
	}
	return count
}

func (pool *LogicClientPool) GetClientStatus() []map[string]interface{} {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	status := make([]map[string]interface{}, 0, len(pool.clients))
	for id, client := range pool.clients {
		status = append(status, map[string]interface{}{
			"serviceID": id,
			"address":   client.address,
			"state":     client.getState().String(),
			"connected": client.IsConnected(),
		})
	}
	return status
}

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

func (pool *LogicClientPool) AddStaticService(address string) {
	serviceID := "static_" + address
	pool.mu.RLock()
	_, exists := pool.clients[serviceID]
	pool.mu.RUnlock()

	if exists {
		return
	}

	client := NewLogicClient(pool.gateway)
	client.shardCount = runtime.NumCPU()

	pool.mu.Lock()
	pool.clients[serviceID] = client
	if !containsString(pool.ordered, serviceID) {
		pool.ordered = append(pool.ordered, serviceID)
	}
	pool.updateFastClient()
	pool.mu.Unlock()

	go func() {
		tlog.Info("connecting to static logic service", "address", address)
		if err := client.Connect(address); err != nil {
			tlog.Error("failed to connect to static logic service", "address", address, "error", err)
		}
	}()
}

func (pool *LogicClientPool) IsConnected() bool {
	// Fast path: single client, no lock needed
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

func splitRoutes(s string) []string {
	var result []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			result = append(result, r)
		}
	}
	return result
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}
