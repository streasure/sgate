# sgate 设计文档

## 1. 项目概述

sgate 是一个高性能游戏网关，作为客户端与逻辑服之间的协议桥梁，负责：
- 多协议接入（TCP/UDP/WebSocket）
- 消息转发（客户端 ↔ 逻辑服）
- 会话管理（连接状态机、用户绑定）
- 组管理（组广播、全服广播）

### 1.1 设计目标

| 目标 | 说明 |
|------|------|
| 高性能 | 百万级 QPS 双向转发能力 |
| 低延迟 | 亚毫秒级消息转发延迟 |
| 高可用 | 单节点故障自动容灾 |
| 可扩展 | 支持水平扩展，线性提升吞吐 |
| 简洁性 | 核心代码精简，易于理解和维护 |

### 1.2 技术选型

| 组件 | 选型 | 理由 |
|------|------|------|
| 网络框架 | gnet v2 | 事件驱动、零拷贝、多核支持 |
| RPC | gRPC | 双向流、高效序列化、跨语言 |
| 序列化 | Protocol Buffers | 紧凑、高效、向后兼容 |
| 服务发现 | etcd | 强一致性、租约机制、Watch 支持 |
| 编程语言 | Go 1.22+ | 并发模型、GC 性能、生态丰富 |

## 2. 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                        客户端集群                            │
│   (Game Client / Mobile App / Web Client)                   │
└──────────────────────┬──────────────────────────────────────┘
                       │ TCP/UDP/WebSocket
                       │ [4字节长度][MessageFrame{cmd, seq_id, body}]
                       ▼
┌─────────────────────────────────────────────────────────────┐
│                     sgate Gateway                           │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  gnet Event Loop (多核)                              │   │
│  │  ├── OnOpen: 创建 Session                           │   │
│  │  ├── OnTraffic: 解析帧 → 路由 → 转发                │   │
│  │  ├── OnClose: 清理 Session + 通知 Logic              │   │
│  │  └── OnTick: 定时任务                                │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  SessionManager                                      │   │
│  │  ├── sessions: map[sessionID]*Session                │   │
│  │  └── byConn: map[gnet.Conn]*Session                  │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  GroupManager                                        │   │
│  │  └── groups: map[groupID]*group                      │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  GRPCServer                                          │   │
│  │  ├── GatewayStream (双向流)                          │   │
│  │  └── Gateway (Unary RPC: SendToClient/Broadcast等)   │   │
│  └─────────────────────────────────────────────────────┘   │
└──────────────────────┬──────────────────────────────────────┘
                       │ gRPC Bidirectional Stream
                       │ StreamData{session_id, user_key, cmd, data}
                       ▼
┌─────────────────────────────────────────────────────────────┐
│                    Logic Server 集群                         │
│   ┌──────────┐  ┌──────────┐  ┌──────────┐                │
│   │ logic-1  │  │ logic-2  │  │ logic-3  │                │
│   └──────────┘  └──────────┘  └──────────┘                │
└─────────────────────────────────────────────────────────────┘
```

### 2.1 核心组件职责

| 组件 | 文件 | 职责 |
|------|------|------|
| Gateway | `gateway.go` | 核心网关，gnet 事件处理，帧编解码，消息转发 |
| SessionManager | `session.go` | 连接管理，状态机，用户绑定 |
| GroupManager | `groups.go` | 组生命周期管理，组内广播 |
| GRPCServer | `grpc_server.go` | gRPC 服务，Logic 连接管理，双向流处理 |
| TransportComponent | `transport_component.go` | 传输层启动，gnet 配置 |

## 3. 协议设计

### 3.1 客户端 ↔ Gateway：MessageFrame

客户端与 Gateway 之间使用 `MessageFrame` 作为线路协议：

```protobuf
message MessageFrame {
    int32 cmd    = 1;   // 指令号
    int64 seq_id = 2;   // 序列号（请求-响应配对）
    bytes body   = 99;  // 业务载荷（序列化的 StreamData）
}
```

**线路格式**：
```
[4字节大端长度][MessageFrame protobuf 字节]
```

**设计要点**：
- 固定 3 个字段，protowire 零拷贝解析
- `body` 内部是业务数据的序列化字节
- 长度前缀支持高效帧解析，避免半包问题

### 3.2 Gateway ↔ Logic：StreamData

Gateway 与 Logic 之间使用 `GatewayStream.onData` 双向 gRPC 流：

```protobuf
service GatewayStream {
    rpc onData(stream StreamData) returns (stream StreamData);
}

message StreamData {
    string session_id = 1;   // 连接会话 ID
    string user_key   = 2;   // 用户唯一标识（登录后填充）
    int32  cmd        = 3;   // 指令号
    int64  seq_id     = 4;   // 序列号
    bytes  data       = 5;   // 业务载荷
    string client_ip  = 6;   // 客户端 IP
}
```

**设计要点**：
- `session_id` 用于标识客户端连接，支持 Logic 主动推送
- `user_key` 用于用户级推送（跨连接）
- `cmd` 用于路由到具体业务处理器
- `data` 承载业务数据，Gateway 不解析内容

### 3.3 Gateway ↔ Logic：Unary RPCs

Logic 可通过 Unary RPC 主动操作 Gateway：

| RPC | 说明 |
|-----|------|
| `SendToClient` | 向指定客户端推送消息 |
| `Broadcast` | 向指定组广播消息 |
| `BroadcastAll` | 全服广播 |
| `JoinGroup` | 客户端加入组 |
| `LeaveGroup` | 客户端离开组 |
| `CloseSession` | 关闭客户端连接 |
| `KickSession` | 踢下线 |
| `GetGroupInfo` | 查询组信息 |

### 3.4 指令号定义

```protobuf
enum Cmd {
    CMD_UNSPECIFIED       = 0;
    
    // Gateway 控制 (1,000,000 - 1,099,999)
    CMD_LOGIN_GATE_REQ    = 1000001;  // 登录网关请求
    CMD_LOGIN_GATE_ACK    = 1000002;  // 登录网关响应
    
    // Logic 业务 (1,100,000 - 1,199,999)
    CMD_LOGIN_REQ         = 1100001;  // 业务登录请求
    CMD_LOGIN_ACK         = 1100002;  // 业务登录响应
    CMD_HEARTBEAT_REQ     = 1100010;  // 心跳请求
    CMD_HEARTBEAT_ACK     = 1100011;  // 心跳响应
    CMD_USER_OFFLINE      = 1100012;  // 用户下线通知
}
```

## 4. 数据流设计

### 4.1 正向流（客户端 → 逻辑服）

```
1. 客户端发送: [4字节长度][MessageFrame{cmd=1000001, body=LoginGateReq}]
2. gnet OnTraffic 触发
3. Peek 读取缓冲区
4. 解析 4 字节长度前缀
5. ExtractMessageFrame 提取 cmd/seq_id/body
6. 检查 cmd 是否为 LoginGateReq
   - 是: handleLoginGate 处理登录
   - 否: SendToLogic 转发
7. SendToLogic 构造 StreamData
8. LogicConn.sendCh <- msg
9. sendLoop 异步发送到 Logic
```

### 4.2 反向流（逻辑服 → 客户端）

```
1. Logic 调用 SendToClient/Broadcast 等 RPC
2. GRPCServer 收到请求
3. Gateway.SendToClient/Broadcast 处理
4. 按 sessionID 查找连接
5. EncodeMessageFrame 编码响应
6. [4字节长度][MessageFrame] 写入连接
7. gnet AsyncWrite 异步发送
```

### 4.3 批量处理

**OnTraffic 批量解析**：
```go
for {
    buf, err := c.Peek(-1)
    if err != nil || len(buf) < 4 {
        break
    }
    bodyLen := int(binary.BigEndian.Uint32(buf[:4]))
    if bodyLen <= 0 || bodyLen+4 > len(buf) {
        break
    }
    frame := buf[4 : 4+bodyLen]
    c.Discard(4 + bodyLen)
    handleFrame(sess, frame)
}
```

**设计要点**：
- 一次 OnTraffic 可能包含多帧
- 循环解析直到缓冲区不足
- 减少事件循环次数，提升吞吐

## 5. 会话管理

### 5.1 Session 状态机

```
┌─────────────────┐
│ StateConnected  │  TCP 已连接，未认证
│ (state = 0)     │
└────────┬────────┘
         │ LoginGateReq + LoginGateAck
         ▼
┌─────────────────┐
│ StateBound      │  已绑定 Logic Server
│ (state = 1)     │
└────────┬────────┘
         │ Logic 返回 UserKey
         ▼
┌─────────────────┐
│ StateAuthenticated │ 已完成用户认证
│ (state = 2)     │
└─────────────────┘
```

### 5.2 Session 结构

```go
type Session struct {
    conn      gnet.Conn      // 底层连接
    id        string         // 会话 ID (UUID)
    ip        string         // 客户端 IP
    state     SessionState   // 状态机
    serverID  string         // 绑定的 Logic Server
    userID    string         // 客户端用户 ID
    userKey   string         // Logic 分配的用户 Key
    groups    map[string]bool // 加入的组
}
```

### 5.3 SessionManager

```go
type SessionManager struct {
    sessions map[string]*Session      // sessionID → Session
    byConn   map[gnet.Conn]*Session   // conn → Session
    mu       sync.RWMutex
}
```

**查询操作**：
- `GetByID(id)`: O(1) 按 sessionID 查询
- `GetByConn(conn)`: O(1) 按连接查询
- `Range(fn)`: O(N) 遍历所有 session

**写操作**：
- `Add(s)`: O(1) 添加 session
- `Remove(conn)`: O(1) 移除 session

## 6. 组管理

### 6.1 隐式组生命周期

组的创建和删除是隐式的：
- **创建**: 第一个成员 Join 时自动创建
- **删除**: 最后一个成员 Leave 时自动删除

```go
func (m *GroupManager) Join(groupID string, sess *Session) int {
    g, ok := m.groups[groupID]
    if !ok {
        g = &group{id: groupID, members: make(map[string]*Session)}
        m.groups[groupID] = g
    }
    g.members[sess.ID()] = sess
    return len(g.members)
}

func (m *GroupManager) Leave(groupID string, sess *Session) int {
    g, ok := m.groups[groupID]
    if !ok {
        return 0
    }
    delete(g.members, sess.ID())
    count := len(g.members)
    if count == 0 {
        delete(m.groups, groupID)  // 空组自动删除
    }
    return count
}
```

### 6.2 组广播

```go
func (g *Gateway) SendToGroup(groupID string, cmd int32, data []byte, excludeSessionIDs ...string) {
    exclude := make(map[string]bool, len(excludeSessionIDs))
    for _, id := range excludeSessionIDs {
        exclude[id] = true
    }
    g.groups.RangeSessions(groupID, func(sess *Session) bool {
        if !exclude[sess.ID()] {
            SendFrameToConn(sess.Conn(), cmd, 0, data)
        }
        return true
    })
}
```

### 6.3 全服广播

```go
func (g *Gateway) Broadcast(cmd int32, data []byte) {
    g.sessions.Range(func(sess *Session) bool {
        SendFrameToConn(sess.Conn(), cmd, 0, data)
        return true
    })
}
```

## 7. gRPC 连接管理

### 7.1 LogicConn 结构

```go
type LogicConn struct {
    serverID string                           // Logic Server ID
    conn     *grpc.ClientConn                 // gRPC 连接
    stream   protoGw.GatewayStream_OnDataClient // 双向流
    sendCh   chan *protoGw.StreamData          // 发送队列
    cancel   context.CancelFunc               // 取消函数
}
```

### 7.2 连接建立

```
1. 客户端发送 LoginGateReq 指定 serverID
2. Gateway 检查是否已连接该 Logic
3. 若未连接，调用 ConnectLogic 建立连接：
   a. grpc.NewClient 创建连接
   b. NewGatewayStreamClient.OnData 打开双向流
   c. 启动 sendLoop 和 receiveLoop goroutine
4. 更新 logicClients 映射
```

### 7.3 消息收发

**发送循环 (sendLoop)**：
```go
func (lc *LogicConn) sendLoop(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case msg := <-lc.sendCh:
            lc.stream.Send(msg)
        }
    }
}
```

**接收循环 (receiveLoop)**：
```go
func (lc *LogicConn) receiveLoop(ctx context.Context) {
    for {
        msg, err := lc.stream.Recv()
        if err != nil {
            return
        }
        if msg.SessionId != "" {
            lc.gw.SendToClient(msg.SessionId, msg.Cmd, msg.Data)
        }
    }
}
```

## 8. 帧编解码

### 8.1 零拷贝解析

```go
func ExtractMessageFrame(data []byte) (cmd int32, seqID int64, body []byte, ok bool) {
    for len(data) > 0 {
        num, typ, n := protowire.ConsumeTag(data)
        if n < 0 {
            return 0, 0, nil, false
        }
        data = data[n:]
        switch num {
        case 1: // cmd
            v, m := protowire.ConsumeVarint(data)
            cmd = int32(v)
            data = data[m:]
        case 2: // seq_id
            v, m := protowire.ConsumeVarint(data)
            seqID = int64(v)
            data = data[m:]
        case 99: // body
            v, m := protowire.ConsumeBytes(data)
            body = v
            data = data[m:]
        default:
            m := protowire.ConsumeFieldValue(num, typ, data)
            data = data[m:]
        }
    }
    return cmd, seqID, body, cmd != 0 && len(body) > 0
}
```

**设计要点**：
- 直接操作 protowire 字节，不反序列化整个 MessageFrame
- 只提取需要的字段（cmd, seq_id, body）
- 减少内存分配和 CPU 开销

### 8.2 编码

```go
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
```

## 9. 性能优化

### 9.1 网络层优化

| 优化项 | 实现 | 效果 |
|--------|------|------|
| 事件驱动 | gnet v2 多核事件循环 | 充分利用多核 CPU |
| 零拷贝解析 | protowire 直接扫描 | 减少内存分配 |
| 异步写入 | AsyncWrite 非阻塞 | 避免写阻塞事件循环 |
| 缓冲区调优 | Socket 4MB, 读写 256KB | 减少系统调用次数 |

### 9.2 协议层优化

| 优化项 | 实现 | 效果 |
|--------|------|------|
| 紧凑编码 | protobuf 3 字段 | 减少序列化开销 |
| 批量解析 | OnTraffic 循环处理 | 减少事件触发次数 |
| 长度前缀 | 4 字节大端长度 | 快速帧边界判断 |

### 9.3 内存优化

| 优化项 | 实现 | 效果 |
|--------|------|------|
| 零分配解析 | ExtractMessageFrame 无分配 | 减少 GC 压力 |
| 预分配缓冲区 | 编码时预估容量 | 减少扩容拷贝 |
| 连接池复用 | gRPC 连接复用 | 减少连接建立开销 |

### 9.4 并发优化

| 优化项 | 实现 | 效果 |
|--------|------|------|
| RWMutex | SessionManager 读写锁 | 支持并发读 |
| 无锁队列 | sendCh channel | 避免锁竞争 |
| 细粒度锁 | Session/Group 独立锁 | 减少锁竞争 |

## 10. 企业级架构（Legacy）

### 10.1 架构概览

企业级架构在简化架构基础上增加了：

```
┌─────────────────────────────────────────────────────────────┐
│                     sgate Gateway (Enterprise)               │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Filter Chain                                        │   │
│  │  ├── PreAuth: IP黑白名单                            │   │
│  │  ├── Auth: JWT 鉴权                                 │   │
│  │  ├── PostAuth: 限流、熔断、WAF                       │   │
│  │  └── Forward: 完整性校验、路由                        │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Overload Protector                                  │   │
│  │  ├── CPU 监控                                        │   │
│  │  ├── 内存监控                                        │   │
│  │  └── 过载降级                                        │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Observability                                       │   │
│  │  ├── Prometheus Metrics                              │   │
│  │  ├── pprof Profiling                                 │   │
│  │  ├── Health/Readiness/Liveness Probes                │   │
│  │  └── OpenTelemetry Tracing                           │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Traffic Management                                  │   │
│  │  ├── Canary Release                                  │   │
│  │  ├── Traffic Mirroring                               │   │
│  │  ├── Degradation Rules                               │   │
│  │  └── WASM Plugins                                   │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Cluster Management                                  │   │
│  │  ├── etcd Service Discovery                          │   │
│  │  ├── Leader Election                                 │   │
│  │  ├── Load Balancing                                  │   │
│  │  └── Alert Webhooks                                  │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### 10.2 安全防护

| 组件 | 功能 | 配置 |
|------|------|------|
| IP 白名单/黑名单 | 按 IP 过滤连接 | `security.whitelist/blacklist` |
| 限流 | 令牌桶限流 | `security.rateLimit` |
| 熔断器 | 失败熔断、半开恢复 | `security.circuitBreaker` |
| WAF | SQL 注入/XSS 检测 | `waf.enabled` |
| JWT | Token 鉴权 | `jwtAuth.enabled` |
| TLS | 加密传输 | `tls.enabled` |

### 10.3 可观测性

| 组件 | 功能 | 端点 |
|------|------|------|
| Prometheus | 指标采集 | `/metrics` |
| pprof | 性能分析 | `:6060/debug/pprof/` |
| Health | 健康检查 | `/health` |
| Readiness | 就绪检查 | `/ready` |
| Liveness | 存活检查 | `/live` |
| Tracing | 分布式追踪 | OpenTelemetry/Zipkin |

### 10.4 流量管理

| 组件 | 功能 | 配置 |
|------|------|------|
| 灰度发布 | 按百分比/用户灰度 | `canary.enabled` |
| 流量镜像 | 录制/回放流量 | `trafficMirror.enabled` |
| 降级规则 | 错误阈值触发降级 | `degradation.rules` |
| WASM 插件 | 自定义过滤逻辑 | `traffic.wasm` |

## 11. 部署架构

### 11.1 单机部署

```
┌─────────────────────────────┐
│         单机部署             │
│  ┌───────────────────────┐  │
│  │    sgate Gateway       │  │
│  │    (TCP :48080)        │  │
│  │    (gRPC :50051)       │  │
│  │    (metrics :9100)     │  │
│  └───────────────────────┘  │
│  ┌───────────────────────┐  │
│  │    Logic Server        │  │
│  │    (gRPC :50052)       │  │
│  └───────────────────────┘  │
└─────────────────────────────┘
```

### 11.2 集群部署

```
                    ┌─────────────┐
                    │   etcd      │
                    │   集群      │
                    └──────┬──────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
┌───────▼──────┐  ┌───────▼──────┐  ┌───────▼──────┐
│  Gateway-1   │  │  Gateway-2   │  │  Gateway-3   │
│  (TCP:48080) │  │  (TCP:48080) │  │  (TCP:48080) │
└───────┬──────┘  └───────┬──────┘  └───────┬──────┘
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │ gRPC
        ┌──────────────────┼──────────────────┐
        │                  │                  │
┌───────▼──────┐  ┌───────▼──────┐  ┌───────▼──────┐
│  Logic-1     │  │  Logic-2     │  │  Logic-3     │
│  (gRPC:50052)│  │  (gRPC:50052)│  │  (gRPC:50052)│
└──────────────┘  └──────────────┘  └──────────────┘
```

### 11.3 扩容策略

| 维度 | 策略 | 说明 |
|------|------|------|
| Gateway 水平扩展 | 增加节点 | 客户端通过负载均衡连接 |
| Logic 水平扩展 | 增加节点 | Gateway 通过 etcd 发现 |
| 连接数扩展 | 增加 Gateway 节数 | 每个节点处理部分连接 |
| 吞吐扩展 | 增加 Logic 节数 | 业务处理能力线性提升 |

## 12. 配置管理

### 12.1 配置加载

```go
func LoadConfig(configFiles ...string) (*Config, error) {
    // 1. 加载默认配置
    cfg := loadDefaultConfig()
    
    // 2. 从 yaml 文件覆盖
    if file != nil {
        yaml.NewDecoder(file).Decode(cfg)
    }
    
    return cfg, nil
}
```

**合并语义**：
- yaml 中显式出现的字段，覆盖默认值
- yaml 中未出现的字段，保留默认值
- 支持环境变量覆盖

### 12.2 核心配置项

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `transports[].port` | 8080 | TCP 监听端口 |
| `grpc.port` | 50051 | gRPC 服务端口 |
| `grpc.windowSize` | 16MB | gRPC 窗口大小 |
| `stream.shardCount` | 0 | 流分片数（0=不分片） |
| `protection.cpuThreshold` | 90% | CPU 过载阈值 |

## 13. 错误处理

### 13.1 错误码定义

```go
// Gateway 级错误
var (
    ErrInvalidFrame    = errors.New("invalid frame")
    ErrFrameTooLarge   = errors.New("frame too large")
    ErrNotConnected    = errors.New("not connected to logic")
    ErrSessionNotFound = errors.New("session not found")
    ErrGroupNotFound   = errors.New("group not found")
)

// Protocol 级错误
var (
    ErrUnauthorized  = errors.New("unauthorized")
    ErrRateLimited   = errors.New("rate limited")
    ErrCircuitOpen   = errors.New("circuit breaker open")
)
```

### 13.2 错误恢复

| 场景 | 处理策略 |
|------|----------|
| Logic 连接断开 | 自动重连，通知客户端 |
| 客户端断开 | 清理 Session，通知 Logic |
| 消息发送失败 | 丢弃消息，记录日志 |
| 内存溢出 | 触发过载保护，拒绝新连接 |

## 14. 监控指标

### 14.1 核心指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `connections_total` | Counter | 总连接数 |
| `connections_active` | Gauge | 活跃连接数 |
| `messages_received` | Counter | 收到消息数 |
| `messages_forwarded` | Counter | 转发消息数 |
| `messages_pushed` | Counter | 推送消息数 |

### 14.2 性能指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `forward_qps` | Gauge | 转发 QPS |
| `push_qps` | Gauge | 推送 QPS |
| `latency_p50` | Histogram | 50 分位延迟 |
| `latency_p99` | Histogram | 99 分位延迟 |

## 15. 测试策略

### 15.1 单元测试

- `ExtractMessageFrame` 帧解析测试
- `EncodeMessageFrame` 帧编码测试
- `SessionManager` 会话管理测试
- `GroupManager` 组管理测试

### 15.2 集成测试

- 完整登录流程测试
- 心跳保活测试
- 组广播测试
- 全服广播测试

### 15.3 性能测试

- 双向 QPS 压测
- 推送 QPS 压测
- 高并发连接测试
- 长时间稳定性测试

## 16. 未来规划

### 16.1 短期优化

- [ ] 批量转发优化（RouteBatch）
- [ ] 写合并优化（Writev）
- [ ] gRPC 流分片
- [ ] 连接池优化

### 16.2 中期规划

- [ ] WebSocket 支持完善
- [ ] UDP 支持
- [ ] 消息压缩
- [ ] 消息加密

### 16.3 长期规划

- [ ] 多租户支持
- [ ] 跨区域部署
- [ ] AI 流量分析
- [ ] 自适应限流
