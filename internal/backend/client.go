package backend

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/gatewayutil"
	"github.com/streasure/util/tlog"
)

// LogicClient 逻辑服连接：sgate 不再主动拨号，logic 主动拨入后
// 由 LogicClientPool.Attach 附着到本对象。连接可用性完全由
// 分片接入计数驱动（attached == totalShards 即 Connected），
// 重连责任在 logic 拨出侧。
type LogicClient struct {
	mu            sync.RWMutex                  // 保护状态和连接的读写锁
	state         atomic.Int32                  // 连接状态（原子操作）
	address       string                        // 逻辑服地址（握手元数据，诊断用）
	streamManager atomic.Pointer[StreamManager] // 流分片管理器（原子替换，热路径无锁读取）
	messageQueue  *StreamMessageQueue           // 断线期间的消息缓存队列
	gateway       GatewayInterface              // 网关接口引用
	closing       bool                          // 是否正在关闭
	closed        chan struct{}                 // 关闭完成信号
	totalShards   int                           // 分片总数（握手时确定，创建后只读）
	attached      atomic.Int32                  // 当前已接入的分片数
	serverID      string                        // 逻辑服标识
}

// NewLogicClient 创建逻辑服客户端实例
func NewLogicClient(gateway GatewayInterface) *LogicClient {
	queuePolicy := config.StreamQueueConfig{}
	var sendTimeout time.Duration
	if gateway != nil {
		queuePolicy = gateway.GetStreamConfig().QueuePolicy
		if d := gatewayutil.ParseDurationDefault(queuePolicy.SendTimeout, 0); d > 0 {
			sendTimeout = d
		}
	}
	lc := &LogicClient{
		gateway:      gateway,
		closing:      false,
		closed:       make(chan struct{}),
		messageQueue: NewStreamMessageQueue(queuePolicy),
	}
	lc.streamManager.Store(NewStreamManager(0, 0, sendTimeout))
	return lc
}

func (lc *LogicClient) SetServerID(serverID string) { lc.serverID = serverID }

// SetGateway 设置网关接口引用。
func (lc *LogicClient) SetGateway(gateway GatewayInterface) {
	lc.mu.Lock()
	lc.gateway = gateway
	lc.mu.Unlock()
}

// getState 原子获取连接状态
func (lc *LogicClient) getState() LogicConnectionState {
	return LogicConnectionState(lc.state.Load())
}

// setState 原子设置连接状态并通知状态变更
func (lc *LogicClient) setState(newState LogicConnectionState) {
	oldState := LogicConnectionState(lc.state.Load())
	if oldState == newState {
		return
	}
	lc.state.Store(int32(newState))
	lc.notifyStateChange(oldState, newState)
}

// notifyStateChange 记录连接状态变更日志
func (lc *LogicClient) notifyStateChange(oldState, newState LogicConnectionState) {
	tlog.Info(context.TODO(), "logic connection state changed oldState=%s newState=%s",
		oldState.String(),
		newState.String(),
	)
}

// initAccepted 构建接入（flip）模式的流分片管理器并启动各分片发送循环。
// 由 LogicClientPool.Attach 在创建客户端时调用一次；totalShards 此后只读。
func (lc *LogicClient) initAccepted(totalShards int) {
	sendChannelSize := 0
	var sendTimeout time.Duration
	if lc.gateway != nil {
		streamCfg := lc.gateway.GetStreamConfig()
		sendChannelSize = streamCfg.SendChannelSize
		if d := gatewayutil.ParseDurationDefault(streamCfg.QueuePolicy.SendTimeout, 0); d > 0 {
			sendTimeout = d
		}
	}
	sm := NewStreamManager(totalShards, sendChannelSize, sendTimeout)
	for i := range sm.shards {
		sm.shards[i].lc = lc
		go sm.shards[i].startSendLoop()
	}
	lc.totalShards = totalShards
	lc.streamManager.Store(sm)
}

// attachShard 将 logic 拨入的一条流附着到指定分片。
// 全部分片接入后置为 Connected 并冲刷断线期间缓存的消息。
func (lc *LogicClient) attachShard(idx int, stream dataStream) error {
	lc.mu.RLock()
	closing := lc.closing
	lc.mu.RUnlock()
	if closing {
		return ErrConnectionClosing
	}
	sm := lc.streamManager.Load()
	if sm == nil || idx < 0 || idx >= len(sm.shards) {
		return ErrNotConnected
	}
	sh := sm.shards[idx]

	sh.mu.Lock()
	prev := sh.stream
	sh.stream = stream
	sh.mu.Unlock()

	if prev == nil {
		lc.attached.Add(1)
	} else {
		// logic 重拨时旧流尚未报错：原地替换，计数不变。
		tlog.Warn(context.TODO(), "shard stream replaced by new dial shard=%d serverID=%s", idx, lc.serverID)
	}

	if int(lc.attached.Load()) >= lc.totalShards && lc.getState() != LogicStateConnected {
		lc.setState(LogicStateConnected)
		if lc.messageQueue != nil {
			go lc.messageQueue.Flush(lc)
		}
	}
	return nil
}

// detachShard 解除分片与指定流的绑定（幂等）。
// 仅当该流仍是分片当前绑定时生效；接入数低于总数即判定断开，
// 消息重新进入断线队列，等待 logic 重新拨入。
func (lc *LogicClient) detachShard(sh *StreamShard, stream dataStream) {
	sh.mu.Lock()
	if sh.stream != stream {
		sh.mu.Unlock()
		return
	}
	sh.stream = nil
	sh.mu.Unlock()

	lc.mu.RLock()
	closing := lc.closing
	lc.mu.RUnlock()
	if closing {
		return
	}

	n := lc.attached.Add(-1)
	if n < 0 {
		tlog.Error(context.TODO(), "detachShard: attached count went negative serverID=%s", lc.serverID)
		return
	}
	if int(n) < lc.totalShards && lc.getState() == LogicStateConnected {
		tlog.Warn(context.TODO(), "logic stream set incomplete, marking disconnected attached=%d total=%d serverID=%s",
			int(n), lc.totalShards, lc.serverID)
		lc.setState(LogicStateDisconnected)
	}
}

// Close 关闭逻辑服客户端，释放所有资源。
// 分片停止后，各 OnData 接收循环在下一次 Recv 报错时退出
// （gRPC server Stop 会强制关闭底层连接）。
func (lc *LogicClient) Close() {
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		return
	}
	lc.closing = true

	if oldSM := lc.streamManager.Load(); oldSM != nil {
		for i := range oldSM.shards {
			if shard := oldSM.shards[i]; shard != nil {
				shard.closed.Store(true)
				shard.stop()
			}
		}
	}
	lc.mu.Unlock()

	lc.setState(LogicStateDisconnected)

	// 唤醒阻塞在满队列上的 Enqueue（block 策略无超时，不唤醒将永久挂起）
	if lc.messageQueue != nil {
		lc.messageQueue.Close()
	}

	close(lc.closed)
	tlog.Info(context.TODO(), "closed logic server connection")
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
			return nil // 消息已入队，等重连后自动发送
		}
		return ErrNotConnected
	}

	sm := lc.streamManager.Load()
	if sm == nil {
		return ErrNotConnected
	}
	shard := sm.GetShard(msg.SessionId)
	err := shard.SendMessage(msg)
	if err != nil {
		if lc.messageQueue != nil {
			if qErr := lc.messageQueue.Enqueue(msg); qErr != nil {
				return qErr
			}
			// 消息已入队：所有权转移给队列，返回 nil 避免调用方重复归还（双 Put）。
			// 必须立即触发 Flush：状态仍为 Connected 时不会有状态跃迁，
			// 否则该消息要等完整断连-重连后才会发出（Flush 内有 CAS 幂等保护）。
			go lc.messageQueue.Flush(lc)
			return nil
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

	sm := lc.streamManager.Load()
	if sm == nil {
		return ErrNotConnected
	}
	return sm.GetShard(msg.SessionId).SendMessage(msg)
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
	stopOnce    sync.Once      // 保护 stopCh 只 close 一次
	stopCh      chan struct{}  // 停止信号
	wg          sync.WaitGroup // 等待检查循环退出
}

// NewHealthChecker 创建健康检查器实例
func NewHealthChecker(lc *LogicClient, config HealthCheckConfig) *HealthChecker {
	interval := config.Interval
	if interval <= 0 {
		interval = 10 * time.Second // 防 time.NewTicker(0) panic
	}
	return &HealthChecker{
		lc:          lc,
		interval:    interval,
		timeout:     config.Timeout,
		maxFailures: config.MaxFailures,
		enabled:     config.Enabled,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动健康检查循环
func (hc *HealthChecker) Start() {
	hc.wg.Go(hc.checkLoop)
}

// Stop 停止健康检查循环并等待退出（幂等）
func (hc *HealthChecker) Stop() {
	hc.stopOnce.Do(func() { close(hc.stopCh) })
	hc.wg.Wait()
}

// checkLoop 健康检查定时循环
func (hc *HealthChecker) checkLoop() {
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
		Cmd: int32(routes.CmdHeartbeatReq),
	}

	err := hc.lc.SendMessageDirect(pingMsg)
	if err != nil {
		hc.failCount++
		tlog.Warn(context.TODO(), "health check failed failCount=%d maxFailures=%d error=%v", hc.failCount, hc.maxFailures, err)
		if hc.failCount >= hc.maxFailures {
			tlog.Error(context.TODO(), "too many health check failures, marking disconnected failCount=%d", hc.failCount)
			hc.lc.setState(LogicStateDisconnected)
		}
	} else {
		hc.failCount = 0
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
	flushing              atomic.Bool           // 防止并发 Flush
	closed                bool                  // 已关闭：唤醒并拒绝阻塞入队
}

// NewStreamMessageQueue 创建消息队列
func NewStreamMessageQueue(cfg config.StreamQueueConfig) *StreamMessageQueue {
	maxSize := cfg.MaxSize
	if maxSize <= 0 {
		maxSize = 100000
	}
	blockTimeout := gatewayutil.ParseDurationDefault(cfg.BlockTimeout, 500*time.Millisecond)
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

// Close 关闭队列：唤醒所有等待空间的入队者，后续阻塞入队快速失败。
// 由 LogicClient.Close 调用，防止 block 策略 Enqueue 在关闭时永久挂起。
func (mq *StreamMessageQueue) Close() {
	mq.mu.Lock()
	mq.closed = true
	mq.mu.Unlock()
	mq.cond.Broadcast()
}

// Enqueue 将消息入队，根据策略选择阻塞/超时/背压/丢弃
func (mq *StreamMessageQueue) Enqueue(msg *protoGw.StreamData) error {
	mq.mu.Lock()
	switch mq.policy {
	case config.QueuePolicyBlock:
		for len(mq.queue) >= mq.maxSize {
			if mq.closed {
				mq.mu.Unlock()
				return ErrConnectionClosing
			}
			mq.cond.Wait()
		}
		mq.queue = append(mq.queue, msg)
		mq.mu.Unlock()
		return nil

	case config.QueuePolicyTimeout:
		deadline := time.Now().Add(mq.blockTimeout)
		for len(mq.queue) >= mq.maxSize {
			if mq.closed {
				mq.mu.Unlock()
				return ErrConnectionClosing
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				mq.mu.Unlock()
				return ErrQueueFull
			}
			// 用 timer + Broadcast 唤醒 Wait，避免 1ms busy-wait
			timer := time.AfterFunc(remaining, func() { mq.cond.Broadcast() })
			mq.cond.Wait()
			timer.Stop()
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
	mq.cond.Signal() // 通知等待入队的 goroutine 有空间了
	mq.mu.Unlock()
	return msg, true
}

// RequeueFront 将消息非阻塞放回队首（保持投递顺序）。
// 队列已满时丢弃当前最旧消息腾位，绝不阻塞——Flush 冲刷失败必须走此路径，
// 否则 Block 等策略的 Enqueue 会等待空间，而唯一能腾出空间的 Dequeue 正被
// 自身占用，形成死锁。
func (mq *StreamMessageQueue) RequeueFront(msg *protoGw.StreamData) {
	mq.mu.Lock()
	if len(mq.queue) >= mq.maxSize {
		mq.queue = mq.queue[1:] // 与 Drop/Backpressure 一致：丢最旧
	}
	mq.queue = append([]*protoGw.StreamData{msg}, mq.queue...)
	mq.cond.Signal()
	mq.mu.Unlock()
}

// Flush 冲刷队列中的消息，重连后调用以恢复转发。
// maxRetries 统计连续失败次数而非成功消息数，避免长队列在 100 条后停止冲刷。
func (mq *StreamMessageQueue) Flush(lc *LogicClient) {
	if !mq.flushing.CompareAndSwap(false, true) {
		return // 已有 Flush 在运行
	}
	defer mq.flushing.Store(false)

	const maxFailures = 100
	const retryInterval = 100 * time.Millisecond
	failures := 0

	for failures < maxFailures {
		msg, ok := mq.Dequeue()
		if !ok {
			return
		}

		lc.mu.RLock()
		state := lc.getState()
		closing := lc.closing
		lc.mu.RUnlock()

		if closing {
			PutStreamData(msg)
			return
		}

		if state == LogicStateConnected {
			if err := lc.SendMessageDirect(msg); err == nil {
				failures = 0
				continue
			}
		}
		// 发送失败：非阻塞放回队首保序重试，避免 Enqueue 等待空间造成自死锁。
		mq.RequeueFront(msg)
		failures++
		time.Sleep(retryInterval)
	}
	tlog.Warn(context.TODO(), "flush: aborted after consecutive failures policy=%v", mq.policy)
}
