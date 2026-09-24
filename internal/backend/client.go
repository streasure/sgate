package backend

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

type connGroup struct {
	conn   *grpc.ClientConn
	client protoGw.GatewayStreamClient
}

// LogicClient 逻辑服客户端，管理与单个逻辑服实例的连接和流通信
type LogicClient struct {
	mu                sync.RWMutex        // 保护状态和连接的读写锁
	state             atomic.Int32        // 连接状态（原子操作）
	address           string              // 逻辑服地址
	streamManager     *StreamManager      // 流分片管理器
	streamCtx         context.Context     // 流上下文
	streamCancel      context.CancelFunc  // 取消流上下文
	reconnectConfig   ReconnectConfig     // 重连配置
	healthCheckConfig HealthCheckConfig   // 健康检查配置
	healthChecker     *HealthChecker      // 健康检查器
	reconnectManager  *ReconnectManager   // 重连管理器
	messageQueue      *StreamMessageQueue // 断线期间的消息缓存队列
	gateway           GatewayInterface    // 网关接口引用
	closing           bool                // 是否正在关闭
	closed            chan struct{}       // 关闭完成信号
	shardCount        int                 // 分片数量
	connGroupCount    int                 // 独立 TCP 连接组数（默认 4）
	connGroups        []connGroup         // N 条独立 gRPC 连接
	serverID          string              // 逻辑服标识
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
		state:             atomic.Int32{},
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
		oldState = LogicConnectionState(lc.state.Load())
		lc.state.Store(int32(LogicStateReconnecting))
	} else {
		oldState = LogicConnectionState(lc.state.Load())
		lc.state.Store(int32(LogicStateConnecting))
	}

	// 关闭旧的连接组
	if len(lc.connGroups) > 0 {
		tlog.Info(context.TODO(), "doConnect closing old connGroups count=%d isReconnect=%v", len(lc.connGroups), isReconnect)
		for _, cg := range lc.connGroups {
			cg.conn.Close()
		}
		lc.connGroups = nil
	}
	lc.mu.Unlock()

	lc.notifyStateChange(oldState, LogicConnectionState(lc.state.Load()))

	tlog.Info(context.TODO(), "connecting to logic server address=%s reconnect=%v", lc.address, isReconnect)

	windowSize := int32(524288)
	maxMsgSize := 4 * 1024 * 1024
	connGroupCount := 4 // 默认 4 条独立 TCP 连接
	if lc.gateway != nil {
		grpcCfg := lc.gateway.GetGRPCConfig()
		windowSize = int32(grpcCfg.WindowSize)
		maxMsgSize = grpcCfg.MaxMessageSize
		streamCfg := lc.gateway.GetStreamConfig()
		if streamCfg.ConnGroupCount > 0 {
			connGroupCount = streamCfg.ConnGroupCount
		}
	}

	// 创建 N 条独立 gRPC 连接
	newGroups := make([]connGroup, connGroupCount)
	for g := 0; g < connGroupCount; g++ {
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
			tlog.Error(context.TODO(), "grpc.Dial failed error=%v address=%s connGroup=%d", err, lc.address, g)
			// 关闭已创建的连接
			for j := 0; j < g; j++ {
				newGroups[j].conn.Close()
			}
			lc.setState(LogicStateDisconnected)
			return err
		}
		newGroups[g] = connGroup{
			conn:   conn,
			client: protoGw.NewGatewayStreamClient(conn),
		}
	}

	// 在阻塞的 dial 之后重新检查关闭状态
	lc.mu.Lock()
	if lc.closing {
		lc.mu.Unlock()
		for _, cg := range newGroups {
			cg.conn.Close()
		}
		lc.setState(LogicStateDisconnected)
		return ErrConnectionClosing
	}
	lc.connGroups = newGroups
	lc.connGroupCount = connGroupCount
	lc.mu.Unlock()

	lc.mu.Lock()
	if lc.streamCancel != nil {
		lc.streamCancel()
	}
	lc.streamCtx, lc.streamCancel = context.WithCancel(context.Background())
	lc.mu.Unlock()

	// 关闭旧的流分片
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

	// 建立 stream：每个 shard 分配到对应的 connGroup
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i := 0; i < shardCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			groupIdx := idx % connGroupCount
			lc.mu.RLock()
			cg := lc.connGroups[groupIdx]
			ctx := lc.streamCtx
			lc.mu.RUnlock()

			if cg.client == nil || ctx == nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("client or context is nil") })
				return
			}

			if lc.gateway != nil {
				ctx = metadata.AppendToOutgoingContext(ctx, "sgate-gateway-id", lc.gateway.GetGatewayID())
			}
			stream, err := cg.client.OnData(ctx)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				tlog.Error(context.TODO(), "failed to establish stream shard shard=%d connGroup=%d error=%v", idx, groupIdx, err)
				return
			}

			shard := lc.streamManager.shards[idx]
			shard.mu.Lock()
			shard.stream = stream
			shard.ctx = ctx
			shard.mu.Unlock()

			tlog.Info(context.TODO(), "stream shard established shard=%d connGroup=%d", idx, groupIdx)
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		tlog.Error(context.TODO(), "failed to establish all stream shards error=%v", firstErr)
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
		for _, cg := range lc.connGroups {
			cg.conn.Close()
		}
		lc.connGroups = nil
		lc.mu.Unlock()
		lc.setState(LogicStateDisconnected)
		return firstErr
	}

	tlog.Info(context.TODO(), "all stream shards established count=%d connGroups=%d", shardCount, connGroupCount)

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

	tlog.Info(context.TODO(), "successfully connected to logic server address=%s shards=%d connGroups=%d isReconnect=%v", lc.address, shardCount, connGroupCount, isReconnect)

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
	for _, cg := range lc.connGroups {
		cg.conn.Close()
	}
	lc.connGroups = nil
	lc.mu.Unlock()

	close(lc.closed)
	tlog.Info(context.TODO(), "closed logic server connection")
}

// receiveMessages 从流中接收消息并推送到客户端连接
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
		tlog.Info(context.TODO(), "logic server disconnected, attempting reconnect in 5s...")
		time.Sleep(5 * time.Second)
		if lc.closing {
			return
		}
		err := lc.doConnect(true)
		if err != nil {
			tlog.Error(context.TODO(), "reconnect failed error=%v", err)
		} else {
			tlog.Info(context.TODO(), "reconnect succeeded")
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
			return nil // 消息已入队，等重连后自动发送
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
		Cmd: int32(routes.CmdHeartbeatReq),
	}

	err := hc.lc.SendMessageDirect(pingMsg)
	if err != nil {
		hc.failCount++
		tlog.Warn(context.TODO(), "health check failed failCount=%d maxFailures=%d error=%v", hc.failCount, hc.maxFailures, err)
		if hc.failCount >= hc.maxFailures {
			tlog.Error(context.TODO(), "too many health check failures, triggering reconnect failCount=%d", hc.failCount)
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
			tlog.Error(context.TODO(), "max reconnect attempts reached, trying etcd discovery maxAttempts=%d serverID=%s",
				rm.config.MaxAttempts, rm.lc.serverID)

			// 尝试通过 etcd 服务发现查找替代节点。
			// 尝试通过 etcd 发现替代节点
			if rm.lookupAddress != nil {
				if newAddr := rm.lookupAddress(rm.lc.serverID); newAddr != "" && newAddr != originalAddress {
					tlog.Info(context.TODO(), "discovered replacement address from etcd serverID=%s oldAddress=%s newAddress=%s",
						rm.lc.serverID, originalAddress, newAddr)
					rm.lc.mu.Lock()
					rm.lc.address = newAddr
					rm.lc.mu.Unlock()
					if err := rm.lc.doConnect(true); err == nil {
						tlog.Info(context.TODO(), "reconnect to replacement node succeeded address=%s", newAddr)
						return
					}
					tlog.Warn(context.TODO(), "reconnect to replacement node failed address=%s", newAddr)
				}
			}
			return
		}

		attempt++
		tlog.Info(context.TODO(), "attempting reconnect attempt=%d address=%s", attempt, rm.lc.address)

		select {
		case <-rm.stopCh:
			return
		case <-time.After(interval):
		}

		err := rm.lc.doConnect(true)
		if err == nil {
			tlog.Info(context.TODO(), "reconnect successful attempt=%d", attempt)
			return
		}

		tlog.Warn(context.TODO(), "reconnect failed attempt=%d error=%v", attempt, err)

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
	flushing              atomic.Bool           // 防止并发 Flush
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

// Flush 冲刷队列中的消息，重连后调用以恢复转发
func (mq *StreamMessageQueue) Flush(lc *LogicClient) {
	if !mq.flushing.CompareAndSwap(false, true) {
		return // 已有 Flush 在运行
	}
	defer mq.flushing.Store(false)

	const maxRetries = 100
	const retryInterval = 100 * time.Millisecond

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
			if err := lc.SendMessageDirect(msg); err == nil {
				continue
			}
		}
		// 发送失败，尝试重新入队
		if mq.policy == config.QueuePolicyBlock {
			// Block 策略 Enqueue 会阻塞直到有空间，不会丢消息
			mq.Enqueue(msg)
		} else {
			if err := mq.Enqueue(msg); err != nil {
				tlog.Warn(context.TODO(), "flush: re-enqueue failed, message dropped policy=%v error=%v", mq.policy, err)
			}
		}
		time.Sleep(retryInterval)
	}
}
