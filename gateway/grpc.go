package gateway

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/discovery"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
}

var DefaultHealthCheckConfig = HealthCheckConfig{
	Interval:    5 * time.Second,
	Timeout:     3 * time.Second,
	MaxFailures: 3,
}

type StreamShard struct {
	stream protobuf.GatewayService_StreamMessagesClient
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

type StreamManager struct {
	shards []*StreamShard
}

func NewStreamManager(shardCount int) *StreamManager {
	if shardCount <= 0 {
		shardCount = runtime.NumCPU()
	}
	sm := &StreamManager{
		shards: make([]*StreamShard, shardCount),
	}
	for i := range sm.shards {
		sm.shards[i] = &StreamShard{}
	}
	return sm
}

func (sm *StreamManager) ShardCount() int {
	return len(sm.shards)
}

func (sm *StreamManager) GetShard(connectionID string) *StreamShard {
	h := fnv.New32a()
	h.Write([]byte(connectionID))
	idx := h.Sum32() % uint32(len(sm.shards))
	return sm.shards[idx]
}

func (s *StreamShard) SendMessage(msg *protobuf.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == nil {
		return ErrNotConnected
	}
	return s.stream.Send(msg)
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
		streamManager:     NewStreamManager(0),
		gateway:           gateway,
		closing:           false,
		closed:            make(chan struct{}),
		shardCount:        runtime.NumCPU(),
	}
}

func NewLogicClientWithConfig(gateway GatewayInterface, reconnectConfig ReconnectConfig, healthCheckConfig HealthCheckConfig) *LogicClient {
	return &LogicClient{
		state:             int32(StateDisconnected),
		reconnectConfig:   reconnectConfig,
		healthCheckConfig: healthCheckConfig,
		streamManager:     NewStreamManager(0),
		gateway:           gateway,
		closing:           false,
		closed:            make(chan struct{}),
		shardCount:        runtime.NumCPU(),
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
		go callback(oldState, newState)
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
		lc.conn.Close()
		lc.conn = nil
		lc.client = nil
	}
	lc.mu.Unlock()

	lc.notifyStateChange(oldState, LogicConnectionState(atomic.LoadInt32(&lc.state)))

	tlog.Info("connecting to logic server", "address", lc.address, "reconnect", isReconnect)

	conn, err := grpc.Dial(lc.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	lc.streamCtx, lc.streamCancel = context.WithCancel(context.Background())
	lc.mu.Unlock()

	shardCount := lc.shardCount
	lc.streamManager = NewStreamManager(shardCount)

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
		go lc.streamManager.shards[i].receiveMessages(lc, i)
	}

	if lc.reconnectManager == nil {
		lc.reconnectManager = NewReconnectManager(lc, lc.reconnectConfig)
		go lc.reconnectManager.Run()
	}

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
	for {
		select {
		case <-lc.closed:
			return
		case <-s.ctx.Done():
			return
		default:
		}

		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()

		if stream == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		msg, err := stream.Recv()
		if err != nil {
			tlog.Error("shard receive error", "shard", shardIdx, "error", err)

			lc.mu.RLock()
			closing := lc.closing
			lc.mu.RUnlock()

			if closing {
				return
			}

			lc.handleDisconnection()
			return
		}

		if lc.gateway != nil && msg.ConnectionId != "" {
			if msg.Route == "server.kick" {
				reason := ""
				if msg.Payload != nil {
					reason = msg.Payload["reason"]
				}
				conn := lc.gateway.GetConnectionManager().GetConnection(msg.ConnectionId)
				if conn != nil {
					responseData, _ := proto.Marshal(msg)
					conn.Send(responseData)
					tlog.Info("kicking connection by logic server", "connectionID", msg.ConnectionId, "reason", reason)
					lc.gateway.GetConnectionManager().RemoveConnection(msg.ConnectionId)
					if conn.Conn != nil {
						conn.Conn.Close()
					}
				}
			} else if msg.Route == "server.join_group" {
				groupID := msg.Payload["groupID"]
				connID := msg.ConnectionId
				if groupID != "" && connID != "" {
					lc.gateway.GetConnectionManager().AddUserToGroup(groupID, connID)
					tlog.Debug("connection joined group by logic server", "connectionID", connID, "groupID", groupID)
				}
				conn := lc.gateway.GetConnectionManager().GetConnection(connID)
				if conn != nil {
					responseData, _ := proto.Marshal(msg)
					conn.Send(responseData)
				}
			} else if msg.Route == "server.leave_group" {
				groupID := msg.Payload["groupID"]
				connID := msg.ConnectionId
				if groupID != "" && connID != "" {
					lc.gateway.GetConnectionManager().RemoveUserFromGroup(groupID, connID)
				}
			} else {
				conn := lc.gateway.GetConnectionManager().GetConnection(msg.ConnectionId)
				if conn != nil {
					responseData, err := proto.Marshal(msg)
					if err != nil {
						tlog.Error("failed to marshal response", "error", err, "connectionID", msg.ConnectionId)
					} else {
						err = conn.Send(responseData)
						if err != nil {
							tlog.Error("failed to forward response to client", "error", err, "connectionID", msg.ConnectionId)
						}
					}
				} else {
					tlog.Warn("client connection not found", "connectionID", msg.ConnectionId)
				}
			}
		} else {
			if msg.ConnectionId == "" {
				tlog.Warn("received message with empty ConnectionId", "route", msg.Route)
			}
		}
	}
}

func (lc *LogicClient) handleDisconnection() {
	lc.mu.RLock()
	closing := lc.closing
	lc.mu.RUnlock()

	if closing {
		return
	}

	state := lc.getState()
	if state == StateConnecting || state == StateReconnecting {
		return
	}

	lc.setState(StateDisconnected)

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

func (lc *LogicClient) SendMessage(msg *protobuf.Message) error {
	lc.mu.RLock()
	state := lc.getState()
	closing := lc.closing
	lc.mu.RUnlock()

	if closing {
		return ErrConnectionClosing
	}

	if state != StateConnected {
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
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

func NewHealthChecker(lc *LogicClient, config HealthCheckConfig) *HealthChecker {
	return &HealthChecker{
		lc:          lc,
		interval:    config.Interval,
		timeout:     config.Timeout,
		maxFailures: config.MaxFailures,
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

	pingMsg := &protobuf.Message{
		Route:   "ping",
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
	queue []*protobuf.Message
	mu    sync.Mutex
	cond  *sync.Cond
}

func NewStreamMessageQueue() *StreamMessageQueue {
	mq := &StreamMessageQueue{
		queue: make([]*protobuf.Message, 0),
	}
	mq.cond = sync.NewCond(&mq.mu)
	return mq
}

func (mq *StreamMessageQueue) Enqueue(msg *protobuf.Message) {
	mq.mu.Lock()
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
	for {
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
	GetRouteManager() *RouteManager
	GetConnectionManager() *ConnectionManager
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
					Route: "error",
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
				Route: "error",
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
	s.gateway.GetRouteManager().HandleRoute(connectionID, msg.Route, msg.Payload, callback, ctx)
}

func StartGRPCServer(gateway GatewayInterface, port string) error {
	tlog.Info("creating gRPC server")
	server := grpc.NewServer()
	tlog.Info("registering GatewayService")
	protobuf.RegisterGatewayServiceServer(server, NewGRPCServer(gateway))

	tlog.Info("listening on port", "port", port)
	listener, err := net.Listen("tcp", port)
	if err != nil {
		tlog.Error("failed to listen on port", "error", err, "port", port)
		return err
	}

	go func() {
		if err := server.Serve(listener); err != nil {
			tlog.Error("gRPC server failed", "error", err)
		}
	}()

	tlog.Info("gRPC server started", "port", port)
	return nil
}

type LogicClientPool struct {
	clients   map[string]*LogicClient
	mu        sync.RWMutex
	gateway   GatewayInterface
	discovery *ServiceDiscovery
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewLogicClientPool(gateway GatewayInterface) *LogicClientPool {
	return &LogicClientPool{
		clients: make(map[string]*LogicClient),
		gateway: gateway,
		stopCh:  make(chan struct{}),
	}
}

func (pool *LogicClientPool) SetDiscovery(discovery *ServiceDiscovery) {
	pool.discovery = discovery
	discovery.OnServiceChange(pool.handleServiceChange)
}

func (pool *LogicClientPool) handleServiceChange(event discovery.ServiceEvent) {
	switch event.Type {
	case discovery.EventRegister:
		pool.handleServiceRegister(event)
	case discovery.EventDeregister:
		pool.handleServiceDeregister(event)
	}
}

func (pool *LogicClientPool) handleServiceRegister(event discovery.ServiceEvent) {
	pool.mu.RLock()
	_, exists := pool.clients[event.Service.ServiceID]
	pool.mu.RUnlock()

	if exists {
		return
	}

	if routesStr, ok := event.Service.Metadata["routes"]; ok && routesStr != "" {
		for _, route := range splitRoutes(routesStr) {
			pool.gateway.GetRouteManager().RegisterLogicRoute(route)
			tlog.Info("logic route registered from discovery", "route", route, "serviceID", event.Service.ServiceID)
		}
	}

	client := NewLogicClient(pool.gateway)
	client.shardCount = runtime.NumCPU()

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
	pool.mu.Unlock()

	tlog.Info("logic client added to pool",
		"serviceID", event.Service.ServiceID,
		"address", event.Service.Address,
		"totalClients", pool.ClientCount(),
	)
}

func (pool *LogicClientPool) handleServiceDeregister(event discovery.ServiceEvent) {
	pool.mu.Lock()
	client, exists := pool.clients[event.Service.ServiceID]
	if exists {
		delete(pool.clients, event.Service.ServiceID)
	}
	pool.mu.Unlock()

	if exists && client != nil {
		tlog.Warn("logic service offline, closing connection immediately",
			"serviceID", event.Service.ServiceID,
			"address", event.Service.Address,
		)
		go client.Close()
	}

	tlog.Warn("logic client removed from pool",
		"serviceID", event.Service.ServiceID,
		"address", event.Service.Address,
		"totalClients", pool.ClientCount(),
	)
}

func (pool *LogicClientPool) SendMessage(msg *protobuf.Message) error {
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
	defer pool.mu.RUnlock()

	if len(pool.clients) == 0 {
		return ErrNotConnected
	}

	connected := make([]*LogicClient, 0, len(pool.clients))
	for _, client := range pool.clients {
		if client.IsConnected() {
			connected = append(connected, client)
		}
	}

	if len(connected) == 0 {
		return ErrNotConnected
	}

	h := fnv.New32a()
	h.Write([]byte(msg.ConnectionId))
	idx := h.Sum32() % uint32(len(connected))
	return connected[idx].SendMessage(msg)
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
	}
	pool.mu.Unlock()

	if exists && client != nil {
		go client.Close()
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
	pool.mu.Unlock()

	go func() {
		tlog.Info("connecting to static logic service", "address", address)
		if err := client.Connect(address); err != nil {
			tlog.Error("failed to connect to static logic service", "address", address, "error", err)
		}
	}()
}

func (pool *LogicClientPool) IsConnected() bool {
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
