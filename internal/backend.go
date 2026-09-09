package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/gateway"
	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type LogicConnectionState int32

const (
	LogicStateDisconnected LogicConnectionState = iota
	LogicStateConnecting
	LogicStateConnected
	LogicStateReconnecting
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

var (
	ErrNotConnected      = errors.New("not connected to logic server")
	ErrConnectionClosing = errors.New("connection is closing")
	ErrQueueFull         = errors.New("send queue full")
	ErrSendTimeout       = errors.New("send timeout")
	ErrBackpressure      = errors.New("backpressure activated")
)

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
	stream      protoGw.GatewayStream_OnDataClient
	mu          sync.Mutex
	sendCh      chan *protoGw.StreamData
	stopCh      chan struct{}
	stopOnce    sync.Once
	ctx         context.Context
	cancel      context.CancelFunc
	index       int
	lc          *LogicClient
	closed      atomic.Bool
	sendTimeout time.Duration
}

type StreamManager struct {
	shards      []*StreamShard
	sendTimeout time.Duration
}

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
	batch := make([]*protoGw.StreamData, 0, maxBatchCount)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		var msg *protoGw.StreamData
		select {
		case <-s.stopCh:
			return
		case msg = <-s.sendCh:
			if msg == nil {
				continue
			}
		case <-ticker.C:
			// Periodic flush for low-throughput scenarios
			continue
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

		// Get stream once for the entire batch
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()

		if stream == nil {
			continue
		}

		// Send entire batch with single stream reference
		for _, message := range batch {
			if err := safeStreamSend(stream, message); err != nil {
				tlog.Warn("shard send error, isolating shard", "shard", s.index, "error", err)
				s.markShardBroken()
				break
			}
		}
	}
}

// safeStreamSend wraps stream.Send() to recover from panics caused by
// concurrent close operations on the gRPC stream.
func safeStreamSend(stream protoGw.GatewayStream_OnDataClient, msg *protoGw.StreamData) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("stream send panic: %v", r)
		}
	}()
	return stream.Send(msg)
}

func (s *StreamShard) SendMessage(msg *protoGw.StreamData) (err error) {
	// Fast path: check closed flag atomically, skip defer/recover overhead
	if s.closed.Load() {
		return ErrNotConnected
	}

	// Try non-blocking send first (most common case)
	select {
	case s.sendCh <- msg:
		return nil
	default:
		// Channel full, try with timeout
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

func (s *StreamShard) stop() {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		close(s.stopCh)
	})
}

type LogicClient struct {
	client            protoGw.GatewayStreamClient
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
	gateway           GatewayInterface
	closing           bool
	closed            chan struct{}
	shardCount        int
	serverID          string
}

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
	tlog.Info("logic connection state changed",
		"oldState", oldState.String(),
		"newState", newState.String(),
	)
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

	// Re-check closing after blocking dial — Close() may have been called during dial
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

	// Shut down old stream shards: nil out the stream reference and close send channels
	// so that startSendLoop goroutines stop using the old (now-closed) streams.
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

	// Flush buffered messages from disconnect period
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
		if !lc.closing {
			s.markShardBroken()
		}
		return
	}

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

		if lc.gateway != nil && msg.SessionId != "" {
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

func (lc *LogicClient) SendMessage(msg *protoGw.StreamData) error {
	// Fast path: check state atomically without lock
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

func (lc *LogicClient) startHealthChecker() {
	if lc.healthChecker != nil {
		lc.healthChecker.Stop()
	}
	lc.healthChecker = NewHealthChecker(lc, lc.healthCheckConfig)
	lc.healthChecker.Start()
}

type ReconnectManager struct {
	lc            *LogicClient
	config        ReconnectConfig
	stopCh        chan struct{}
	doneCh        chan struct{}
	disconnectCh  chan struct{}
	lookupAddress func(serverID string) string // optional: query etcd for replacement address
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

func (rm *ReconnectManager) SetLookupAddress(fn func(serverID string) string) {
	rm.lookupAddress = fn
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

			// Try etcd discovery for a replacement node
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

type StreamMessageQueue struct {
	queue                 []*protoGw.StreamData
	mu                    sync.Mutex
	cond                  *sync.Cond
	maxSize               int
	policy                config.QueuePolicy
	blockTimeout          time.Duration
	backpressureThreshold float64
}

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

	default: // config.QueuePolicyDrop or unknown
		if len(mq.queue) >= mq.maxSize {
			mq.queue = mq.queue[1:]
		}
		mq.queue = append(mq.queue, msg)
		mq.cond.Signal()
		mq.mu.Unlock()
		return nil
	}
}

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

type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer
	gateway GatewayInterface
	mu      sync.Mutex
}

func NewGRPCServer(gateway GatewayInterface) *GRPCServer {
	return &GRPCServer{
		gateway: gateway,
	}
}

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

func (s *GRPCServer) BroadcastAll(_ context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	var firstErr error
	s.gateway.GetConnectionManager().connections.Range(func(_, value any) bool {
		conn := value.(*Connection)
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

func (s *GRPCServer) GetGroupInfo(_ context.Context, req *protoGw.GetGroupInfoReq) (*protoGw.GetGroupInfoAck, error) {
	cm := s.gateway.GetConnectionManager()
	return &protoGw.GetGroupInfoAck{
		GroupId:     req.GetGroupId(),
		MemberCount: int32(cm.GetGroupMemberCount(req.GetGroupId())),
		SessionIds:  cm.GetGroupSessions(req.GetGroupId()),
	}, nil
}

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

func encodePushMessage(cmd int32, data []byte) []byte {
	msg, _ := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: data})
	return msg
}

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

func (s *GRPCServer) handleGRPCMessage(connectionID string, msg *protoGw.StreamData, callback func(interface{}), ctx map[string]interface{}) {
	if msg.Cmd == 0 {
		callback(newErrorResponse("error", "Missing cmd", "", ""))
		return
	}
	callback(newErrorResponse("error", "Gateway does not handle commands locally, forward to logic server", "", ""))
}

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

type LogicClientPool struct {
	clients    map[string]*LogicClient
	ordered    []string // deterministic round-robin: ordered list of service IDs
	mu         sync.RWMutex
	gateway    GatewayInterface
	discovery  *etcd.Component
	balancer   *cluster.Balancer
	stopCh     chan struct{}
	wg         sync.WaitGroup
	rrIndex    uint64
	fastClient atomic.Pointer[LogicClient]
	addressMap map[string]string // serverID -> address (from etcd)
}

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

func (pool *LogicClientPool) GetClient(serverID string) LogicClientProvider {
	pool.mu.RLock()
	client := pool.clients[serverID]
	pool.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	return client
}

func NewLogicClientPool(gateway GatewayInterface) *LogicClientPool {
	return &LogicClientPool{
		clients:    make(map[string]*LogicClient),
		addressMap: make(map[string]string),
		gateway:    gateway,
		stopCh:     make(chan struct{}),
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

// LookupAddress returns the address for a given serverID from the etcd-maintained map.
func (pool *LogicClientPool) LookupAddress(serverID string) string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.addressMap[serverID]
}

func (pool *LogicClientPool) SetDiscovery(discovery *etcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *LogicClientPool) SetBalancer(balancer *cluster.Balancer) {
	pool.balancer = balancer
}

func (pool *LogicClientPool) handleServiceChange(event etcd.ServiceEvent) {
	switch event.Type {
	case etcd.EventRegister:
		pool.handleServiceRegister(event)
	case etcd.EventDeregister:
		pool.handleServiceDeregister(event)
	}
}

func (pool *LogicClientPool) handleServiceRegister(event etcd.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	// 已存在且连接正常，跳过
	if exists && existing != nil && existing.IsConnected() {
		return
	}

	// 已存在但连接已断开，先清理旧 client
	if exists && existing != nil && !existing.IsConnected() {
		pool.mu.Lock()
		delete(pool.clients, event.InstanceID)
		pool.updateFastClient()
		pool.mu.Unlock()
		go existing.Close()
	}

	client := NewLogicClient(pool.gateway)
	client.SetServerID(event.InstanceID)
	client.shardCount = runtime.NumCPU() * 8

	go func() {
		tlog.Info("connecting to discovered logic service",
			"serviceID", event.InstanceID,
			"address", event.Address,
		)
		if err := client.Connect(event.Address); err != nil {
			tlog.Error("failed to connect to discovered logic service",
				"serviceID", event.InstanceID,
				"address", event.Address,
				"error", err,
			)
			return
		}
		tlog.Info("connected to discovered logic service",
			"serviceID", event.InstanceID,
			"address", event.Address,
		)
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

func (pool *LogicClientPool) handleServiceDeregister(event etcd.ServiceEvent) {
	// A discovery lease may expire while an existing gRPC connection is still usable.
	// 不立即从 pool 删除和关闭连接，避免误判导致转发中断。
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

func (pool *LogicClientPool) SendMessage(msg *protoGw.StreamData) error {
	// Fast path: single client, no lock needed
	if c := pool.fastClient.Load(); c != nil {
		return c.SendMessage(msg)
	}
	return pool.RoundRobinSendMessage(msg)
}

// SendMessageTo sends a session-bound message to exactly one logic server.
// It never falls back to round-robin because that could cross server shards.
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

// ---------------------------------------------------------------------------
// GatewayClient / GatewayClientPool – gateway-to-gateway gRPC client pool
// ---------------------------------------------------------------------------

// GatewayClient wraps a single gRPC connection to another gateway instance.
type GatewayClient struct {
	client   protoGw.GatewayClient
	conn     *grpc.ClientConn
	address  string
	serverID string
	mu       sync.RWMutex
	closing  bool
}

func NewGatewayClient(serverID, address string) *GatewayClient {
	return &GatewayClient{
		address:  address,
		serverID: serverID,
	}
}

func (gc *GatewayClient) Connect() error {
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
		return fmt.Errorf("client %s is closing", gc.serverID)
	}
	gc.conn = conn
	gc.client = protoGw.NewGatewayClient(conn)
	gc.mu.Unlock()

	tlog.Info("gateway client created", "serverID", gc.serverID, "address", gc.address)
	return nil
}

func (gc *GatewayClient) IsConnected() bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.conn != nil && !gc.closing && gc.conn.GetState() != connectivity.Shutdown
}

func (gc *GatewayClient) Client() protoGw.GatewayClient {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.client
}

func (gc *GatewayClient) Close() {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.closing {
		return
	}
	gc.closing = true
	if gc.conn != nil {
		gc.conn.Close()
	}
}

// GatewayClientPool manages gRPC clients to other gateway instances discovered
// via etcd. The pool mirrors the structure of LogicClientPool but is simpler
// because gateway-to-gateway communication uses unary RPCs (no streams).
type GatewayClientPool struct {
	clients    map[string]*GatewayClient
	mu         sync.RWMutex
	discovery  *etcd.Component
	addressMap map[string]string // serverID → address (from etcd)
	selfID     string            // this gateway's instance ID (excluded from pool)
}

func NewGatewayClientPool(gateway GatewayInterface) *GatewayClientPool {
	return &GatewayClientPool{
		clients:    make(map[string]*GatewayClient),
		addressMap: make(map[string]string),
		selfID:     gateway.GetServerID(),
	}
}

func (pool *GatewayClientPool) LoadEvents(events []etcd.ServiceEvent) {
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

func (pool *GatewayClientPool) SetDiscovery(discovery *etcd.Component) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *GatewayClientPool) handleServiceChange(event etcd.ServiceEvent) {
	// The dedicated discovery component watches Gateway:{zone}. Skip self.
	if event.InstanceID == pool.selfID {
		return
	}
	switch event.Type {
	case etcd.EventRegister:
		pool.handleRegister(event)
	case etcd.EventDeregister:
		pool.handleDeregister(event)
	}
}

func (pool *GatewayClientPool) handleRegister(event etcd.ServiceEvent) {
	pool.mu.RLock()
	existing, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && existing != nil && existing.IsConnected() && existing.address == event.Address {
		return
	}

	// Clean up stale entry
	if exists && existing != nil && !existing.IsConnected() {
		pool.mu.Lock()
		delete(pool.clients, event.InstanceID)
		delete(pool.addressMap, event.InstanceID)
		pool.mu.Unlock()
		go existing.Close()
	}

	client := NewGatewayClient(event.InstanceID, event.Address)
	go func() {
		tlog.Info("connecting to discovered gateway", "serverID", event.InstanceID, "address", event.Address)
		if err := client.Connect(); err != nil {
			tlog.Error("failed to connect to discovered gateway",
				"serverID", event.InstanceID, "address", event.Address, "error", err)
			return
		}
		tlog.Info("gateway client ready for discovered gateway", "serverID", event.InstanceID, "address", event.Address)
	}()

	pool.mu.Lock()
	pool.clients[event.InstanceID] = client
	pool.addressMap[event.InstanceID] = event.Address
	pool.mu.Unlock()

	tlog.Info("gateway client added to pool",
		"serverID", event.InstanceID, "address", event.Address, "totalClients", pool.ClientCount())
}

func (pool *GatewayClientPool) handleDeregister(event etcd.ServiceEvent) {
	pool.mu.RLock()
	client, exists := pool.clients[event.InstanceID]
	pool.mu.RUnlock()

	if exists && client != nil {
		if !client.IsConnected() {
			pool.mu.Lock()
			delete(pool.clients, event.InstanceID)
			delete(pool.addressMap, event.InstanceID)
			pool.mu.Unlock()
			go client.Close()
			tlog.Warn("gateway offline and connection already disconnected, cleaned up",
				"serverID", event.InstanceID, "address", event.Address)
		} else {
			tlog.Warn("gateway deregistered from etcd, keeping gRPC connection (still connected)",
				"serverID", event.InstanceID, "address", event.Address)
		}
	}

	tlog.Warn("gateway client deregister event processed",
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
