package dataplane

import (
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// WSOpCode 表示WebSocket操作码
type WSOpCode byte

const (
	WSOpText   WSOpCode = 0x1 // 文本帧
	WSOpBinary WSOpCode = 0x2 // 二进制帧
	WSOpClose  WSOpCode = 0x8 // 关闭帧
	WSOpPing   WSOpCode = 0x9 // 心跳探测帧
	WSOpPong   WSOpCode = 0xA // 心跳回复帧
)

// WebSocket 连接状态常量
const (
	WSStateHandshake = iota // 握手中
	WSStateOpen             // 已打开
	WSStateClosing          // 关闭中
	WSStateClosed           // 已关闭
)

// WebSocketConnection 表示WebSocket连接，包含缓冲区和状态信息。
// 不使用 sync.Pool — 指针作为 sync.Map key 时，pool 复用会导致陈旧 entry。
// 缓冲/分片字段（Buffer/BufOff/Frag*）由 transport 平面的帧循环读写，故导出。
type WebSocketConnection struct {
	Conn         gnet.Conn
	State        atomic.Int32
	Buffer       []byte
	BufOff       int                    // Buffer 已消费前缀偏移（同 TCP ConnContext.FrameBuf，append 热路径零重分配）
	connID       atomic.Pointer[string] // ConnectionID（事件循环写、心跳协程读，须原子）
	lastPingUnix atomic.Int64           // 纳秒时间戳，避免 time.Time 字段跨协程读写竞态

	// 分片帧重组（FIN=0 的 continuation）
	FragOp   WSOpCode
	FragBuf  []byte
	Fragging bool
}

// ConnectionID 返回连接标识（并发安全）。
func (c *WebSocketConnection) ConnectionID() string {
	if v := c.connID.Load(); v != nil {
		return *v
	}
	return ""
}

// SetConnectionID 设置连接标识（并发安全）。
func (c *WebSocketConnection) SetConnectionID(id string) { c.connID.Store(&id) }

// NewWebSocketConnection 创建 WebSocket 连接并初始化状态。
func NewWebSocketConnection(conn gnet.Conn) *WebSocketConnection {
	ws := &WebSocketConnection{
		Conn:   conn,
		Buffer: make([]byte, 0, 4096),
	}
	ws.lastPingUnix.Store(time.Now().UnixNano())
	return ws
}

// SetLastPingTime 更新最后心跳时间（并发安全）。
func (c *WebSocketConnection) SetLastPingTime(t time.Time) {
	c.lastPingUnix.Store(t.UnixNano())
}

// GetLastPingTime 获取最后心跳时间（并发安全）。
func (c *WebSocketConnection) GetLastPingTime() time.Time {
	return time.Unix(0, c.lastPingUnix.Load())
}
