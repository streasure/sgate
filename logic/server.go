package logic

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// appendMessageFast 将 *gateway.StreamData 序列化后直接追加到 dst（零中间分配）。
// 快速路径仅覆盖常见字段：session_id(1)/user_key(2)/route(3)/cmd(4)/
// data(5)/timestamp(6)——反向推送的绝大多数消息（回包/推送通知）只含这些字段。
// 若设置了扩展字段（payload/checksum/protocol_version 等），回退到 proto.Marshal 以保证
// 与标准序列化字节完全一致。字段号与 backend.proto 定义一一对应。
func appendMessageFast(dst []byte, m *gateway.StreamData) []byte {
	if m.Payload != nil || m.Checksum != "" || m.SeqId != 0 ||
		m.ProtocolVersion != "" {
		b, err := proto.Marshal(m)
		if err != nil {
			return dst
		}
		return append(dst, b...)
	}
	if m.SessionId != "" {
		dst = protowire.AppendTag(dst, 1, protowire.BytesType)
		dst = protowire.AppendString(dst, m.SessionId)
	}
	if m.UserKey != "" {
		dst = protowire.AppendTag(dst, 2, protowire.BytesType)
		dst = protowire.AppendString(dst, m.UserKey)
	}
	if m.Route != "" {
		dst = protowire.AppendTag(dst, 3, protowire.BytesType)
		dst = protowire.AppendString(dst, m.Route)
	}
	if m.Cmd != 0 {
		dst = protowire.AppendTag(dst, 4, protowire.VarintType)
		dst = protowire.AppendVarint(dst, uint64(m.Cmd))
	}
	if len(m.Data) > 0 {
		dst = protowire.AppendTag(dst, 5, protowire.BytesType)
		dst = protowire.AppendBytes(dst, m.Data)
	}
	if m.Timestamp != 0 {
		dst = protowire.AppendTag(dst, 6, protowire.VarintType)
		dst = protowire.AppendVarint(dst, uint64(m.Timestamp))
	}
	return dst
}

type DisconnectCallback func(connectionID string)

// reverseItem 携带预序列化的反向消息，避免 flushLoop 单协程做 proto.Marshal。
// reverseItem 反向推送队列条目。
// 优化：直接携带 *gateway.StreamData 引用，由 flushLoop 在组装 multi-conn
// 批量缓冲时用 appendMessageFast 就地序列化。原先在 worker 侧先 proto.Marshal
// 分配独立 []byte、flushLoop 再整体拷贝一次，两条开销全部消除。
type reverseItem struct {
	connID string
	route  string
	msg    *gateway.StreamData
}

type streamConn struct {
	stream    gateway.GatewayStream_OnDataServer
	sendCh    chan reverseItem
	done      chan struct{}
	gatewayID string
}

func newStreamConn(stream gateway.GatewayStream_OnDataServer, sendChSize int, gatewayID string) *streamConn {
	if sendChSize <= 0 {
		sendChSize = 1048576
	}
	sc := &streamConn{
		stream:    stream,
		sendCh:    make(chan reverseItem, sendChSize),
		done:      make(chan struct{}),
		gatewayID: gatewayID,
	}
	go sc.flushLoop()
	return sc
}

// Send 将消息引用推入 sendCh（无序列化、无分配）。
// 序列化延迟到 flushLoop 组批时用 appendMessageFast 直写完成。
func (sc *streamConn) Send(msg *gateway.StreamData) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("send on closed channel: %v", r)
		}
	}()
	item := reverseItem{
		connID: msg.SessionId,
		route:  msg.Route,
		msg:    msg,
	}
	select {
	case sc.sendCh <- item:
		return nil
	default:
		return fmt.Errorf("send channel full")
	}
}

func (sc *streamConn) flushLoop() {
	defer close(sc.done)
	// 批量上限 1024：单次 stream.Send 携带更多回复帧，
	// 将 gRPC Send 固定开销（HTTP/2 帧、封送、syscall）再摊薄 4 倍
	const maxBatchCount = 1024
	batch := make([]reverseItem, 0, maxBatchCount)
	// mcBuf 跨批次复用（gRPC Send 同步序列化，返回后即可安全重用）
	mcBuf := make([]byte, 0, 128*1024)

	// sendBatch 发送一批消息：
	// - RouteBatch / server.* 路由消息直接 stream.Send
	// - 常规消息用 multi-conn 格式打包：[2字节 connIDLen][connID][4字节 payloadLen][payload] 重复
	//   可将不同 connID 的消息合并为一次 stream.Send
	// payload 由 appendMessageFast 直写进 mcBuf，无中间分配、无二次拷贝
	sendBatch := func(items []reverseItem) {
		mcCount := 0
		flushMultiConn := func() {
			if mcCount == 0 {
				return
			}
			_ = sc.stream.Send(&gateway.StreamData{
				Route: gateway.RouteBatch,
				Data:  mcBuf,
				Cmd:   int32(mcCount),
			})
			mcBuf = mcBuf[:0]
			mcCount = 0
		}
		for _, item := range items {
			if item.route == gateway.RouteBatch || (len(item.route) >= 7 && item.route[:7] == "server.") || item.route == gateway.RouteLogin {
				flushMultiConn()
				_ = sc.stream.Send(item.msg)
			} else {
				// multi-conn 格式: [2字节 connIDLen][connID][4字节 payloadLen][payload]
				connID := item.connID
				var connIDLenBuf [2]byte
				binary.BigEndian.PutUint16(connIDLenBuf[:], uint16(len(connID)))
				mcBuf = append(mcBuf, connIDLenBuf[:]...)
				mcBuf = append(mcBuf, connID...)
				payload, err := marshalLogicFrame(item.msg)
				if err != nil {
					continue
				}
				var payloadLenBuf [4]byte
				binary.BigEndian.PutUint32(payloadLenBuf[:], uint32(len(payload)))
				mcBuf = append(mcBuf, payloadLenBuf[:]...)
				mcBuf = append(mcBuf, payload...)
				mcCount++
			}
		}
		flushMultiConn()
	}

	for {
		item, ok := <-sc.sendCh
		if !ok {
			return
		}
		batch = batch[:0]
		batch = append(batch, item)
		drained := true
		for drained {
			select {
			case it, ok := <-sc.sendCh:
				if !ok {
					drained = false
					break
				}
				batch = append(batch, it)
				if len(batch) >= maxBatchCount {
					drained = false
				}
			default:
				drained = false
			}
		}
		sendBatch(batch)
	}
}

func marshalLogicFrame(msg *gateway.StreamData) ([]byte, error) {
	body := msg.Data
	if len(body) == 0 {
		var err error
		body, err = proto.Marshal(msg)
		if err != nil {
			return nil, err
		}
	}
	cmd := msg.Cmd
	if cmd == 0 {
		cmd = gateway.CmdForRoute(msg.Route)
	}
	return proto.Marshal(&gateway.MessageFrame{Cmd: cmd, SeqId: msg.SeqId, Body: body})
}

type pushGroup struct {
	members map[string]struct{}
}

type Server struct {
	gateway.UnimplementedGatewayStreamServer
	routes       sync.Map
	cmdRoutes    sync.Map
	connections  sync.Map
	onDisconnect []DisconnectCallback
	mu           sync.Mutex // protects onDisconnect

	serverID   string
	groupMu    sync.RWMutex
	groups     map[string]*pushGroup
	connGroups map[string]map[string]struct{}

	// userConnections 存储 userUUID → connectionID 映射。
	// Logic 不需要关心 streamConn ID，只需知道 client 的 connectionID。
	// 由 handler 在 login/handshake 时调用 RegisterUser 注册。
	userConnections sync.Map // userUUID -> connectionID
	// connUserReverse 存储 connectionID → userUUID 反向映射，用于断连时自动清理。
	connUserReverse sync.Map // connectionID -> userUUID

	// dispatchChs 分片分发通道：单一全局 channel 在千万级 QPS 下成为
	// 锁竞争热点（96 个 recv 生产者 + 1536 个 worker 抢一把 chan lock），
	// 分片后竞争面缩小为 1/N。
	dispatchChs         []chan dispatchItem
	dispatchChTotalSize int
	dispatchRR          atomic.Uint64
	dispatchWg          sync.WaitGroup
	streamChSize        int
	dispatchWorkerCount int
	passthrough         bool
	connIDCounter       atomic.Uint64
	stopOnce            sync.Once
}

type dispatchItem struct {
	msg  *gateway.StreamData
	conn *streamConn
}

// msgPool reuses gateway.StreamData structs in the RouteBatch dispatch path
// to eliminate per-frame struct allocation at 20M+ QPS.
// After dispatchMessage returns, the dispatch worker resets and returns
// the Message to the pool. This is safe because all callbacks are called
// synchronously within dispatchMessage — no async reference to msg survives.
var msgPool = sync.Pool{
	New: func() interface{} { return &gateway.StreamData{} },
}

func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		groups:       make(map[string]*pushGroup),
		connGroups:   make(map[string]map[string]struct{}),
		streamChSize: 1048576,
	}
	for _, opt := range opts {
		opt(s)
	}

	// 分片分发：每 CPU 一个 channel，总容量与原先单通道相当
	shardCount := runtime.NumCPU()
	if shardCount < 4 {
		shardCount = 4
	}
	totalSize := runtime.NumCPU() * 65536
	if s.dispatchChTotalSize > 0 {
		totalSize = s.dispatchChTotalSize
	}
	chSize := totalSize / shardCount
	if chSize < 4096 {
		chSize = 4096
	}
	s.dispatchChs = make([]chan dispatchItem, shardCount)
	for i := range s.dispatchChs {
		s.dispatchChs[i] = make(chan dispatchItem, chSize)
	}

	// 过多 worker 会让 dispatch channel 和调度器成为瓶颈。
	// 默认使用每核 2 个 worker；高负载部署可通过 WithDispatchWorkerCount 覆盖。
	workerCount := runtime.NumCPU() * 2
	if s.dispatchWorkerCount > 0 {
		workerCount = s.dispatchWorkerCount
	}
	// worker 均匀绑定到各分片，只消费自己分片的通道
	base := workerCount / shardCount
	rem := workerCount % shardCount
	for i, ch := range s.dispatchChs {
		n := base
		if i < rem {
			n++
		}
		for j := 0; j < n; j++ {
			s.dispatchWg.Add(1)
			go s.dispatchWorker(ch)
		}
	}

	return s
}

func (s *Server) dispatchWorker(ch <-chan dispatchItem) {
	defer s.dispatchWg.Done()
	for item := range ch {
		s.dispatchOne(item)
	}
}

// dispatchOne 处理单条消息：执行路由回调并归还 Message 到池。
func (s *Server) dispatchOne(item dispatchItem) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("dispatch handler panic",
				"route", item.msg.Route,
				"error", r,
			)
		}
	}()
	connID := item.msg.SessionId
	s.dispatchMessage(item.msg, func(response *gateway.StreamData) {
		if response != nil {
			if response.SessionId == "" {
				response.SessionId = connID
			}
			if response.SessionId == "" {
				return
			}
			item.conn.Send(response)
		}
	})
	// Return pooled Message after dispatch.
	// Safe because dispatchMessage calls all callbacks synchronously;
	// no async reference to item.msg survives after this point.
	item.msg.Reset()
	msgPool.Put(item.msg)
}

type ServerOption func(*Server)

func WithServerID(serverID string) ServerOption {
	return func(s *Server) { s.serverID = serverID }
}

func WithDispatchWorkers(n int) ServerOption {
	return func(s *Server) { s.dispatchWorkerCount = n }
}

func WithDispatchChSize(n int) ServerOption {
	return func(s *Server) {
		if n > 0 {
			s.dispatchChTotalSize = n
		}
	}
}

func WithStreamChSize(n int) ServerOption {
	return func(s *Server) { s.streamChSize = n }
}

func WithServerPassthrough() ServerOption {
	return func(s *Server) { s.passthrough = true }
}

// Stop gracefully shuts down dispatch workers by closing channels and waiting.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		for _, ch := range s.dispatchChs {
			close(ch)
		}
		s.dispatchWg.Wait()
	})
}

func (s *Server) GetServerID() string {
	return s.serverID
}

func (s *Server) serverGroupID() string {
	return "server:" + s.serverID
}

func (s *Server) OnDisconnect(cb DisconnectCallback) {
	s.mu.Lock()
	s.onDisconnect = append(s.onDisconnect, cb)
	s.mu.Unlock()
}

func (s *Server) OnData(stream gateway.GatewayStream_OnDataServer) error {
	connectionID := fmt.Sprintf("conn_%s_%d", s.serverID, s.connIDCounter.Add(1))

	gatewayID := ""
	if values := metadata.ValueFromIncomingContext(stream.Context(), "sgate-gateway-id"); len(values) > 0 {
		gatewayID = values[0]
	}
	// Legacy clients without metadata retain per-stream behavior.
	if gatewayID == "" {
		gatewayID = connectionID
	}
	conn := newStreamConn(stream, s.streamChSize, gatewayID)
	s.connections.Store(connectionID, conn)

	if s.serverID != "" {
		s.JoinGroup(s.serverGroupID(), connectionID)
	}

	tlog.Info("new stream connection", "connectionID", connectionID, "gatewayID", gatewayID, "serverID", s.serverID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			close(conn.sendCh)
			<-conn.done
			s.connections.CompareAndDelete(connectionID, conn)
			s.leaveAllGroups(connectionID)
			s.cleanupUserByConnection(connectionID)
			tlog.Info("stream connection closed", "connectionID", connectionID, "serverID", s.serverID)
			s.mu.Lock()
			callbacks := make([]DisconnectCallback, len(s.onDisconnect))
			copy(callbacks, s.onDisconnect)
			s.mu.Unlock()
			for _, cb := range callbacks {
				go cb(connectionID)
			}
			return err
		}

		if s.passthrough {
			// Throughput mode intentionally skips route parsing, validation, and dispatch.
			// The received protobuf envelope is queued unchanged for the reverse stream.
			_ = conn.Send(msg)
			continue
		}

		// Forward batch: unbatch and dispatch each entry individually.
		// Format: [4-byte payloadLen][payload] repeated, payload = serialized gateway.StreamData
		// Two sources of RouteBatch:
		//   1. Gateway gnet-level batch (handleBatchTraffic): ConnectionId on outer message,
		//      inner payloads are raw client frames (may lack ConnectionId)
		//   2. Gateway startSendLoop batch: each inner payload is a full gateway.StreamData
		//      with its own ConnectionId
		//
		// Optimization: use ExtractRouteAndCmd (lightweight field scan) instead of
		// full proto.Unmarshal. This avoids allocating strings/maps for Payload,
		// UserUuid, etc. — reducing per-frame allocation from ~200B to ~0B at 20M QPS.
		// The Data field points to the raw payload (zero-copy) for handlers that
		// need full message data (Dispatcher/protoEntry will Unmarshal Data themselves).
		if msg.Route == gateway.RouteBatch {
			data := msg.Data
			connID := msg.SessionId
			// multi-conn batch: 外层 ConnectionId 为空，需从每条 payload 提取各自的 ConnectionId
			// single-conn batch: 外层 ConnectionId 已设置，所有 payload 共用，无需逐条提取
			multiConn := connID == ""
			for len(data) >= 4 {
				payloadLen := int(binary.BigEndian.Uint32(data[:4]))
				if payloadLen == 0 || len(data) < 4+payloadLen {
					break
				}
				payload := data[4 : 4+payloadLen]
				data = data[4+payloadLen:]

				cmd, seqID, body, frameOK := gateway.ExtractMessageFrame(payload)
				var route string
				var innerConnID string
				if !frameOK {
					if multiConn {
						route, cmd, innerConnID = gateway.ExtractRouteCmdAndConnID(payload)
					} else {
						route, cmd = gateway.ExtractRouteAndCmd(payload)
					}
					if route == "" {
						continue
					}
					body = payload
				} else if cmd == 0 || len(body) == 0 {
					continue
				}
				innerMsg := msgPool.Get().(*gateway.StreamData)
				// The client frame body contains the gateway StreamData envelope.
				// Unwrap it here so business handlers receive only their protobuf
				// payload, while the session identity remains authoritative.
				var envelope gateway.StreamData
				if err := proto.Unmarshal(body, &envelope); err == nil && envelope.Route != "" {
					route = envelope.Route
					if envelope.Cmd != 0 {
						cmd = envelope.Cmd
					}
					body = envelope.Data
					if envelope.UserKey != "" {
						msg.UserKey = envelope.UserKey
					}
				}
				innerMsg.Route = route
				innerMsg.Cmd = cmd
				innerMsg.SeqId = seqID
				if multiConn {
					innerMsg.SessionId = innerConnID
				} else {
					innerMsg.SessionId = connID
				}
				innerMsg.Data = body // zero-copy: points into batch's Data buffer

				select {
				case s.dispatchChs[s.dispatchRR.Add(1)%uint64(len(s.dispatchChs))] <- dispatchItem{msg: innerMsg, conn: conn}:
				default:
					// dispatch channel full, drop and return msg to pool
					innerMsg.Reset()
					msgPool.Put(innerMsg)
				}
			}
			continue
		}

		if msg.SessionId == "" {
			tlog.Warn("received message with empty ConnectionId", "route", msg.Route)
		}

		select {
		case s.dispatchChs[s.dispatchRR.Add(1)%uint64(len(s.dispatchChs))] <- dispatchItem{msg: msg, conn: conn}:
		default:
			tlog.Warn("dispatch channel full, dropping message", "route", msg.Route, "connectionID", msg.SessionId)
		}
	}
}

func (s *Server) SendMessage(ctx context.Context, msg *gateway.StreamData) (*gateway.StreamData, error) {
	var response *gateway.StreamData

	func() {
		defer func() {
			if r := recover(); r != nil {
				tlog.Error("SendMessage dispatchMessage panic recovered", "error", r, "route", msg.Route, "cmd", msg.Cmd)
			}
		}()
		s.dispatchMessage(msg, func(resp *gateway.StreamData) { response = resp })
	}()

	return response, nil
}

func (s *Server) PushToConnection(connectionID string, msg *gateway.StreamData) error {
	val, ok := s.connections.Load(connectionID)
	if !ok {
		return fmt.Errorf("connection %s not found", connectionID)
	}
	conn := val.(*streamConn)
	if msg.SessionId == "" {
		msg.SessionId = connectionID
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	return conn.Send(msg)
}

// RegisterUser 注册 userUUID → connectionID 映射。
// 在 login/handshake handler 中调用，使 Broadcast/PushToGroup 等 API 能正确工作。
func (s *Server) RegisterUser(userUUID, connectionID string) {
	// 先清理旧的映射（同 userUUID 可能重连）
	if oldConnID, ok := s.userConnections.LoadAndDelete(userUUID); ok {
		s.connUserReverse.Delete(oldConnID.(string))
	}
	s.userConnections.Store(userUUID, connectionID)
	s.connUserReverse.Store(connectionID, userUUID)
}

// UnregisterUser 移除 userUUID → connectionID 映射。
func (s *Server) UnregisterUser(userUUID string) {
	if connID, ok := s.userConnections.LoadAndDelete(userUUID); ok {
		s.connUserReverse.Delete(connID.(string))
	}
}

// GetConnectionIDByUser 获取 userUUID 对应的 connectionID。
func (s *Server) GetConnectionIDByUser(userUUID string) (string, bool) {
	val, ok := s.userConnections.Load(userUUID)
	if !ok {
		return "", false
	}
	return val.(string), true
}

// cleanupUserByConnection 断连时根据 connectionID 自动清理 userUUID 映射。
func (s *Server) cleanupUserByConnection(connectionID string) {
	if userUUID, ok := s.connUserReverse.LoadAndDelete(connectionID); ok {
		s.userConnections.Delete(userUUID.(string))
	}
}

// Offline removes all user and push-group state for a client session. It is
// called when sgate reports a TCP/WebSocket close without a business logout.
func (s *Server) Offline(connectionID, userKey string) {
	s.leaveAllGroups(connectionID)
	if userKey != "" {
		s.UnregisterUser(userKey)
		return
	}
	s.cleanupUserByConnection(connectionID)
}

func (s *Server) PushToServer(msg *gateway.StreamData, exclude ...string) int {
	if s.serverID == "" {
		return 0
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	count := 0
	s.connections.Range(func(key, value interface{}) bool {
		conn := value.(*streamConn)
		if err := conn.Send(msg); err == nil {
			count++
		}
		return true
	})
	return count
}

// sentGatewaysPool 复用 PushToEachGateway 的去重 map，避免每次广播/组推送分配新 map。
var sentGatewaysPool = sync.Pool{
	New: func() interface{} { return make(map[string]struct{}, 16) },
}

// PushToEachGateway sends a control command once per Gateway instance rather
// than once per stream shard. Each target Gateway then fans the command out to
// its local connections. Streams without an identity intentionally remain
// independent for backward compatibility.
func (s *Server) PushToEachGateway(msg *gateway.StreamData) int {
	if s.serverID == "" {
		return 0
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	sentGateways := sentGatewaysPool.Get().(map[string]struct{})
	count := 0
	s.connections.Range(func(_, value interface{}) bool {
		conn := value.(*streamConn)
		if _, alreadySent := sentGateways[conn.gatewayID]; alreadySent {
			return true
		}
		if err := conn.Send(msg); err == nil {
			sentGateways[conn.gatewayID] = struct{}{}
			count++
		}
		return true
	})
	// 清空并归还 map 到池
	for k := range sentGateways {
		delete(sentGateways, k)
	}
	sentGatewaysPool.Put(sentGateways)
	return count
}

// Broadcast 向所有客户端广播消息。
// 通过 server.broadcast 指令让 Gateway 的 ConnectionManager.Broadcast 执行推送。
// Logic 不直接操作 streamConn，只通过 Gateway 侧的推送机制。
func (s *Server) Broadcast(msg *gateway.StreamData, exclude ...string) int {
	pushMsg := &gateway.StreamData{
		Route:   gateway.RouteServerBroadcast,
		Payload: make(map[string]string),
	}
	if msg.Route != "" {
		pushMsg.Payload["route"] = msg.Route
	}
	if msg.Payload != nil {
		for k, v := range msg.Payload {
			pushMsg.Payload[k] = v
		}
	}
	if len(exclude) > 0 {
		pushMsg.Payload["exclude"] = strings.Join(exclude, ",")
	}
	pushMsg.Timestamp = time.Now().UnixMilli()

	s.PushToEachGateway(pushMsg)
	return 0
}

func (s *Server) JoinGroup(groupID, connectionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	g, ok := s.groups[groupID]
	if !ok {
		g = &pushGroup{members: make(map[string]struct{})}
		s.groups[groupID] = g
	}
	g.members[connectionID] = struct{}{}

	groups, ok := s.connGroups[connectionID]
	if !ok {
		groups = make(map[string]struct{})
		s.connGroups[connectionID] = groups
	}
	groups[groupID] = struct{}{}

	return len(g.members)
}

func (s *Server) LeaveGroup(groupID, connectionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	g, ok := s.groups[groupID]
	if !ok {
		return 0
	}
	delete(g.members, connectionID)
	memberCount := len(g.members)

	if memberCount == 0 {
		delete(s.groups, groupID)
	}

	if groups, ok := s.connGroups[connectionID]; ok {
		delete(groups, groupID)
		if len(groups) == 0 {
			delete(s.connGroups, connectionID)
		}
	}

	return memberCount
}

func (s *Server) leaveAllGroups(connectionID string) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	groups, ok := s.connGroups[connectionID]
	if !ok {
		return
	}

	for groupID := range groups {
		if g, ok := s.groups[groupID]; ok {
			delete(g.members, connectionID)
			if len(g.members) == 0 {
				delete(s.groups, groupID)
			}
		}
	}

	delete(s.connGroups, connectionID)
}

func (s *Server) PushToGroup(groupID string, msg *gateway.StreamData, exclude ...string) int {
	pushMsg := &gateway.StreamData{
		Route:   gateway.RouteServerSendToGroup,
		Payload: make(map[string]string),
	}
	pushMsg.Payload["groupID"] = groupID
	if msg.Route != "" {
		pushMsg.Payload["route"] = msg.Route
	}
	if msg.Payload != nil {
		for k, v := range msg.Payload {
			pushMsg.Payload[k] = v
		}
	}
	if len(exclude) > 0 {
		pushMsg.Payload["exclude"] = strings.Join(exclude, ",")
	}
	pushMsg.Timestamp = time.Now().UnixMilli()

	s.PushToEachGateway(pushMsg)
	return 0
}

func (s *Server) GetGroupMembers(groupID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	g, ok := s.groups[groupID]
	if !ok {
		return nil
	}

	members := make([]string, 0, len(g.members))
	for connID := range g.members {
		members = append(members, connID)
	}
	return members
}

func (s *Server) GetGroupCount(groupID string) int {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	g, ok := s.groups[groupID]
	if !ok {
		return 0
	}
	return len(g.members)
}

func (s *Server) GetConnectionGroups(connectionID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	groups, ok := s.connGroups[connectionID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(groups))
	for groupID := range groups {
		result = append(result, groupID)
	}
	return result
}

func (s *Server) GetServerPlayerCount() int {
	if s.serverID == "" {
		return 0
	}
	return s.GetGroupCount(s.serverGroupID())
}

func (s *Server) ForEachConnection(fn func(connectionID string) bool) {
	s.connections.Range(func(key, _ interface{}) bool {
		return fn(key.(string))
	})
}

func (s *Server) ConnectionExists(connectionID string) bool {
	_, ok := s.connections.Load(connectionID)
	return ok
}

func (s *Server) RegisterGatewayStreamServer(srv *grpc.Server) {
	gateway.RegisterGatewayStreamServer(srv, s)
}

func (s *Server) GetConnectionCount() int {
	count := 0
	s.connections.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (s *Server) JoinGroupByUser(groupID, serverID, userUUID string) {
	s.PushToServer(&gateway.StreamData{
		Route: gateway.RouteServerJoinGroupByUser,
		Payload: map[string]string{
			"groupID":  groupID,
			"serverID": serverID,
			"userUUID": userUUID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) LeaveGroupByUser(groupID, serverID, userUUID string) {
	s.PushToServer(&gateway.StreamData{
		Route: gateway.RouteServerLeaveGroupByUser,
		Payload: map[string]string{
			"groupID":  groupID,
			"serverID": serverID,
			"userUUID": userUUID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) CreateGroup(groupID, groupName string) {
	s.PushToServer(&gateway.StreamData{
		Route: gateway.RouteServerCreateGroup,
		Payload: map[string]string{
			"groupID":   groupID,
			"groupName": groupName,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) DeleteGroup(groupID string) {
	s.PushToServer(&gateway.StreamData{
		Route: gateway.RouteServerDeleteGroup,
		Payload: map[string]string{
			"groupID": groupID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) SendToGroup(groupID string, msg *gateway.StreamData) {
	pushMsg := &gateway.StreamData{
		Route:     gateway.RouteServerSendToGroup,
		Payload:   make(map[string]string),
		Timestamp: time.Now().UnixMilli(),
	}
	pushMsg.Payload["groupID"] = groupID
	if msg.Route != "" {
		pushMsg.Payload["route"] = msg.Route
	}
	if msg.Payload != nil {
		for k, v := range msg.Payload {
			pushMsg.Payload[k] = v
		}
	}
	s.PushToEachGateway(pushMsg)
}

func (s *Server) GetGroupInfo(groupID string) {
	s.PushToServer(&gateway.StreamData{
		Route: gateway.RouteServerGetGroupInfo,
		Payload: map[string]string{
			"groupID": groupID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}
