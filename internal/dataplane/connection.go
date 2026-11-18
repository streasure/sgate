package dataplane

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
)

type ConnState int32

const (
	StateForward ConnState = 0 // 转发状态，连接正常可转发消息
	StateClosed  ConnState = 1 // 已关闭状态
)

// Connection 表示客户端与网关之间的连接，封装了连接状态、用户信息和发送逻辑。
type Connection struct {
	id         string
	Conn       gnet.Conn
	CreatedAt  int64
	LastActive atomic.Int64
	// remoteAddr 对端地址（ip:port）：SetRemoteHost（反代改写）在事件循环写，
	// idle-checker/RemoveConnection 在检查协程读——atomic.Pointer 防 string 撕裂竞态。
	remoteAddr  atomic.Pointer[string]
	activitySeq atomic.Uint32
	Status      int8
	state       atomic.Int32

	// 原子字段，用于无锁并发访问
	userUUID    atomic.Pointer[string]              // 保存用户唯一标识。
	serverID    atomic.Pointer[string]              // 保存绑定的逻辑服标识。
	accountID   atomic.Pointer[string]              // 登录账号（=LoginGate userId；批量踢账号索引用）
	loginKey    atomic.Pointer[string]              // LoginGate loginKey（校验通过后保存；周期 BatchValidate 安全网重校验用，不落盘不转发）
	logicClient atomic.Pointer[LogicClientProvider] // LogicClientProvider 缓存，避免每条消息查询连接池
	udp         atomic.Pointer[UDPDownlink]         // UDP 下行通道（ASSOCIATE 成功注入；nil=回落 WS）
	jwtJti      atomic.Pointer[string]              // string：JWT jti（logout/封禁时撤销）
	jwtExp      atomic.Int64                        // JWT exp（Unix 秒；0=未知）
	offlineSent atomic.Bool                         // CmdUserOffline 已发送标记（防显式通知+OnClose 双发）
	remoteHost  atomic.Pointer[string]              // 去端口对端 IP（每连接缓存一次，热路径免 SplitHostPort）

	// 协议标识：建连时绑定（TCP=false / WS=true），此后只读，无竞态。
	isWebSocket bool

	// 连接级流控
	msgRateMu      sync.Mutex // 保护 msgWindowStart 和 msgCount 的原子更新
	msgCount       int64      // 当前窗口消息计数
	msgWindowStart int64      // 当前窗口起始时间（UnixMilli）

	// 连接级写合并：每连接独立 buffer，addMulti 只需 per-Connection 锁（几乎无竞争）
	coalescedMu     sync.Mutex
	coalescedData   []byte  // [4-byte len][payload] 累积格式
	coalescedBufPtr *[]byte // 指向 coalescerBufPool 的 buffer，用于归还
	coalescedCount  int     // 累积消息数

	// 下行写背压：已入队 gnet 异步写、尚未完成回调的本连接字节数
	//（reserveWrite/writeDone 维护，与全局 pendingWriteTotal 成对）。
	pendingWriteBytes atomic.Int64
}

// newConnection 创建新的连接对象，初始化基本属性和状态。
// framer 在建连时绑定协议身份（TCP/WS），此后只读，无晚置切换竞态。
func newConnection(id string, conn gnet.Conn, userUUID, remoteAddr string, isWebSocket bool) *Connection {
	now := time.Now().UnixMilli()
	c := &Connection{
		id:             id,
		Conn:           conn,
		CreatedAt:      now,
		msgWindowStart: now,
		isWebSocket:    isWebSocket,
	}
	c.remoteAddr.Store(&remoteAddr)
	c.LastActive.Store(now)
	c.userUUID.Store(&userUUID)
	c.state.Store(int32(StateForward))
	return c
}

// ID 返回连接标识。
func (c *Connection) ID() string { return c.id }

// GetState 原子读取连接状态。
func (c *Connection) GetState() ConnState { return ConnState(c.state.Load()) }

// SetState 原子地设置连接状态，仅当当前状态等于old时才更新为new。
func (c *Connection) SetState(old, new ConnState) bool {
	return c.state.CompareAndSwap(int32(old), int32(new))
}

// IsBound 判断连接是否已绑定到服务器。
func (c *Connection) IsBound() bool {
	v := c.serverID.Load()
	return v != nil && *v != ""
}

// IsAuthenticated 判断连接是否已完成用户认证（UUID非空且不以temp_开头）。
func (c *Connection) IsAuthenticated() bool {
	uuid := c.GetUserUUID()
	return uuid != "" && !strings.HasPrefix(uuid, "temp_")
}

// SetUserUUID 设置连接关联的用户 UUID。
func (c *Connection) SetUserUUID(uuid string) { c.userUUID.Store(&uuid) }
func (c *Connection) GetUserUUID() string {
	if v := c.userUUID.Load(); v != nil {
		return *v
	}
	return ""
}

// SetAccountID / GetAccountID 登录账号（KickService KickAccounts 本地索引）
func (c *Connection) SetAccountID(id string) { c.accountID.Store(&id) }
func (c *Connection) GetAccountID() string {
	if v := c.accountID.Load(); v != nil {
		return *v
	}
	return ""
}

// SetLoginKey / GetLoginKey LoginGate 校验通过后的 loginKey（周期 BatchValidate 安全网用）
func (c *Connection) SetLoginKey(k string) { c.loginKey.Store(&k) }
func (c *Connection) GetLoginKey() string {
	if v := c.loginKey.Load(); v != nil {
		return *v
	}
	return ""
}

// GetCachedLogicClient 获取缓存的LogicClient，避免每次消息都查询连接池。
func (c *Connection) GetCachedLogicClient() LogicClientProvider {
	if v := c.logicClient.Load(); v != nil {
		return *v
	}
	return nil
}

// SetCachedLogicClient 设置缓存的LogicClient。
func (c *Connection) SetCachedLogicClient(lc LogicClientProvider) {
	c.logicClient.Store(&lc)
}

// SetUDPDownlink 注入 UDP 下行通道（ASSOCIATE 成功时调用；关联失效后
// SendUDP 返回错误，SendByCmd 自动回落 WS，无需清除注入）。
func (c *Connection) SetUDPDownlink(d UDPDownlink) {
	c.udp.Store(&d)
}

// SetServerID 设置连接关联的服务器 ID。
func (c *Connection) SetServerID(sid string) { c.serverID.Store(&sid) }
func (c *Connection) GetServerID() string {
	if v := c.serverID.Load(); v != nil {
		return *v
	}
	return ""
}

// GetRemoteAddr 返回对端地址（ip:port，可能被反代改写）；并发安全。
func (c *Connection) GetRemoteAddr() string {
	if v := c.remoteAddr.Load(); v != nil {
		return *v
	}
	return ""
}

// RemoteHost 返回去掉端口的对端 IP，每连接首次调用后缓存。
// 热路径（过滤器上下文、安全链按 IP 限流）逐消息调用可免 SplitHostPort 分配。
func (c *Connection) RemoteHost() string {
	if v := c.remoteHost.Load(); v != nil {
		return *v
	}
	addr := c.GetRemoteAddr()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	c.remoteHost.Store(&host)
	return host
}

// SetRemoteHost 覆盖缓存的对端 IP（反代场景：WS 握手解析 X-Real-IP/X-Forwarded-For
// 后用真实客户端 IP 替换代理 IP）。同时更新 remoteAddr 使日志口径一致。
// 调用方需保证在此之前未并发调用过 RemoteHost()（热路径消息处理前完成），
// 否则 per-IP 计数与缓存会短暂不一致——WS 握手在首帧之前，满足此约束。
func (c *Connection) SetRemoteHost(host string) {
	c.remoteHost.Store(&host)
	if addr := c.GetRemoteAddr(); addr != "" {
		if _, port, ok := strings.Cut(addr, ":"); ok {
			addr = host + ":" + port
		} else {
			addr = host
		}
		c.remoteAddr.Store(&addr)
	}
}

// SetJWTJti 持久化当前会话 JWT jti（供 logout/封禁撤销）。
func (c *Connection) SetJWTJti(jti string) { c.jwtJti.Store(&jti) }

// GetJWTJti 返回连接上缓存的 JWT jti，无则空串。
func (c *Connection) GetJWTJti() string {
	if v := c.jwtJti.Load(); v != nil {
		return *v
	}
	return ""
}

// SetJWTExp 持久化 JWT 过期时间（Unix 秒）。
func (c *Connection) SetJWTExp(exp int64) { c.jwtExp.Store(exp) }

// GetJWTExp 返回连接上缓存的 JWT exp。
func (c *Connection) GetJWTExp() int64 { return c.jwtExp.Load() }

// OfflineNotified 返回下线通知是否已成功送达。
// 幂等语义 = "已送达"而非"已尝试"：发送失败不置位，允许 OnClose 等后续路径重试。
func (c *Connection) OfflineNotified() bool {
	return c.offlineSent.Load()
}

// MarkOfflineNotified 标记下线通知已成功送达（发送成功后调用）。
func (c *Connection) MarkOfflineNotified() {
	c.offlineSent.Store(true)
}

// TouchActive 刷新最后活跃时间（入站流量路径调用；与 Send 的 push 采样独立）。
func (c *Connection) TouchActive() {
	c.LastActive.Store(time.Now().UnixMilli())
}

// CheckAndIncrementMsgRate 检查连接级消息速率是否超限，未超限则自增计数。
// maxPerConn=0 表示不限制。返回 true 表示允许，false 表示超限。
func (c *Connection) CheckAndIncrementMsgRate(maxPerConn int) bool {
	if maxPerConn <= 0 {
		return true
	}
	now := time.Now().UnixMilli()
	c.msgRateMu.Lock()
	if now-c.msgWindowStart >= 1000 {
		c.msgWindowStart = now
		c.msgCount = 1
		c.msgRateMu.Unlock()
		return true
	}
	if c.msgCount >= int64(maxPerConn) {
		c.msgRateMu.Unlock()
		return false
	}
	c.msgCount++
	c.msgRateMu.Unlock()
	return true
}

// asyncWriteFailures 异步写完成回调收到的失败计数。
// 端到端 ACK 观测的一环（roadmap P2-1 阶段 3）：写失败原本被 Noop 回调吞掉，
// /stats 完全无感知；计数后与 logic 侧 giveUps/droppedOffline 互补，
// 覆盖"网关异步写客户端失败"这一丢包点。
var asyncWriteFailures atomic.Int64

// AsyncWriteFailures 返回异步写完成回调累计收到的失败数（/stats 观测）。
func AsyncWriteFailures() int64 { return asyncWriteFailures.Load() }

// recordAsyncWriteFailure 记录一次异步写完成失败。
// 所有 async 完成回调共用的唯一观测点，勿在调用点散写计数。
func recordAsyncWriteFailure(err error) {
	if err != nil {
		asyncWriteFailures.Add(1)
	}
}

// AsyncWriteCallback 是异步写入完成时的计数回调：写失败时累加计数，
// 不干预连接生命周期（连接由心跳超时与读错误清理负责）。
// 回调在 gnet event loop 内触发，必须不阻塞——只做一次原子自增。
// 返回 nil：gnet 忽略回调返回值，保持与原 NoopAsyncCallback 行为一致。
func AsyncWriteCallback(_ gnet.Conn, err error) error {
	recordAsyncWriteFailure(err)
	return nil
}

// ============================================================================
// 下行写背压（有界化）—— benchmark-report「场景 C OOM」遗留修复建议 1
// ============================================================================
//
// 无界增长的两处：AppendCoalesced 合批缓冲（慢消费者不读 → flush 积压）
// 与 gnet 异步写队列（kernel 缓冲满 → 完成回调迟迟不触发，队列单调增长）。
// 裁定语义（先延迟后丢弃 + 两级上限）：
//   - 每连接在途写 ≤ maxConnPendingWrite：超限时 flush 延迟（缓冲原样保留、
//     下轮重试），直发返回 ErrWriteBackpressure（计数后交调用方处理）；
//   - 全局在途写超过 maxGlobalPendingWrite（安全阀）时，只约束已超过公平
//     份额 pendingValveShare（= 全局上限/活跃连接数，flushLoop 每轮发布）
//     的连接——慢连接囤积被切，份额以下的健康连接不被饿死；
//   - 合批缓冲 ≤ maxCoalescedBytes：延迟后仍满才丢新消息（计数）。
//
// 总量上界 ≈ maxGlobalPendingWrite + 活跃连接数 × maxCoalescedBytes，
// 即"12K 无读连接"场景内存封顶约 1~2GB，不再打爆系统提交量。
const (
	maxConnPendingWrite   = 2 << 20  // 2MB：单连接在途（异步写未完成）字节上限
	maxCoalescedBytes     = 64 << 10 // 64KB：单连接合批缓冲硬顶（超则丢新）
	maxGlobalPendingWrite = 1 << 30  // 1GB：全局在途安全阀（慢连接集群兜底）
)

var (
	// pendingWriteTotal 全局在途写字节（安全阀读数，与各连接 pendingWriteBytes 成对增减）。
	pendingWriteTotal atomic.Int64
	// backpressureDrops 背压拒绝的下行帧数（直发拒绝 + 合批缓冲丢新；
	// batchPush 合帧按帧计，一帧可含多条消息）。/stats pushDroppedBackpressure。
	backpressureDrops atomic.Int64
	// pendingValveShare 阀触发时的单连接公平份额：flushLoop 每轮按活跃
	// 连接数发布；零值（coalescer 未启动）回退为每连接上限（阀不生效）。
	pendingValveShare atomic.Int64
)

// ErrWriteBackpressure 下行在途写超预算，本条消息被拒绝（已计 backpressureDrops）。
// 调用方按普通发送失败处理：kick 等关键路径已有回退（直连关闭），
// 应答路径由客户端超时重试兜底。
var ErrWriteBackpressure = errors.New("downlink write backpressure: pending budget exceeded")

// BackpressureDrops 返回背压拒绝/丢弃的下行帧数（/stats 观测）。
func BackpressureDrops() int64 { return backpressureDrops.Load() }

// PendingWriteTotal 返回全局在途写字节（/stats gauge 观测）。
func PendingWriteTotal() int64 { return pendingWriteTotal.Load() }

// reserveWrite 预留在途写字节：乐观相加后判定，超预算则回滚并返回 false。
// 并发发送在判定窗口内的瞬时超调 = 并发发送数，不影响上界结论。
// 超预算 = 每连接上限，或全局安全阀已触发且本连接超过公平份额。
func (c *Connection) reserveWrite(n int64) bool {
	local := c.pendingWriteBytes.Add(n)
	global := pendingWriteTotal.Add(n)
	share := pendingValveShare.Load()
	if share <= 0 || share > maxConnPendingWrite {
		share = maxConnPendingWrite
	}
	if local > maxConnPendingWrite || (global > maxGlobalPendingWrite && local > share) {
		c.pendingWriteBytes.Add(-n)
		pendingWriteTotal.Add(-n)
		return false
	}
	return true
}

// writeDone 归还在途字节：异步写完成回调（成功或失败）或入队失败回滚时调用。
// gnet 契约：AsyncWrite 返回 nil ⇒ 回调必达（含写失败/连接关闭）；返回非
// nil ⇒ 未入队、回调不触发——两条路互斥，不会重复归还（进程退出时回调
// 可能不触发，但此时进程正在退出，计数无意义）。
func (c *Connection) writeDone(n int64) {
	c.pendingWriteBytes.Add(-n)
	pendingWriteTotal.Add(-n)
}

// writeCallback 包装异步写完成回调：先归还在途字节，再执行 next。
func (c *Connection) writeCallback(n int64, next gnet.AsyncCallback) gnet.AsyncCallback {
	return func(g gnet.Conn, err error) error {
		c.writeDone(n)
		if next != nil {
			return next(g, err)
		}
		return nil
	}
}

// asyncWrite 直发统一入口：预算预留 → 入队 → 失败回滚。
// do 封装实际入队差异（AsyncWrite/AsyncWritev），next 为完成回调的后续动作。
func (c *Connection) asyncWrite(n int64, do func(gnet.AsyncCallback) error, next gnet.AsyncCallback) error {
	if !c.reserveWrite(n) {
		backpressureDrops.Add(1)
		return ErrWriteBackpressure
	}
	if err := do(c.writeCallback(n, next)); err != nil {
		c.writeDone(n) // 未入队，回调不触发，回滚预留
		return err
	}
	return nil
}

// touch 更新连接的最后活跃时间，每64次调用才实际更新一次以减少原子操作开销。
func (c *Connection) touch() {
	seq := c.activitySeq.Add(1)
	if seq&63 != 0 && c.LastActive.Load() != 0 {
		return
	}
	c.LastActive.Store(time.Now().UnixMilli())
}

// SendByCmd 按命令号段分流的唯一回程入口（docs/udp-ws-design.md §5.3）。
// 号段内（1200000~1299999）且 UDP 通道可用 → 走 UDP；无关联/关联不新鲜/
// 发送失败 → 回落 WS（降级不丢是硬要求）。号段外直接 WS，与原 Send 等价。
// 全系统下行调用点只换本方法名，不得在调用点散写 IsUDPCmd 判断。
func (c *Connection) SendByCmd(cmd int32, data []byte) error {
	if d := c.udp.Load(); d != nil && protoGw.IsUDPCmd(cmd) {
		if err := (*d).SendUDP(c.id, data); err == nil {
			return nil
		}
		// UDP 路不可用：回落 WS，消息不丢
	}
	return c.Send(data)
}

// Send 向客户端发送数据，根据协议自动封帧（TCP 4字节前缀 / WS binary 帧）。
// 在途写超预算时返回 ErrWriteBackpressure（消息被拒并计数），不做无限排队。
func (c *Connection) Send(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	// 内联异步封帧逻辑
	if c.isWebSocket {
		frame := EncodeWSFrame(0x82, data)
		return c.asyncWrite(int64(len(frame)), func(cb gnet.AsyncCallback) error {
			return c.Conn.AsyncWrite(frame, cb)
		}, AsyncWriteCallback)
	}
	// TCP 快速路径：4字节长度前缀 + payload
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	return c.asyncWrite(int64(4+len(data)), func(cb gnet.AsyncCallback) error {
		return c.Conn.AsyncWritev([][]byte{header[:], data}, cb)
	}, AsyncWriteCallback)
}

// SendMulti 发送已组装好的合并数据。
func (c *Connection) SendMulti(combined []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	return c.asyncWrite(int64(len(combined)), func(cb gnet.AsyncCallback) error {
		return c.Conn.AsyncWrite(combined, cb)
	}, AsyncWriteCallback)
}

// SendMultiWithCallback 发送合并数据，写入完成后执行回调函数。
// WebSocket 与 TCP 均执行回调：coalescer 依赖回调归还池化缓冲，
// WS 分支曾丢弃回调导致缓冲永不归还。
// 超预算返回 ErrWriteBackpressure（不入队、不执行回调）。
func (c *Connection) SendMultiWithCallback(combined []byte, cb func()) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	return c.asyncWrite(int64(len(combined)), func(gcb gnet.AsyncCallback) error {
		return c.Conn.AsyncWrite(combined, gcb)
	}, func(_ gnet.Conn, err error) error {
		// coalescer 合并写同样计入 async 写失败（唯一观测点）。
		recordAsyncWriteFailure(err)
		if cb != nil {
			cb()
		}
		return nil
	})
}

// SendThenClose 发送数据并在写入完成后关闭连接（踢人通知专用）。
// 保证客户端在连接关闭前收到通知帧：AsyncWrite(v) 回调在 write 完成（成功或
// 失败）后于 event loop 内触发，此时再执行 Close，避免 fire-and-forget Send
// 后立即 Close 导致通知丢失（尤其 Windows 无 outbound flush）。
func (c *Connection) SendThenClose(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	closeAfterWrite := func(_ gnet.Conn, err error) error {
		recordAsyncWriteFailure(err)
		_ = c.Conn.Close()
		return nil
	}
	// 内联异步封帧逻辑
	if c.isWebSocket {
		frame := EncodeWSFrame(0x82, data)
		return c.asyncWrite(int64(len(frame)), func(cb gnet.AsyncCallback) error {
			return c.Conn.AsyncWrite(frame, cb)
		}, closeAfterWrite)
	}
	// TCP 快速路径
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	return c.asyncWrite(int64(4+len(data)), func(cb gnet.AsyncCallback) error {
		return c.Conn.AsyncWritev([][]byte{header[:], data}, cb)
	}, closeAfterWrite)
}

// SendSync 在事件循环内同步写出一条消息（Writev TCP / Write WS）。
// 仅允许在 gnet 事件循环内调用（OnTraffic 等）；跨协程写用 Send。
func (c *Connection) SendSync(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	// 内联封帧逻辑：编译器可完全消除分支开销（isWebSocket 常量传播）
	if c.isWebSocket {
		frame := EncodeWSFrame(0x82, data)
		_, err := c.Conn.Write(frame)
		return err
	}
	// TCP 快速路径：4字节长度前缀 + payload，零分配 Writev
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	_, err := c.Conn.Writev([][]byte{header[:], data})
	return err
}

func (c *Connection) Close() error {
	return c.Conn.Close()
}

// ============================================================================
// 连接级写合并（per-Connection Coalescing）
// ============================================================================

// AppendCoalesced 将一条消息追加到连接的 coalescing buffer。
// 由 receiveMessages 单协程调用，per-Connection 锁几乎无竞争。
// 返回 false = 缓冲已达硬顶（flush 被背压延迟所致），本条被丢弃并计数
// （backpressureDrops）——先延迟后丢弃的最后一步，防止为兜底延迟而失守内存。
// 缓冲为空时首条不设限：单条大消息（可超硬顶）放行，上限只约束攒积。
func (c *Connection) AppendCoalesced(payload []byte) bool {
	c.coalescedMu.Lock()
	defer c.coalescedMu.Unlock()
	if len(c.coalescedData) > 0 && len(c.coalescedData)+4+len(payload) > maxCoalescedBytes {
		backpressureDrops.Add(1)
		return false
	}
	if c.coalescedBufPtr == nil {
		bufPtr := coalescerBufPool.Get().(*[]byte)
		c.coalescedBufPtr = bufPtr
		c.coalescedData = (*bufPtr)[:0]
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	c.coalescedData = append(c.coalescedData, lenBuf[:]...)
	c.coalescedData = append(c.coalescedData, payload...)
	c.coalescedCount++
	return true
}

// hasCoalesced 是否仍有未 flush 的攒积数据（flush 延迟后的重试判据）。
func (c *Connection) hasCoalesced() bool {
	c.coalescedMu.Lock()
	defer c.coalescedMu.Unlock()
	return c.coalescedCount > 0
}

// FlushCoalesced 将累积的数据通过 AsyncWrite 发送，然后重置 buffer。
// 返回成功推送的消息数。IO 在锁外执行，不阻塞 AppendCoalesced。
// 通过 Framer.WrapCoalesced 转换为线上格式：TCP 零拷贝原样发送，WS 重封为帧序列。
// 预算判定在换出缓冲之前：在途写超限 → 返回 0 且缓冲原样保留（只延迟不丢），
// 由 coalescer 重新标脏、下轮 flush 重试。
func (c *Connection) FlushCoalesced() int64 {
	c.coalescedMu.Lock()
	if c.coalescedCount == 0 {
		c.coalescedMu.Unlock()
		return 0
	}
	n := int64(len(c.coalescedData))
	// 在途写预算预留（每连接上限 / 全局安全阀份额）：失败 = 延迟本轮。
	// 在锁内完成"读尺寸 + 预留"，避免换出后预留失败导致缓冲数据丢失。
	if !c.reserveWrite(n) {
		c.coalescedMu.Unlock()
		return 0
	}
	data := c.coalescedData
	bufPtr := c.coalescedBufPtr
	count := c.coalescedCount
	c.coalescedData = nil
	c.coalescedBufPtr = nil
	c.coalescedCount = 0
	c.coalescedMu.Unlock()

	// 内联封帧转换：TCP 零拷贝 / WS reframe
	if c.isWebSocket {
		// 将 [len][payload]* 合并缓冲重封为 WS 帧序列
		wsData := reframeCoalescedAsWSFrames(data)
		if wsData != nil {
			data = wsData
		}
		// 解析失败时 data 保持原缓冲（罕见异常路径，WS 客户端收到裸 TCP 格式）
		// 注：预留按重封前尺寸计（WS 帧头 ~10B/条的差额远小于 MB 级上限）。
	}
	// TCP 路径：data 已是 [len][payload]* 格式，零拷贝直接发送

	// data 为只读：TCP 路径直接引用 bufPtr 底层数组，WS 路径为 reframed 拷贝。
	// 回调触发时 AsyncWrite 已完成，才可把 bufPtr 归还池；不能提前归还。
	// WS 路径同样执行回调：否则 WS 连接的 coalescer 缓冲永不归还池。
	// returned CAS 保证缓冲只归还一次：AsyncWrite 同步返回错误（未入队，回调
	// 不触发）时此处归还；若实现同时触发回调也不会双重 Put 污染池。
	var returned atomic.Bool
	putBack := func() {
		if bufPtr == nil || !returned.CompareAndSwap(false, true) {
			return
		}
		if cap(*bufPtr) <= coalescerMaxBufCap {
			*bufPtr = (*bufPtr)[:0]
			coalescerBufPool.Put(bufPtr)
		}
	}
	if err := c.Conn.AsyncWrite(data, c.writeCallback(n, func(_ gnet.Conn, err error) error {
		// coalescer 合并写同样计入 async 写失败（唯一观测点）。
		recordAsyncWriteFailure(err)
		putBack()
		return nil
	})); err != nil {
		// 未入队：回调不会执行，回滚预算预留 + 同步归还缓冲（消息已丢，
		// 但 coalescedCount 已在锁内清零，调用方以返回值记推送量）
		c.writeDone(n)
		putBack()
		return 0
	}
	return int64(count)
}

// reframeCoalescedAsWSFrames 将 coalescer 缓冲中的 [len][payload]* 序列转换为
// 等价的 WS binary 帧序列（每个 payload 一个 FIN 帧）。解析失败返回 nil。
// 已移入 wsFramer.WrapCoalesced；此函数保留供直接调用（测试/诊断）。
func reframeCoalescedAsWSFrames(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	out := make([]byte, 0, len(data)+len(data)/8+16)
	off := 0
	for off+4 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		if n < 0 || off+n > len(data) {
			return nil
		}
		frame := EncodeWSFrame(0x82, data[off:off+n])
		out = append(out, frame...)
		off += n
	}
	if off != len(data) {
		return nil
	}
	return out
}
