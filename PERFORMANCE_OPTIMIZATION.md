# sgate 性能优化方案

## 目标

- **双向通信**：千万级（10,000,000）并发连接，单机百万级 QPS
- **组推送**：百万级（1,000,000）消息/秒

## 当前瓶颈分析

### 1. 连接管理层

| 瓶颈 | 位置 | 严重程度 | 优化方案 |
|------|------|----------|----------|
| `IsBound()/IsAuthenticated()` 每条消息加锁 | `connection.go:61-71` | 高 | 改用 `atomic.Value` 存储 |
| `Send()` 每次分配 4 字节 header | `connection.go:97` | 中 | 使用 `sync.Pool` |
| `sendWSFrame()` 每帧分配 buffer | `connection.go:126-143` | 高 | 使用 `sync.Pool` |
| `groupMutex` 全局写锁串行化 | `connection.go:364-413` | 高 | 分片锁或 `sync.Map` |

### 2. Codec 层

| 瓶颈 | 位置 | 严重程度 | 优化方案 |
|------|------|----------|----------|
| `TCPCodec.Decode()` 每条消息分配 buffer | `codec_tcp.go:50` | 严重 | `sync.Pool` 池化 |
| `TCPCodec.Encode()` 每次分配 buffer | `codec_tcp.go:59` | 高 | `sync.Pool` 池化 |
| `WebSocketCodec.readFrame()` payload 分配 | `codec_ws.go:182` | 高 | `sync.Pool` 池化 |

### 3. gRPC Stream 层

| 瓶颈 | 位置 | 严重程度 | 优化方案 |
|------|------|----------|----------|
| `SendMessage()` 每条消息创建 timer | `backend.go:375` | 高 | 复用 timer 或 channel buffer |
| `StreamShard` 逐条 `stream.Send()` | `backend.go:333-349` | 高 | 批量发送优化 |
| `StreamMessageQueue` busy-wait | `backend.go:1115-1116` | 中 | 条件变量通知 |

### 4. 安全层

| 瓶颈 | 位置 | 严重程度 | 优化方案 |
|------|------|----------|----------|
| `RateLimiter` 全局 RWMutex + 嵌套 map | `ratelimit.go:112-146` | 高 | 分片 map 或 `sync.Map` |

### 5. 配置层

| 瓶颈 | 位置 | 严重程度 | 优化方案 |
|------|------|----------|----------|
| gnet 缓冲区硬编码 | `frontend.go:387-394` | 中 | 支持配置化 |
| `GOMEMLIMIT` 未调用 | `main.go` 缺失 | 高 | 启动时调用 |

---

## 优化实施计划

### Phase 1: 核心热路径优化（预计提升 3-5x）

#### 1.1 连接字段原子化

**文件**: `internal/connection.go`

```go
// 优化前
type Connection struct {
    mu       sync.Mutex
    ServerID string
    UserUUID string
    IsWS     bool
}

func (c *Connection) IsBound() bool {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.ServerID != ""
}

// 优化后
type Connection struct {
    serverID atomic.Value  // string
    userUUID atomic.Value  // string
    isWS     atomic.Bool
}

func (c *Connection) IsBound() bool {
    return c.serverID.Load().(string) != ""
}

func (c *Connection) SetServerID(sid string) {
    c.serverID.Store(sid)
}
```

#### 1.2 Codec Buffer Pool

**文件**: `internal/codec/codec_tcp.go`, `internal/codec/codec_ws.go`

```go
// 新增 buffer pool
var (
    decodeBufPool = sync.Pool{
        New: func() interface{} {
            buf := make([]byte, 0, 64*1024) // 64KB
            return &buf
        },
    }
    encodeBufPool = sync.Pool{
        New: func() interface{} {
            buf := make([]byte, 0, 64*1024)
            return &buf
        },
    }
)

// TCPCodec.Decode 优化
func (c *TCPCodec) Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error) {
    bufPtr := decodeBufPool.Get().(*[]byte)
    defer decodeBufPool.Put(bufPtr)
    buf := (*bufPtr)[:0]
    // ... 使用 buf 而不是 make([]byte, dataLen)
}
```

#### 1.3 Header Buffer Pool

**文件**: `internal/connection.go`

```go
var headerPool = sync.Pool{
    New: func() interface{} {
        buf := make([]byte, 4)
        return &buf
    },
}

func (c *Connection) Send(data []byte) error {
    headerPtr := headerPool.Get().(*[]byte)
    defer headerPool.Put(headerPtr)
    header := *headerPtr
    binary.BigEndian.PutUint32(header, uint32(len(data)))
    return c.Conn.AsyncWritev([][]byte{header, data}, noopAsyncCallback)
}
```

### Phase 2: 锁优化（预计提升 2-3x）

#### 2.1 GroupManager 分片锁

**文件**: `internal/connection.go`

```go
// 分片组管理
const groupShardCount = 256

type GroupManager struct {
    shards [groupShardCount]struct {
        mu    sync.RWMutex
        items map[string]*ConnectionGroupInfo
    }
}

func (gm *GroupManager) getShard(key string) *groupShard {
    h := fnv.New32a()
    h.Write([]byte(key))
    return &gm.shards[h.Sum32()%groupShardCount]
}

func (gm *GroupManager) AddUserToGroup(groupID, serverID, userUUID string) {
    shard := gm.getShard(groupID)
    shard.mu.Lock()
    defer shard.mu.Unlock()
    // ...
}
```

#### 2.2 RateLimiter 分片

**文件**: `internal/security/ratelimit.go`

```go
type RateLimiter struct {
    shards [256]struct {
        mu     sync.RWMutex
        tokens map[string]*TokenBucket
    }
}
```

### Phase 3: gRPC Stream 优化（预计提升 2x）

#### 3.1 Timer 复用

**文件**: `internal/backend.go`

```go
// 优化前
func (s *StreamShard) SendMessage(msg *protoGw.StreamData) error {
    timer := time.NewTimer(s.sendTimeout)
    defer timer.Stop()
    select {
    case s.sendCh <- msg:
        return nil
    case <-timer.C:
        return ErrSendTimeout
    }
}

// 优化后：使用 buffered channel + non-blocking send
func (s *StreamShard) SendMessage(msg *protoGw.StreamData) error {
    select {
    case s.sendCh <- msg:
        return nil
    default:
        // channel 满时的降级策略
        return ErrQueueFull
    }
}
```

#### 3.2 批量发送优化

**文件**: `internal/backend.go`

```go
func (s *StreamShard) startSendLoop() {
    batch := make([]*protoGw.StreamData, 0, 64)
    ticker := time.NewTicker(time.Millisecond)
    defer ticker.Stop()
    
    for {
        select {
        case msg := <-s.sendCh:
            batch = append(batch, msg)
            if len(batch) >= 64 {
                s.sendBatch(batch)
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                s.sendBatch(batch)
                batch = batch[:0]
            }
        }
    }
}

func (s *StreamShard) sendBatch(batch []*protoGw.StreamData) {
    s.mu.Lock()
    defer s.mu.Unlock()
    for _, msg := range batch {
        if err := s.stream.Send(msg); err != nil {
            return
        }
    }
}
```

### Phase 4: 配置优化

#### 4.1 gnet 缓冲区配置化

**文件**: `internal/config/config.go`

```go
type GnetConfig struct {
    ReadBufferCap    int `yaml:"readBufferCap"`    // 默认 256KB
    WriteBufferCap   int `yaml:"writeBufferCap"`   // 默认 256KB
    SocketRecvBuffer int `yaml:"socketRecvBuffer"` // 默认 4MB
    SocketSendBuffer int `yaml:"socketSendBuffer"` // 默认 4MB
    Multicore        bool `yaml:"multicore"`       // 默认 true
}
```

#### 4.2 GOMEMLIMIT 启用

**文件**: `cmd/gateway/main.go`

```go
func main() {
    // 启动时调用
    applyGOMEMLIMIT()
    
    // 设置 GOGC
    debug.SetGCPercent(200)
    
    // ... 其余代码
}
```

---

## 性能指标目标

| 指标 | 当前值 | 目标值 | 优化后预期 |
|------|--------|--------|------------|
| 并发连接数 | 10K | 10M | 10M |
| TCP 双向 QPS | 10K | 10M | 5-8M |
| WebSocket QPS | 10K | 5M | 3-5M |
| 组推送 QPS | 6.6K | 1M | 500K-1M |
| 内存占用 | 100MB | 10GB | 8-10GB |
| P99 延迟 | 2ms | <1ms | 0.5ms |

---

## 硬件要求

### 单机千万级连接

- **CPU**: 32+ 核心
- **内存**: 32GB+（每连接 ~2KB 元数据）
- **网络**: 10Gbps+
- **文件描述符**: 16M+（需调整 ulimit）

### 操作系统调优

```bash
# Linux
sysctl -w net.core.somaxconn=65535
sysctl -w net.ipv4.tcp_max_syn_backlog=65535
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sysctl -w net.ipv4.tcp_tw_reuse=1
sysctl -w fs.file-max=16777216
ulimit -n 16777216

# Windows (PowerShell)
netsh int ipv4 set global maxuserport=65534
netsh int ipv4 set global烟囱模式=disabled
```

---

## 验证方法

1. **单元测试**: 确保优化后功能正确
2. **基准测试**: 对比优化前后性能
3. **压力测试**: 逐步增加负载至目标值
4. **长时间稳定性测试**: 24小时持续运行验证内存泄漏
