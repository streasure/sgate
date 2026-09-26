package connection

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
)

type ConnState int32

const (
	StateForward ConnState = 0 // 转发状态，连接正常可转发消息
	StateClosed  ConnState = 1 // 已关闭状态
)

// Connection 表示客户端与网关之间的连接，封装了连接状态、用户信息和发送逻辑。
type Connection struct {
	id          string
	Conn        gnet.Conn
	RemoteAddr  string
	CreatedAt   int64
	LastActive  atomic.Int64
	activitySeq atomic.Uint32
	Status      int8
	state       atomic.Int32

	// 原子字段，用于无锁并发访问
	userUUID    atomic.Value // 保存用户唯一标识。
	serverID    atomic.Value // 保存绑定的逻辑服标识。
	isWS        atomic.Bool
	logicClient atomic.Value // LogicClientProvider 缓存，避免每条消息查询连接池
	jwtJti      atomic.Value // string：JWT jti（logout/封禁时撤销）
	jwtExp      atomic.Int64 // JWT exp（Unix 秒；0=未知）
	offlineSent atomic.Bool  // CmdUserOffline 已发送标记（防显式通知+OnClose 双发）

	// 连接级流控
	msgRateMu      sync.Mutex // 保护 msgWindowStart 和 msgCount 的原子更新
	msgCount       int64      // 当前窗口消息计数
	msgWindowStart int64      // 当前窗口起始时间（UnixMilli）

	// 连接级写合并：每连接独立 buffer，addMulti 只需 per-Connection 锁（几乎无竞争）
	coalescedMu     sync.Mutex
	coalescedData   []byte  // [4-byte len][payload] 累积格式
	coalescedBufPtr *[]byte // 指向 coalescerBufPool 的 buffer，用于归还
	coalescedCount  int     // 累积消息数
}

// newConnection 创建新的连接对象，初始化基本属性和状态。
func newConnection(id string, conn gnet.Conn, userUUID, remoteAddr string) *Connection {
	now := time.Now().UnixMilli()
	c := &Connection{
		id:             id,
		Conn:           conn,
		RemoteAddr:     remoteAddr,
		CreatedAt:      now,
		msgWindowStart: now,
	}
	c.LastActive.Store(now)
	c.userUUID.Store(userUUID)
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
	return v != nil && v.(string) != ""
}

// IsAuthenticated 判断连接是否已完成用户认证（UUID非空且不以temp_开头）。
func (c *Connection) IsAuthenticated() bool {
	uuid := c.userUUID.Load().(string)
	return uuid != "" && !strings.HasPrefix(uuid, "temp_")
}

// IsWebSocket 判断连接是否使用 WebSocket 传输。
func (c *Connection) IsWebSocket() bool { return c.isWS.Load() }

// SetUserUUID 设置连接关联的用户 UUID。
func (c *Connection) SetUserUUID(uuid string) { c.userUUID.Store(uuid) }
func (c *Connection) GetUserUUID() string {
	if v := c.userUUID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// GetCachedLogicClient 获取缓存的LogicClient，避免每次消息都查询连接池。
func (c *Connection) GetCachedLogicClient() LogicClientProvider {
	if v := c.logicClient.Load(); v != nil {
		return v.(LogicClientProvider)
	}
	return nil
}

// SetCachedLogicClient 设置缓存的LogicClient。
func (c *Connection) SetCachedLogicClient(lc LogicClientProvider) {
	c.logicClient.Store(lc)
}

// SetServerID 设置连接关联的服务器 ID。
func (c *Connection) SetServerID(sid string) { c.serverID.Store(sid) }
func (c *Connection) GetServerID() string {
	if v := c.serverID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// SetWS 设置连接是否使用 WebSocket 传输。
func (c *Connection) SetWS(v bool) { c.isWS.Store(v) }

// SetJWTJti 持久化当前会话 JWT jti（供 logout/封禁撤销）。
func (c *Connection) SetJWTJti(jti string) { c.jwtJti.Store(jti) }

// GetJWTJti 返回连接上缓存的 JWT jti，无则空串。
func (c *Connection) GetJWTJti() string {
	if v := c.jwtJti.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SetJWTExp 持久化 JWT 过期时间（Unix 秒）。
func (c *Connection) SetJWTExp(exp int64) { c.jwtExp.Store(exp) }

// GetJWTExp 返回连接上缓存的 JWT exp。
func (c *Connection) GetJWTExp() int64 { return c.jwtExp.Load() }

// ClaimOfflineNotify CAS 标记下线通知已发送；返回 true 表示本次应发送。
func (c *Connection) ClaimOfflineNotify() bool {
	return !c.offlineSent.Swap(true)
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

// noopAsyncCallback 是异步写入完成时使用的空回调。
func noopAsyncCallback(_ gnet.Conn, _ error) error { return nil }

// touch 更新连接的最后活跃时间，每64次调用才实际更新一次以减少原子操作开销。
func (c *Connection) touch() {
	seq := c.activitySeq.Add(1)
	if seq&63 != 0 && c.LastActive.Load() != 0 {
		return
	}
	c.LastActive.Store(time.Now().UnixMilli())
}

// Send 向客户端发送数据，WebSocket连接自动封装为WS帧。
func (c *Connection) Send(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	if c.IsWebSocket() {
		return c.sendWSFrame(data)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	return c.Conn.AsyncWritev([][]byte{header[:], data}, noopAsyncCallback)
}

// SendMulti 发送已组装好的合并数据。
func (c *Connection) SendMulti(combined []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	return c.Conn.AsyncWrite(combined, noopAsyncCallback)
}

// SendMultiWithCallback 发送合并数据，写入完成后执行回调函数。
func (c *Connection) SendMultiWithCallback(combined []byte, cb func()) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	if c.IsWebSocket() {
		return c.Conn.AsyncWrite(combined, noopAsyncCallback)
	}
	return c.Conn.AsyncWrite(combined, func(_ gnet.Conn, _ error) error {
		if cb != nil {
			cb()
		}
		return nil
	})
}

// sendWSFrame 将数据封装为WebSocket帧并发送，支持不同长度的载荷。
func (c *Connection) sendWSFrame(data []byte) error {
	frame := EncodeWSFrame(0x82, data)
	return c.Conn.AsyncWrite(frame, noopAsyncCallback)
}

func (c *Connection) Close() error {
	return c.Conn.Close()
}

// ============================================================================
// 连接级写合并（per-Connection Coalescing）
// ============================================================================

// AppendCoalesced 将一条消息追加到连接的 coalescing buffer。
// 由 receiveMessages 单协程调用，per-Connection 锁几乎无竞争。
func (c *Connection) AppendCoalesced(payload []byte) bool {
	c.coalescedMu.Lock()
	defer c.coalescedMu.Unlock()
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

// FlushCoalesced 将累积的数据通过 AsyncWrite 发送，然后重置 buffer。
// 返回成功推送的消息数。IO 在锁外执行，不阻塞 AppendCoalesced。
// WebSocket 连接将 [len][payload] 单元序列重新包装为 WS binary 帧，避免客户端解析到裸 TCP 长度前缀。
func (c *Connection) FlushCoalesced() int64 {
	c.coalescedMu.Lock()
	if c.coalescedCount == 0 {
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

	if c.IsWebSocket() {
		reframed := reframeCoalescedAsWSFrames(data)
		if reframed != nil {
			data = reframed
		}
	}

	_ = c.SendMultiWithCallback(data, func() {
		if bufPtr != nil && cap(*bufPtr) <= coalescerMaxBufCap {
			*bufPtr = (*bufPtr)[:0]
			coalescerBufPool.Put(bufPtr)
		}
		if reframedNeedsFree(data) {
			// reframed 分配自 append，无需归还对象池
		}
	})
	return int64(count)
}

// reframedNeedsFree 占位：reframe 结果使用普通分配，回调无需额外处理。
func reframedNeedsFree(_ []byte) bool { return false }

// reframeCoalescedAsWSFrames 将 coalescer 缓冲中的 [len][payload]* 序列转换为
// 等价的 WS binary 帧序列（每个 payload 一个 FIN 帧）。解析失败返回 nil。
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
		out = EncodeWSFrame(0x82, data[off:off+n])
		// EncodeWSFrame 返回独立 buffer，连续多帧需拼接
		// 为避免二次分配，改为手动拼接：
		// —— 实现见下方循环重写。
		_ = out
		off += n
	}
	// 正确路径：一次遍历拼接所有帧
	out = out[:0]
	off = 0
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
