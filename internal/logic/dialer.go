package logic

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/bytedance/sonic"

	"github.com/streasure/protocol/enums"
	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

var errDialerStopped = errors.New("logic: gateway dialer stopped")

// sgateRegisterAddress 网关在 etcd 中注册的地址 JSON（与网关 buildRegisterAddress 对应）。
type sgateRegisterAddress struct {
	IP   string `json:"ip"`
	GRPC int    `json:"grpc"`
}

// gateLink 与单个网关 gRPC 地址的拨入链路。
type gateLink struct {
	addr   string
	mu     sync.Mutex
	stale  bool               // etcd 已注销：当前会话结束后不再重拨
	cancel context.CancelFunc // 取消当前拨入会话（Stop 时强制退出）
}

func (g *gateLink) markStale() {
	g.mu.Lock()
	g.stale = true
	g.mu.Unlock()
}

func (g *gateLink) isStale() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stale
}

func (g *gateLink) clearStale() {
	g.mu.Lock()
	g.stale = false
	g.mu.Unlock()
}

func (g *gateLink) setCancel(cancel context.CancelFunc) {
	g.mu.Lock()
	g.cancel = cancel
	g.mu.Unlock()
}

func (g *gateLink) takeCancel() context.CancelFunc {
	g.mu.Lock()
	c := g.cancel
	g.cancel = nil
	g.mu.Unlock()
	return c
}

// Dialer logic 主动拨入网关的连接管理器（flip 模式）。
// 通过 etcd 发现同 zone 网关（辅以静态地址），为每个网关建立
// connGroupCount 条独立 gRPC 连接 × shardCount 条流；断开后指数退避重拨。
// 连接可用性责任在本侧，网关只维护流附着状态。
type Dialer struct {
	server         *Server
	cfg            ServiceConfig
	shardCount     int
	connGroupCount int
	discovery      *uetcd.Component

	mu       sync.Mutex
	gates    map[string]*gateLink // key = 网关 gRPC 地址 (ip:port)
	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewDialer 创建拨入管理器，分片与连接组参数取配置（含默认值）。
func NewDialer(server *Server, cfg ServiceConfig) *Dialer {
	shardCount := cfg.ShardCount
	if shardCount <= 0 {
		shardCount = runtime.NumCPU() * 8
	}
	connGroupCount := cfg.ConnGroupCount
	if connGroupCount <= 0 {
		connGroupCount = 4
	}
	return &Dialer{
		server:         server,
		cfg:            cfg,
		shardCount:     shardCount,
		connGroupCount: connGroupCount,
		gates:          make(map[string]*gateLink),
		stopCh:         make(chan struct{}),
	}
}

// Start 启动静态地址拨入与同 zone 网关发现。
func (d *Dialer) Start() error {
	for _, addr := range d.cfg.Gateways {
		if addr != "" {
			d.ensureGate(addr)
		}
	}
	d.startDiscovery()
	return nil
}

// startDiscovery 监听同 zone 网关注册（{belong}/SGATE:{zone}）。
// 任一环节失败仅降级为静态地址模式，不阻塞服务启动。
func (d *Dialer) startDiscovery() {
	endpoints := d.cfg.EtcdEndpoints
	if len(endpoints) == 0 && d.cfg.EtcdEndpoint != "" {
		endpoints = []string{d.cfg.EtcdEndpoint}
	}
	if len(endpoints) == 0 {
		tlog.Warn(context.TODO(), "gateway discovery disabled (no etcd), dialing static gateways only count=%d", len(d.cfg.Gateways))
		return
	}
	belong := d.cfg.Belong
	if belong == "" {
		belong = "default"
	}
	zone := d.cfg.Zone
	if zone == "" {
		zone = "default"
	}
	sgateType := enums.ServerType_name[int32(enums.ServerType_SERVER_TYPE_SGATE)]
	serviceID := belong + "/" + sgateType + ":" + zone

	comp := uetcd.New(uetcd.ComponentConfig{
		Etcd: uetcd.Config{
			Endpoints:     endpoints,
			Endpoint:      d.cfg.EtcdEndpoint,
			Username:      d.cfg.EtcdUsername,
			Password:      d.cfg.EtcdPassword,
			ServicePrefix: d.cfg.EtcdServicePrefix,
		},
		Discovery: uetcd.DiscoveryConfig{ServiceID: serviceID},
	})
	comp.OnServiceChange(d.handleGateEvent)
	if err := comp.Start(); err != nil {
		tlog.Warn(context.TODO(), "gateway discovery start failed, static gateways only serviceID=%s error=%v", serviceID, err)
		return
	}

	d.mu.Lock()
	d.discovery = comp
	d.mu.Unlock()

	// 重放已知网关，避免 discovery 启动先于回调注册而丢失初始快照。
	knownGateways := 0
	for _, address := range comp.ServiceSet() {
		d.handleGateEvent(uetcd.ServiceEvent{Type: uetcd.EventRegister, ServiceID: serviceID, Address: address})
		knownGateways++
	}
	tlog.Info(context.TODO(), "gateway discovery started serviceID=%s knownGateways=%d", serviceID, knownGateways)
}

// handleGateEvent 处理网关注册/注销事件。
// 注销采用保守策略：不断开存活会话，仅标记 stale，会话自然结束。
func (d *Dialer) handleGateEvent(event uetcd.ServiceEvent) {
	addr, ok := parseGatewayAddress(event.Address)
	if !ok {
		tlog.Warn(context.TODO(), "ignoring unparsable gateway address value=%s", event.Address)
		return
	}
	switch event.Type {
	case uetcd.EventRegister:
		d.ensureGate(addr)
	case uetcd.EventDeregister:
		d.markGateStale(addr)
	}
}

// parseGatewayAddress 解析网关注册地址 JSON {ip, grpc, ...}；容忍裸 host:port。
func parseGatewayAddress(raw string) (string, bool) {
	var reg sgateRegisterAddress
	if err := json.Unmarshal([]byte(raw), &reg); err == nil {
		if reg.IP == "" || reg.GRPC <= 0 {
			return "", false
		}
		return net.JoinHostPort(reg.IP, strconv.Itoa(reg.GRPC)), true
	}
	if strings.Contains(raw, ":") {
		return raw, true
	}
	return "", false
}

// ensureGate 确保目标地址存在拨入循环（幂等；重新注册时清除 stale 标记）。
func (d *Dialer) ensureGate(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.stopCh:
		return
	default:
	}
	if g, ok := d.gates[addr]; ok {
		g.clearStale()
		return
	}
	g := &gateLink{addr: addr}
	d.gates[addr] = g
	d.wg.Go(func() { d.runGate(g) })
}

// markGateStale 标记网关已注销；存活会话结束后不再重拨。
func (d *Dialer) markGateStale(addr string) {
	d.mu.Lock()
	g := d.gates[addr]
	d.mu.Unlock()
	if g == nil {
		return
	}
	g.markStale()
	tlog.Warn(context.TODO(), "gateway deregistered, will stop reconnecting after current session ends addr=%s", addr)
}

// runGate 单网关的会话循环：拨入 → 运行流 → 断开退避 → 重拨。
func (d *Dialer) runGate(g *gateLink) {
	defer d.removeGate(g)

	interval := time.Second
	for {
		select {
		case <-d.stopCh:
			return
		default:
		}
		if g.isStale() {
			tlog.Info(context.TODO(), "gateway link removed (deregistered) addr=%s", g.addr)
			return
		}

		started := time.Now()
		err := d.runSession(g)
		if err == errDialerStopped {
			return
		}

		select {
		case <-d.stopCh:
			return
		default:
		}

		// 会话存活足够久：断开更可能是瞬时故障，重置退避。
		if time.Since(started) > time.Minute {
			interval = time.Second
		}
		tlog.Warn(context.TODO(), "gateway session ended, redialing addr=%s nextRetry=%s error=%v", g.addr, interval, err)

		select {
		case <-d.stopCh:
			return
		case <-time.After(interval):
		}
		interval = min(interval*2, 30*time.Second)
	}
}

// runSession 建立一条完整的拨入会话并运行至任一流退出。
// 连接组与流全部建立成功才算就绪（与旧网关侧 doConnect 对称）；
// 返回错误表示拨入失败或会话中断，由调用方退避后重拨。
func (d *Dialer) runSession(g *gateLink) error {
	ctx, cancel := context.WithCancel(context.Background())
	g.setCancel(cancel)
	defer func() {
		cancel()
		g.setCancel(nil)
	}()

	windowSize := int32(d.cfg.GRPCWindowSize)
	if windowSize <= 0 {
		windowSize = 524288
	}
	maxMsgSize := d.cfg.GRPCMaxMessageSize
	if maxMsgSize <= 0 {
		maxMsgSize = 4 * 1024 * 1024
	}

	conns := make([]*grpc.ClientConn, d.connGroupCount)
	clients := make([]protocol.GatewayStreamClient, d.connGroupCount)
	for i := range conns {
		conn, err := grpc.NewClient(g.addr,
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
			for j := range i {
				conns[j].Close()
			}
			return err
		}
		conns[i] = conn
		clients[i] = protocol.NewGatewayStreamClient(conn)
	}
	defer func() {
		for _, conn := range conns {
			conn.Close()
		}
	}()

	// 建立全部流：任一失败整体作废，退避后重拨。
	streams := make([]protocol.GatewayStream_OnDataClient, d.shardCount)
	var establish sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for i := range d.shardCount {
		idx := i
		establish.Go(func() {
			sctx := routes.AppendLogicStreamMetadata(ctx, routes.LogicStreamMeta{
				LogicID:    d.cfg.ServiceID,
				Zone:       d.cfg.Zone,
				Addr:       d.cfg.AdvertiseAddr,
				ShardIdx:   idx,
				ShardCount: d.shardCount,
			})
			stream, err := clients[idx%d.connGroupCount].OnData(sctx)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			streams[idx] = stream
		})
	}
	establish.Wait()
	if firstErr != nil {
		for _, stream := range streams {
			if stream != nil {
				_ = stream.CloseSend()
			}
		}
		return firstErr
	}
	tlog.Info(context.TODO(), "gateway dial session established addr=%s shards=%d connGroups=%d",
		g.addr, d.shardCount, d.connGroupCount)

	// 运行各流的收发循环；任一流退出即结束整个会话。
	exitCh := make(chan struct{}, len(streams))
	var handlers sync.WaitGroup
	for i := range streams {
		idx := i
		handlers.Go(func() {
			defer func() { exitCh <- struct{}{} }()
			if err := d.server.handleStream(streams[idx], g.addr); err != nil {
				tlog.Info(context.TODO(), "gateway stream exited addr=%s shard=%d error=%v", g.addr, idx, err)
			}
		})
	}
	<-exitCh
	cancel()
	handlers.Wait()

	select {
	case <-d.stopCh:
		return errDialerStopped
	default:
	}
	return nil
}

// removeGate 从映射中移除链路（会话循环退出时调用）。
func (d *Dialer) removeGate(g *gateLink) {
	d.mu.Lock()
	if d.gates[g.addr] == g {
		delete(d.gates, g.addr)
	}
	d.mu.Unlock()
}

// Stop 停止拨入：关闭全部会话、等待循环退出、销毁发现组件。
func (d *Dialer) Stop() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		close(d.stopCh)
		for _, g := range d.gates {
			if cancel := g.takeCancel(); cancel != nil {
				cancel()
			}
		}
		d.mu.Unlock()
	})
	d.wg.Wait()

	d.mu.Lock()
	discovery := d.discovery
	d.discovery = nil
	d.mu.Unlock()
	if discovery != nil {
		discovery.Destroy()
	}
}
