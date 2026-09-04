package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/codec"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	CmdLoginGate     int32 = 1000001
	CmdLoginGateAck  int32 = 1000002
	CmdLogicLoginReq int32 = 1100001
	CmdLogicLoginAck int32 = 1100002
	CmdUserOffline   int32 = 1100012
)

type Gateway struct {
	cfg      *config.Config
	sessions *SessionManager
	groups   *GroupManager
	grpcSrv  *GRPCServer
	stopCh   chan struct{}
	once     sync.Once
	wg       sync.WaitGroup

	connectionsTotal  atomic.Int64
	connectionsActive atomic.Int64
	messagesReceived  atomic.Int64
	messagesForwarded atomic.Int64
	messagesPushed    atomic.Int64
	messagesDropped   atomic.Int64

	transportType sync.Map
	statsServer   *http.Server
	enginesMu     sync.Mutex
	engines       []gnet.Engine
}

func NewGateway(cfg *config.Config) *Gateway {
	gw := &Gateway{
		cfg:      cfg,
		sessions: NewSessionManager(),
		groups:   NewGroupManager(),
		stopCh:   make(chan struct{}),
	}
	gw.grpcSrv = NewGRPCServer(gw)
	return gw
}

func (g *Gateway) StartServices() {
	addr := fmt.Sprintf(":%d", g.cfg.GRPC.Port)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.grpcSrv.Start(addr, g.cfg.GRPC.MaxMessageSize, g.cfg.GRPC.WindowSize)
	}()
	tlog.Info("gateway gRPC server started", "addr", addr)
	g.startDefaultObservability(g.cfg.PortAddress())
}

func (g *Gateway) Close() {
	g.once.Do(func() { close(g.stopCh) })
	if g.statsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = g.statsServer.Shutdown(ctx)
		cancel()
	}
	g.grpcSrv.Stop()
	g.enginesMu.Lock()
	engines := append([]gnet.Engine(nil), g.engines...)
	g.enginesMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, eng := range engines {
		_ = eng.Stop(ctx)
	}
	g.wg.Wait()
}

func (g *Gateway) SetTransportType(port, transportType string) {
	g.transportType.Store(port, transportType)
}

// ---- gnet.EventHandler ----

func (g *Gateway) OnBoot(eng gnet.Engine) gnet.Action {
	g.enginesMu.Lock()
	g.engines = append(g.engines, eng)
	g.enginesMu.Unlock()
	return gnet.None
}

func (g *Gateway) OnShutdown(eng gnet.Engine) {}

func (g *Gateway) OnInit() (options []gnet.Option, action gnet.Action) {
	return nil, gnet.None
}

func (g *Gateway) OnOpen(c gnet.Conn) (out []byte, action gnet.Action) {
	ip := c.RemoteAddr().(*net.TCPAddr).IP.String()
	sess := NewSession(c, ip)
	port := fmt.Sprintf("%d", c.LocalAddr().(*net.TCPAddr).Port)
	transportType, _ := g.transportType.Load(port)
	if transportType == "websocket" {
		sess.codec = codec.NewWebSocketCodecWithLimit(g.cfg.Protection.MaxWSFrameSize)
	} else {
		sess.codec = codec.NewTCPCodecWithLimit(g.cfg.Protection.MaxFrameSize)
	}
	g.sessions.Add(sess)
	g.connectionsTotal.Add(1)
	g.connectionsActive.Add(1)
	return nil, gnet.None
}

func (g *Gateway) OnClose(c gnet.Conn, err error) (action gnet.Action) {
	sess := g.sessions.GetByConn(c)
	if sess == nil {
		return gnet.None
	}
	if sess.IsAuthenticated() {
		g.grpcSrv.NotifyOffline(sess)
	}
	g.groups.RemoveSession(sess.ID())
	g.sessions.Remove(c)
	g.connectionsActive.Add(-1)
	return gnet.None
}

func (g *Gateway) OnTraffic(c gnet.Conn) (action gnet.Action) {
	sess := g.sessions.GetByConn(c)
	if sess == nil {
		return gnet.None
	}

	frames, err := sess.Codec().Decode(context.Background(), c)
	if err != nil {
		return gnet.Close
	}
	for _, frame := range frames {
		sess.Touch()
		g.messagesReceived.Add(1)
		g.handleFrame(sess, frame)
	}
	return gnet.None
}

func (g *Gateway) OnTick() (delay time.Duration, action gnet.Action) {
	if idle, err := time.ParseDuration(g.cfg.Protection.ConnIdleTimeout); err == nil && idle > 0 {
		now := time.Now()
		g.sessions.Range(func(sess *Session) bool {
			if sess.IdleFor(now) > idle {
				_ = sess.Conn().Close()
			}
			return true
		})
	}
	return 30 * time.Second, gnet.None
}

func (g *Gateway) handleFrame(sess *Session, frame []byte) {
	cmd, _, body, ok := ExtractMessageFrame(frame)
	if !ok {
		return
	}

	if cmd == CmdLoginGate {
		g.handleLoginGate(sess, body)
		return
	}

	if !sess.IsBound() {
		return
	}

	if g.grpcSrv.SendToLogic(sess, cmd, body) {
		g.messagesForwarded.Add(1)
	} else {
		g.messagesDropped.Add(1)
	}
}

func (g *Gateway) handleLoginGate(sess *Session, body []byte) {
	var req protoGw.LoginGateReq
	if err := proto.Unmarshal(body, &req); err != nil {
		return
	}

	server, ok := g.cfg.LogicServer(req.ServerId)
	if !ok {
		tlog.Warn("login gate: unknown server_id", "serverId", req.ServerId)
		return
	}

	if !g.grpcSrv.IsLogicConnected(req.ServerId) {
		if err := g.grpcSrv.ConnectLogic(req.ServerId, server.Address); err != nil {
			tlog.Error("login gate: connect logic failed", "serverId", req.ServerId, "error", err)
			return
		}
	}

	sess.Bind(req.ServerId, req.UserId)

	ack := &protoGw.LoginGateAck{
		Code:      0,
		Message:   "ok",
		SessionId: sess.ID(),
		ServerId:  req.ServerId,
	}
	data, _ := proto.Marshal(ack)
	g.sendToSession(sess, CmdLoginGateAck, data)
}

func (g *Gateway) SendToClient(sessionID string, cmd int32, data []byte) bool {
	sess := g.sessions.GetByID(sessionID)
	if sess == nil {
		return false
	}
	return g.sendToSession(sess, cmd, data)
}

func (g *Gateway) sendToSession(sess *Session, cmd int32, data []byte) bool {
	if err := sess.Conn().AsyncWrite(sess.Codec().Encode(EncodeMessageFrame(cmd, 0, data)), nil); err != nil {
		g.messagesDropped.Add(1)
		return false
	}
	g.messagesPushed.Add(1)
	return true
}

func (g *Gateway) Broadcast(cmd int32, data []byte) {
	g.sessions.Range(func(sess *Session) bool {
		g.sendToSession(sess, cmd, data)
		return true
	})
}

func (g *Gateway) SendToGroup(groupID string, cmd int32, data []byte, excludeSessionIDs ...string) {
	exclude := make(map[string]bool, len(excludeSessionIDs))
	for _, id := range excludeSessionIDs {
		exclude[id] = true
	}
	g.groups.RangeSessions(groupID, func(sess *Session) bool {
		if !exclude[sess.ID()] {
			g.sendToSession(sess, cmd, data)
		}
		return true
	})
}

func EncodeMessageFrame(cmd int32, seqID int64, body []byte) []byte {
	buf := make([]byte, 0, 4+len(body))
	buf = protowire.AppendTag(buf, 1, protowire.VarintType)
	buf = protowire.AppendVarint(buf, uint64(cmd))
	buf = protowire.AppendTag(buf, 2, protowire.VarintType)
	buf = protowire.AppendVarint(buf, uint64(seqID))
	buf = protowire.AppendTag(buf, 99, protowire.BytesType)
	buf = protowire.AppendBytes(buf, body)
	return buf
}

func ExtractMessageFrame(data []byte) (cmd int32, seqID int64, body []byte, ok bool) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, nil, false
		}
		data = data[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			cmd = int32(v)
			data = data[m:]
		case 2:
			if typ != protowire.VarintType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			seqID = int64(v)
			data = data[m:]
		case 99:
			if typ != protowire.BytesType {
				return 0, 0, nil, false
			}
			v, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return 0, 0, nil, false
			}
			body = v
			data = data[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, nil, false
			}
			data = data[m:]
		}
	}
	return cmd, seqID, body, cmd != 0 && len(body) > 0
}

func decodeVarintFast(data []byte) (uint64, int) {
	var result uint64
	var shift uint
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		result |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return result, i + 1
		}
		shift += 7
	}
	return 0, 0
}
