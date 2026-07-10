package gateway

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cast"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/metrics"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type LogicClientProvider interface {
	IsConnected() bool
	SendMessage(msg *protobuf.Message) error
}

var (
	frameHeaderPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 4)
			return &buf
		},
	}

	logicProtoMsgPool = sync.Pool{
		New: func() interface{} {
			return &protobuf.Message{
				Payload: make(map[string]string, 8),
			}
		},
	}
)

func resetLogicProtoMsg(msg *protobuf.Message) {
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Cmd = 0
	msg.Data = nil
	msg.Timestamp = 0
	msg.Sequence = 0
	msg.ProtocolVersion = ""
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
}

func extractRouteFast(data []byte) string {
	route, _ := extractRouteAndCmd(data)
	return route
}

func extractCmdFast(data []byte) int32 {
	_, cmd := extractRouteAndCmd(data)
	return cmd
}

// extractRouteAndCmd parses protobuf data once to extract both route (field 3)
// and cmd (field 4) in a single pass, reducing CPU overhead.
func extractRouteAndCmd(data []byte) (route string, cmd int32) {
	offset := 0
	for offset < len(data) {
		b := data[offset]
		if b < 0x80 {
			offset++
			fieldNum := int(b >> 3)
			wireType := int(b & 0x7)

			switch wireType {
			case 0:
				if fieldNum == 4 {
					v, n := decodeVarint(data[offset:])
					if n > 0 {
						cmd = int32(v)
					}
				}
				for offset < len(data) && data[offset] >= 0x80 {
					offset++
				}
				if offset < len(data) {
					offset++
				}
			case 1:
				offset += 8
			case 2:
				if offset >= len(data) {
					return
				}
				l := int(data[offset])
				offset++
				if l >= 0x80 {
					if offset >= len(data) {
						return
					}
					l2 := int(data[offset])
					offset++
					l = (l & 0x7F) | (l2 << 7)
				}
				if fieldNum == 3 && l > 0 && l < 256 && offset+l <= len(data) {
					route = string(data[offset : offset+l])
				}
				offset += l
			case 5:
				offset += 4
			default:
				return
			}
		} else {
			offset++
		}
	}
	return
}

func decodeVarint(buf []byte) (uint64, int) {
	var x uint64
	var s uint
	for i := 0; i < len(buf) && i < 10; i++ {
		b := buf[i]
		if b < 0x80 {
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0
}

type Gateway struct {
	connectionManager  *ConnectionManager
	stopChan           chan struct{}
	closeOnce          sync.Once
	metrics            *metrics.Metrics
	transportType      map[string]string
	ctx                context.Context
	tlsConfig          *tls.Config
	clusterID          string
	isLeader           bool
	bufferPool         *sync.Pool
	minBufferSize      int
	maxBufferSize      int
	defaultBufferSize  int
	cfg                atomic.Value
	wsConnections      sync.Map
	configPath         string
	configUpdateChan   chan *config.Config
	messageIntegrity   *MessageIntegrity
	versionNegotiation *VersionNegotiation
	tracer             *Tracer
	logicClient        *LogicClient
	logicClientPool    *LogicClientPool
	serviceDiscovery   *ServiceDiscovery
	redisClient        *redis.Client
	overloadProtector  *OverloadProtector
	grpcServer         *grpc.Server
	promServer         *http.Server
	statsServer        *http.Server
	zone               string
	protection         config.ProtectionConfig
	grpcCfg            config.GRPCConfig
	streamCfg          config.StreamConfig

	// 转发统计计数器（用于极限压测时观测 sgate 转发能力）
	messagesForwarded                  atomic.Int64
	messagesDroppedOverload            atomic.Int64
	messagesDroppedFull                atomic.Int64
	messagesDroppedNoLogic             atomic.Int64
	messagesDroppedNoLogicNotConnected atomic.Int64
	messagesReceived                   atomic.Int64
	messagesPushedToClient             atomic.Int64
	messagesPushDroppedNoConn          atomic.Int64
}

func (g *Gateway) SetTransportType(port string, transportType string) {
	g.transportType[port] = transportType
}

// AddPushedToClient 增加已推送到客户端的消息计数（接收方向：logic->sgate->client）
func (g *Gateway) AddPushedToClient(n int64) {
	g.messagesPushedToClient.Add(n)
}

// AddPushDroppedNoConn 增加因无连接而丢弃的推送计数
func (g *Gateway) AddPushDroppedNoConn(n int64) {
	g.messagesPushDroppedNoConn.Add(n)
}

func (g *Gateway) GetBuffer(size int) []byte {
	buf := g.bufferPool.Get().([]byte)
	if cap(buf) < size {
		newSize := cap(buf) * 2
		if newSize < size {
			newSize = size
		}
		if newSize > g.maxBufferSize {
			newSize = g.maxBufferSize
		}
		newBuf := make([]byte, newSize)
		g.bufferPool.Put(buf)
		return newBuf
	}
	return buf[:cap(buf)]
}

func (g *Gateway) PutBuffer(buf []byte) {
	if cap(buf) >= g.minBufferSize && cap(buf) <= g.maxBufferSize {
		buf = buf[:cap(buf)]
		g.bufferPool.Put(buf)
	}
}

var protobufMessagePool = sync.Pool{
	New: func() interface{} {
		return &protobuf.Message{
			Payload: make(map[string]string, 32),
		}
	},
}

const preallocatedProtobufMessages = 64

func init() {
	for i := 0; i < preallocatedProtobufMessages; i++ {
		protobufMessagePool.Put(&protobuf.Message{
			Payload: make(map[string]string, 32),
		})
	}
}

func GetProtobufMessage() *protobuf.Message {
	msg := protobufMessagePool.Get().(*protobuf.Message)
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	} else {
		msg.Payload = make(map[string]string, 32)
	}
	return msg
}

func PutProtobufMessage(msg *protobuf.Message) {
	if msg == nil {
		return
	}
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	msg.Sequence = 0
	msg.Timestamp = 0
	msg.ProtocolVersion = ""
	msg.Cmd = 0
	msg.Data = nil
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
	protobufMessagePool.Put(msg)
}

var frameDataPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 4096)
		return &buf
	},
}

func NewGateway() *Gateway {
	ctx := context.Background()

	cfg, err := config.LoadConfig()
	if err != nil {
		tlog.Warn("load config failed, using defaults", "error", err)
	}

	switch cfg.LogLevel {
	case "debug":
		tlog.SetLevel("debug")
	case "info":
		tlog.SetLevel("info")
	case "warn":
		tlog.SetLevel("warn")
	case "error":
		tlog.SetLevel("error")
	}

	protection := cfg.Protection
	if protection.MaxFrameSize <= 0 {
		protection.MaxFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxFrameBufSize <= 0 {
		protection.MaxFrameBufSize = 4 * 1024 * 1024
	}
	if protection.MaxWSFrameSize <= 0 {
		protection.MaxWSFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxWSBufferSize <= 0 {
		protection.MaxWSBufferSize = 4 * 1024 * 1024
	}
	if protection.WSHeartbeatTimeout <= 0 {
		protection.WSHeartbeatTimeout = 60
	}
	if protection.WSCheckInterval <= 0 {
		protection.WSCheckInterval = 30
	}

	grpcCfg := cfg.GRPC
	if grpcCfg.Port <= 0 {
		grpcCfg.Port = 50051
	}
	if grpcCfg.WindowSize <= 0 {
		grpcCfg.WindowSize = 524288
	}
	if grpcCfg.MaxMessageSize <= 0 {
		grpcCfg.MaxMessageSize = 4 * 1024 * 1024
	}

	streamCfg := cfg.Stream
	if streamCfg.SendChannelSize <= 0 {
		streamCfg.SendChannelSize = 65536
	}
	if streamCfg.ReceiveBatchSize <= 0 {
		streamCfg.ReceiveBatchSize = 64
	}

	gw := &Gateway{
		connectionManager: NewConnectionManager(),
		stopChan:          make(chan struct{}),
		metrics:           metrics.NewMetrics(),
		transportType:     make(map[string]string),
		ctx:               ctx,
		tlsConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS13,
		},
		clusterID:         "sgate-cluster",
		isLeader:          false,
		minBufferSize:     4096,
		maxBufferSize:     65536,
		defaultBufferSize: 16384,
		bufferPool: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 16384)
			},
		},
		configUpdateChan:  make(chan *config.Config),
		overloadProtector: NewOverloadProtector(protection),
		logicClient:       NewLogicClient(GatewayInterface(nil)),
		protection:        protection,
		grpcCfg:           grpcCfg,
		streamCfg:         streamCfg,
	}

	gw.cfg.Store(cfg)

	gw.overloadProtector.Start()

	go gw.wsHeartbeatChecker()

	gw.messageIntegrity = NewMessageIntegrity(30000)

	supportedVersions := []string{"1.0.0", "1.1.0", "2.0.0"}
	gw.versionNegotiation = NewVersionNegotiation(supportedVersions, 10*time.Second)

	gw.tracer = NewTracer(5 * time.Minute)

	connCheckInterval, _ := time.ParseDuration(protection.ConnCheckInterval)
	if connCheckInterval <= 0 {
		connCheckInterval = 5 * time.Minute
	}
	connIdleTimeout, _ := time.ParseDuration(protection.ConnIdleTimeout)
	if connIdleTimeout <= 0 {
		connIdleTimeout = 30 * time.Second
	}
	gw.connectionManager.StartConnectionChecker(connCheckInterval, connIdleTimeout)

	go gw.configWatcher()

	go func() {
		for {
			select {
			case <-gw.stopChan:
				return
			case newCfg := <-gw.configUpdateChan:
				gw.handleConfigUpdate(newCfg)
			}
		}
	}()

	gw.logicClient.gateway = gw

	gw.logicClientPool = NewLogicClientPool(gw)

	if cfg.Zone == "" {
		cfg.Zone = "default"
	}
	gw.zone = cfg.Zone

	tlog.Info("gateway zone configured", "zone", gw.zone)

	if cfg.Discovery.Enabled && cfg.Redis.Addr != "" {
		gw.redisClient = redis.NewClient(&redis.Options{
			Addr:         cfg.Redis.Addr,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			PoolSize:     cfg.Redis.PoolSize,
			MinIdleConns: cfg.Redis.MinIdleConns,
		})

		ctx := context.Background()
		if err := gw.redisClient.Ping(ctx).Err(); err != nil {
			tlog.Warn("Redis connection failed, service discovery disabled", "error", err)
			gw.redisClient = nil
		} else {
			tlog.Info("Redis connected, starting service discovery", "addr", cfg.Redis.Addr)

			gw.serviceDiscovery = NewServiceDiscovery(gw.redisClient, cfg.Discovery)
			gw.logicClientPool.SetDiscovery(gw.serviceDiscovery)

			if err := gw.serviceDiscovery.Start(); err != nil {
				tlog.Error("service discovery start failed", "error", err)
			}
		}
	}

	if gw.serviceDiscovery == nil {
		tlog.Info("service discovery disabled, using static logic server connection")
		go func() {
			select {
			case <-time.After(2 * time.Second):
				tlog.Info("connecting to logic server", "address", "localhost:50052")
				if err := gw.logicClient.Connect("localhost:50052"); err != nil {
					tlog.Error("failed to connect to logic server", "error", err)
				} else {
					tlog.Info("successfully connected to logic server")
				}
			case <-gw.stopChan:
				return
			}
		}()
	}

	grpcPort := fmt.Sprintf(":%d", grpcCfg.Port)
	tlog.Info("about to start gRPC server", "port", grpcPort)
	go func() {
		tlog.Info("starting gRPC server", "port", grpcPort)
		if server, err := StartGRPCServer(gw, grpcPort, gw.grpcCfg.MaxMessageSize, gw.grpcCfg.WindowSize); err != nil {
			tlog.Error("failed to start gRPC server", "error", err)
		} else {
			gw.grpcServer = server
			tlog.Info("gRPC server started", "port", grpcPort)
		}
	}()

	// 启动统计 HTTP 服务（供压测工具查询转发计数）
	gw.StartStatsServer(":9091")

	return gw
}

func (g *Gateway) wsHeartbeatChecker() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("wsHeartbeatChecker panic recovered", "error", r)
		}
	}()
	checkInterval := time.Duration(g.protection.WSCheckInterval) * time.Second
	heartbeatTimeout := time.Duration(g.protection.WSHeartbeatTimeout) * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			g.checkWebSocketConnections(heartbeatTimeout)
		}
	}
}

func (g *Gateway) checkWebSocketConnections(timeout time.Duration) {
	var connections []*WebSocketConnection
	g.wsConnections.Range(func(key, value interface{}) bool {
		if conn, ok := key.(*WebSocketConnection); ok {
			connections = append(connections, conn)
		}
		return true
	})

	for _, conn := range connections {
		if time.Since(conn.LastPingTime) > timeout {
			tlog.Warn("WebSocket connection timeout, closing", "connectionID", conn.ConnectionID)
			if conn.Conn != nil {
				conn.Conn.Close()
			}
			if conn.ConnectionID != "" {
				g.connectionManager.RemoveConnection(conn.ConnectionID)
			}
			g.wsConnections.Delete(conn)
			wsConnectionPool.Put(conn)
		}
	}
}

func (g *Gateway) configWatcher() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("configWatcher panic recovered", "error", r)
		}
	}()
	if _, err := os.Stat(g.configPath); os.IsNotExist(err) {
		altPaths := []string{"../config/config.yaml", "../../config/config.yaml"}
		found := false
		for _, path := range altPaths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				g.configPath = path
				found = true
				break
			}
		}
		if !found {
			return
		}
	}

	fileInfo, err := os.Stat(g.configPath)
	if err != nil {
		return
	}

	lastModTime := fileInfo.ModTime()

	for {
		select {
		case <-g.stopChan:
			return
		default:
			fileInfo, err := os.Stat(g.configPath)
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}

			if fileInfo.ModTime() != lastModTime {
				lastModTime = fileInfo.ModTime()
				newCfg, err := config.LoadConfig()
				if err != nil {
					time.Sleep(5 * time.Second)
					continue
				}
				select {
				case g.configUpdateChan <- newCfg:
				default:
				}
			}

			time.Sleep(5 * time.Second)
		}
	}
}

func (g *Gateway) handleConfigUpdate(newCfg *config.Config) {
	g.cfg.Store(newCfg)
	tlog.Info("config updated")
}

var connContextPool = sync.Pool{
	New: func() interface{} {
		return &ConnContext{
			ConnectionID: "",
			FrameBuf:     nil,
		}
	},
}

func GetConnContext() *ConnContext {
	ctx := connContextPool.Get().(*ConnContext)
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	return ctx
}

func PutConnContext(ctx *ConnContext) {
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	connContextPool.Put(ctx)
}

type ConnContext struct {
	ConnectionID string
	FrameBuf     []byte
	FrameOff     int
}

func (g *Gateway) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnOpen panic recovered", "error", r)
			action = gnet.Close
		}
	}()

	localAddr := c.LocalAddr().String()
	isWS := false
	for port, t := range g.transportType {
		if strings.HasSuffix(localAddr, ":"+port) && t == "websocket" {
			isWS = true
			break
		}
	}

	if isWS {
		wsConn := NewWebSocketConnection(c)
		c.SetContext(wsConn)
		g.wsConnections.Store(wsConn, true)
	} else {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID := g.connectionManager.AddConnection(c, tempUserUUID)
		connCtx := GetConnContext()
		connCtx.ConnectionID = connectionID
		c.SetContext(connCtx)
	}

	g.metrics.IncConnectionsTotal()
	g.metrics.IncConnectionsActive()

	tlog.Debug("new connection", "localAddr", localAddr, "isWS", isWS)
	return
}

func (g *Gateway) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnClose panic recovered", "error", r)
		}
	}()

	var connectionID string
	connCtx := c.Context()

	if connCtx != nil {
		if ctx, ok := connCtx.(*ConnContext); ok {
			connectionID = ctx.ConnectionID
			PutConnContext(ctx)
		} else if wsConn, ok := connCtx.(*WebSocketConnection); ok {
			connectionID = wsConn.ConnectionID
			g.wsConnections.Delete(wsConn)
			wsConn.Buffer = nil
			wsConn.ConnectionID = ""
			wsConn.Conn = nil
			atomic.StoreInt32(&wsConn.State, int32(WSStateClosed))
			wsConnectionPool.Put(wsConn)
		} else if id, ok := connCtx.(string); ok {
			connectionID = id
		}
	}

	if connectionID != "" {
		g.connectionManager.RemoveConnection(connectionID)
		g.versionNegotiation.RemoveClientVersion(connectionID)
		g.metrics.DecConnectionsActive()
		tlog.Debug("connection closed", "connectionID", connectionID, "error", err)
	}

	return
}

func (g *Gateway) OnTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("OnTraffic panic recovered", "error", fmt.Sprintf("%v", r))
			action = gnet.Close
		}
	}()

	return g.handleNormalTraffic(c)
}

func (g *Gateway) handleNormalTraffic(c gnet.Conn) (action gnet.Action) {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error("handleNormalTraffic panic recovered", "error", cast.ToString(r))
			action = gnet.Close
		}
	}()

	data, err := c.Next(-1)
	if err != nil {
		return gnet.Close
	}

	connCtx := c.Context()
	if connCtx == nil {
		return gnet.Close

	}
	if wsConn, ok := connCtx.(*WebSocketConnection); ok {
		return g.handleWebSocketMessage(wsConn, data)
	}

	ctx, ok := connCtx.(*ConnContext)
	if !ok {
		if len(data) > 3 && data[0] == 'G' && data[1] == 'E' && data[2] == 'T' {
			wsConn := NewWebSocketConnection(c)
			c.SetContext(wsConn)
			g.wsConnections.Store(wsConn, wsConn)
			return g.handleWebSocketMessage(wsConn, data)
		}
		return gnet.Close
	}

	maxFrameBuf := g.protection.MaxFrameBufSize
	if len(ctx.FrameBuf)+len(data) > maxFrameBuf {
		ctx.FrameBuf = nil
		ctx.FrameOff = 0
		return gnet.Close
	}

	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	maxFrame := g.protection.MaxFrameSize
	for len(ctx.FrameBuf) >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[:4])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			ctx.FrameBuf = nil
			ctx.FrameOff = 0
			return gnet.Close
		}
		totalLen := 4 + int(frameLen)
		if len(ctx.FrameBuf) < totalLen {
			return
		}

		frameData := ctx.FrameBuf[4:totalLen]

		frameCopyPtr := frameDataPool.Get().(*[]byte)
		*frameCopyPtr = (*frameCopyPtr)[:0]
		*frameCopyPtr = append(*frameCopyPtr, frameData...)

		if len(ctx.FrameBuf) > totalLen {
			ctx.FrameBuf = ctx.FrameBuf[totalLen:]
		} else {
			ctx.FrameBuf = nil
		}
		ctx.FrameOff = 0

		ret := g.handleTCPRequest(c, *frameCopyPtr)
		*frameCopyPtr = (*frameCopyPtr)[:0]
		frameDataPool.Put(frameCopyPtr)

		if ret == gnet.Close {
			return gnet.Close
		}
	}

	return
}

func (g *Gateway) isLogicConnected() bool {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return true
	}
	if g.logicClient != nil && g.logicClient.IsConnected() {
		return true
	}
	return false
}

func (g *Gateway) getLogicClient() LogicClientProvider {
	if g.logicClientPool != nil && g.logicClientPool.IsConnected() {
		return g.logicClientPool
	}
	if g.logicClient != nil && g.logicClient.IsConnected() {
		return g.logicClient
	}
	return nil
}

func (g *Gateway) handleTCPRequest(c gnet.Conn, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		return
	}

	g.messagesReceived.Add(1)

	if g.overloadProtector.IsOverloaded() {
		g.overloadProtector.RecordDrop(1)
		g.messagesDroppedOverload.Add(1)
		return
	}

	var connectionID string
	connCtx := c.Context()
	if ctx, ok := connCtx.(*ConnContext); ok {
		connectionID = ctx.ConnectionID
	} else if id, ok := connCtx.(string); ok {
		connectionID = id
	} else {
		tempUserUUID := "temp_" + generateConnectionID()
		connectionID = g.connectionManager.AddConnection(c, tempUserUUID)
		c.SetContext(&ConnContext{
			ConnectionID: connectionID,
			FrameBuf:     nil,
		})
	}

	logicClient := g.getLogicClient()
	if logicClient != nil {
		route, cmd := extractRouteAndCmd(data)
		if route == protobuf.RouteHandshake {
			message := GetProtobufMessage()
			defer PutProtobufMessage(message)
			if err := proto.Unmarshal(data, message); err != nil {
				return
			}
			return g.handleHandshake(c, connectionID, message)
		}

		// 注意: 不能使用对象池复用 protoMsg，因为 SendMessage 是异步的
		// (msg 指针放入 sendCh 后由 startSendLoop 异步发送)
		// 如果在放入 sendCh 后 reset/put，会导致 use-after-reset 数据竞争
		protoMsg := &protobuf.Message{
			ConnectionId: connectionID,
			Route:        route,
			Data:         data,
		}

		if cmd > 0 {
			protoMsg.Cmd = cmd
		}

		if err := logicClient.SendMessage(protoMsg); err != nil {
			g.messagesDroppedFull.Add(1)
		} else {
			g.messagesForwarded.Add(1)
		}
		return
	}

	g.messagesDroppedNoLogicNotConnected.Add(1)
	return
}

func (g *Gateway) handleHandshake(c gnet.Conn, connectionID string, message *protobuf.Message) gnet.Action {
	handshakeDataStr := message.Payload["handshake_data"]
	var handshakeBytes []byte

	decoded, err := base64.StdEncoding.DecodeString(handshakeDataStr)
	if err == nil {
		handshakeBytes = decoded
	} else {
		handshakeBytes = []byte(handshakeDataStr)
	}

	handshake := &protobuf.Handshake{}
	if err := proto.Unmarshal(handshakeBytes, handshake); err != nil {
		return gnet.None
	}

	negotiatedVersion, err := g.versionNegotiation.ProcessHandshake(connectionID, handshake)
	if err != nil {
		return gnet.None
	}

	response := g.versionNegotiation.GenerateHandshakeResponse(negotiatedVersion)
	g.messageIntegrity.PrepareMessage(response)
	writeMsgFrame(c, response)

	if serverID := message.Payload["serverId"]; serverID != "" {
		g.connectionManager.SetConnectionServerID(connectionID, serverID)
	}

	return gnet.None
}

func writeFrame(c gnet.Conn, data []byte) {
	headerPtr := frameHeaderPool.Get().(*[]byte)
	binary.BigEndian.PutUint32(*headerPtr, uint32(len(data)))
	c.Writev([][]byte{*headerPtr, data})
	frameHeaderPool.Put(headerPtr)
}

func writeErrorFrame(c gnet.Conn, errMsg *protobuf.ErrorResponse) {
	data, _ := proto.Marshal(errMsg)
	writeFrame(c, data)
}

func writeMsgFrame(c gnet.Conn, msg *protobuf.Message) {
	data, _ := proto.Marshal(msg)
	writeFrame(c, data)
}

func (g *Gateway) OnBoot(engine gnet.Engine) (action gnet.Action) {
	return
}

func (g *Gateway) GetVersion() string {
	return "1.0.0"
}

func (g *Gateway) GetConnectionManager() *ConnectionManager {
	return g.connectionManager
}

func (g *Gateway) GetGRPCConfig() config.GRPCConfig {
	return g.grpcCfg
}

func (g *Gateway) GetStreamConfig() config.StreamConfig {
	return g.streamCfg
}

func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	g.metrics.LogMetrics()
	return 1 * time.Second, gnet.None
}

func (g *Gateway) OnShutdown(engine gnet.Engine) {
	g.Close()
}

func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		close(g.stopChan)

		if g.serviceDiscovery != nil {
			g.serviceDiscovery.Stop()
		}

		if g.logicClientPool != nil {
			g.logicClientPool.Close()
		}

		if g.grpcServer != nil {
			stopped := make(chan struct{})
			go func() {
				g.grpcServer.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				g.grpcServer.Stop()
			}
		}

		if g.redisClient != nil {
			g.redisClient.Close()
		}

		g.connectionManager.StopConnectionChecker()
		g.connectionManager.CloseAllConnections()

		g.overloadProtector.Stop()
		g.tracer.Stop()

		g.StopPrometheusMetrics()
		g.StopStatsServer()

		tlog.Info("gateway closed")
	})
}
