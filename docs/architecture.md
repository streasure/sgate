# sgate 架构设计

本文档描述 sgate 网关的包结构、分层设计、生命周期管理和核心数据流。

---

## 1. 分层架构

```text
┌─────────────────────────────────────────────────────────┐
│                    cmd/gateway/main.go                   │
│         唯一组装点：Container + FilterChain + Version     │
└────────────────────────┬────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────┐
│              internal/gateway (接入层)                    │
│  Gateway struct · gnet handlers · login · websocket      │
│  pipeline · filter · monitor · overload · integrity      │
└────────────────────────┬────────────────────────────────┘
                         │ 依赖
┌────────────────────────▼────────────────────────────────┐
│              internal/backend (后端层)                    │
│  LogicClient · LogicClientPool · StreamManager           │
│  GRPCServer · GatewayClientPool · GatewayInterface       │
└────────────────────────┬────────────────────────────────┘
                         │ 依赖
┌────────────────────────▼────────────────────────────────┐
│            internal/connection (连接层)                   │
│  Connection · ConnectionManager · Group · ShardedMap      │
│  ShardedWriteCoalescer · EncodeWSFrame · Providers        │
└─────────────────────────────────────────────────────────┘
```

**依赖方向（严格单向）：**

```text
gateway → backend → connection
   │         │
   │         └──→ routes（帧编解码、命令码）
   └──→ routes, connection, backend, component, config, ...
```

禁止：`connection` 引用 `backend` 或 `gateway`；`backend` 引用 `gateway`。

---

## 2. 核心包职责

### 2.1 internal/gateway

Gateway 结构体承载全部接入侧状态：

| 职责 | 文件 | 关键类型/方法 |
| --- | --- | --- |
| 生命周期 | gateway.go | `NewGateway()`, `Init/Start/Destroy`, `StartServices` |
| gnet 事件 | handlers.go | `OnOpen/OnTraffic/OnClose`（原 conn.go） |
| 登录 | login.go | `handleLoginGate`, session 绑定 |
| WebSocket | websocket.go | 握手升级、帧编解码 |
| 消息管道 | pipeline.go | 认证→安全→过滤→转发 |
| 过滤器 | filter.go | SPI 加载、`types.GetFilterChain()` |
| 监控 | monitor.go | `/stats`、Prometheus、配置热更新 |
| 过载保护 | overload.go | `OverloadProtector`、CPU 阈值 |
| 完整性 | integrity.go | `MessageIntegrity`（时间戳、去重） |
| 版本 | version.go | `Version`、`BuildInfo` |

**GatewayInterface 实现**：gateway 包通过编译期断言确保 `*Gateway` 实现 `backend.GatewayInterface`，供 backend 反向调用（查连接、取配置、推送计数等），避免 backend→gateway 的包依赖。

### 2.2 internal/backend

| 类型 | 文件 | 职责 |
| --- | --- | --- |
| `LogicClient` | client.go | 单逻辑服连接：流分片、发送、健康检查、重连 |
| `LogicClientPool` | pool.go | 多逻辑服管理：etcd 发现、负载均衡、地址缓存 |
| `StreamManager` / `StreamShard` | stream.go | gRPC 流分片：按 session 分片降低锁竞争 |
| `LogicConnectionState` 等 | state.go | 连接状态机、`Err*` 错误、重连/健康检查配置 |
| `GRPCServer` | grpc_server.go | 逻辑服推送入口、RPC（Kick/Broadcast/Group） |
| `GatewayClientPool` | gateway_client.go | 网关间 gRPC 客户端池（cluster 模式） |
| `GatewayInterface` | iface.go | backend 对 gateway 的能力依赖（消费方定义接口） |

### 2.3 internal/connection

| 类型 | 文件 | 职责 |
| --- | --- | --- |
| `Connection` | connection.go | 客户端连接：session、状态、消息速率计数 |
| `ConnectionManager` | manager.go | 分片存储、`ForEach`、空闲连接清理 |
| `ConnectionGroup` | group.go | 组成员管理（广播/组播） |
| `ShardedMap` | sharded_map.go | 分片并发 Map（降低锁竞争） |
| `ShardedWriteCoalescer` | coalescer.go | 推送路径分片写合并（批量优化） |
| `EncodeWSFrame` | ws_frame.go | WebSocket 帧编码 |
| `LogicClientProvider` | iface.go | 逻辑服客户端能力接口（消费方定义） |

### 2.4 internal/routes

- `routes.go`：命令码常量（`CmdLoginGate`、`CmdPushBatch` 等）、路由解析、`ExtractMessageFrame`
- `frame.go`：`DecodeClientMessage` / `MarshalClientMessage` / `MarshalClientError` / `NewErrorResponse`

routes 被 gateway 和 backend 共同依赖，承载协议帧编解码，避免循环依赖。

### 2.5 其他支撑包

| 包 | 职责 |
| --- | --- |
| `component` | 组件容器、扁平生命周期、全局资源（`resources.go`） |
| `config` | 配置结构、默认值、`config.Get()` 热读取 |
| `security` | 限流、熔断、WAF、JWT、黑白名单 |
| `traffic` | 灰度、镜像、降级、eBPF/WASM |
| `cluster` | 集群模式、负载均衡、Leader 选举 |
| `obs` | 追踪、pprof、健康检查、延迟分位数 |
| `codec` | TCP/WS 编解码器（含池化） |
| `types` | 过滤器链、公共类型定义 |

---

## 3. 生命周期与组件模型

### 3.1 组件扁平创建

```go
// cmd/gateway/main.go — 唯一组装点
container := component.NewContainer()
container.Add(comp.NewSecurityComponent())    // Order 100
container.Add(comp.NewObservabilityComponent())// Order 200
container.Add(comp.NewTrafficComponent())      // Order 300
container.Add(comp.NewClusterComponent())      // Order 400
container.Add(gateway.NewGateway())            // Order 1000
container.Serve()
```

**Order 约定：**

| Order | 组件 | 说明 |
| --- | --- | --- |
| 100 | Security | 安全链先就绪 |
| 200 | Obs | 日志/追踪/指标 |
| 300 | Traffic | 流量治理 |
| 400 | Cluster | 服务发现、负载均衡 |
| 1000 | Gateway | 接入层最后启动 |

### 3.2 构造函数零参数

所有组件构造函数不接受参数，内部通过 `config.Get()` 读取配置：

```go
func NewGateway() *Gateway { ... }           // 无参数
func NewLogicClient(g GatewayInterface) *LogicClient { ... }  // 仅消费方接口
```

### 3.3 全局资源模式

组件 Init/Start 阶段写入 `internal/component/resources.go` 导出的全局变量；Gateway 通过 getter 读取：

```go
// 组件写入
resources.SetRateLimiter(...)

// Gateway 读取
resources.GetRateLimiter().Allow(...)
```

### 3.4 全局过滤器链

```go
// main.go 初始化一次
fc := types.InitFilterChain()
for _, fi := range cfg.FilterChain.Filters {
    fc.LoadByName(fi.Name, fi.Config)
}

// 组件注册过滤器
types.GetFilterChain().AddFilter(myFilter)
```

---

## 4. 核心数据流

### 4.1 上行：客户端 → 逻辑服

```text
1. gnet OnTraffic (handlers.go)
2. TCP: 长度前缀解码 / WS: 帧解码 (codec)
3. decode MessageFrame (routes.ExtractMessageFrame)
4. MessagePipeline (pipeline.go)
   ├─ 过载检查 (overload)
   ├─ 认证/会话 (login)
   ├─ 安全链 (security: 限流/WAF/JWT)
   ├─ 过滤器链 (types.FilterChain)
   └─ 连接级流控 (Connection.CheckAndIncrementMsgRate)
5. LogicClientProvider.SendMessage (connection iface)
6. StreamShard 按 session 分片入队 (backend/stream)
7. gRPC stream.Send → Logic
```

### 4.2 下行：逻辑服 → 客户端

```text
1. GRPCServer.OnData (backend/grpc_server)
2. 查找连接: connectionManager.Get(sessionID)
3. 编码: routes.MarshalClientMessage / EncodeWSFrame
4. 可选批量: ShardedWriteCoalescer 合并写 (connection/coalescer)
5. gnet 异步写回客户端
```

### 4.3 登录流程

```text
Client → CmdLoginGate(1000001) LoginGateReq
  → pipeline 认证前命令白名单 (preAuthCommands)
  → login.handleLoginGate
  → 分配 sessionID、绑定 Connection
  → 可选转发 logic: CmdLogicLoginReq(1100001)
Logic → CmdLogicLoginAck(1100002)
  → Client ← CmdLoginGateAck(1000002)
```

---

## 5. 并发与性能设计

### 5.1 gRPC 流分片

- 默认 `shardCount = CPU × 8`（约 96 on 12C）
- 按 `sessionID` 哈希选择 shard，同连接消息有序
- 每 shard 独立 send channel + 发送协程，降低锁竞争

### 5.2 多 TCP 连接组（connGroupCount）

- gateway→logic 建立 N 条独立 TCP 连接（默认 4）
- 每条连接承载 `shardCount/N` 个 stream
- HTTP/2 同连接 stream 共享写锁 → 多连接实现真正并行写
- 实测吞吐提升 76-77%

### 5.3 分片写合并（ShardedWriteCoalescer）

- 推送路径（logic→client）按连接分片
- 短时间内多条推送合并为一次系统调用
- `batchPush=true` 时配合 `PushBatch` 命令进一步合并

### 5.4 分片连接存储

- `ConnectionManager` 使用分片 Map（非单把锁 sync.Map）
- 热路径读（按 sessionID 查连接）几乎无锁

---

## 6. 接口设计

### 6.1 消费方定义接口（依赖倒置）

| 接口 | 定义位置 | 实现方 | 消费方 |
| --- | --- | --- | --- |
| `GatewayInterface` | backend/iface.go | `*gateway.Gateway` | LogicClient、GRPCServer、Pools |
| `LogicClientProvider` | connection/iface.go | `*backend.LogicClient` | pipeline、Connection 缓存 |
| `GatewayClientProvider` | backend/iface.go | `*backend.GatewayClient` | 跨网关推送 |

**好处**：backend 不 import gateway，避免循环依赖；测试时可 mock。

### 6.2 编译期断言

```go
// gateway/gateway.go
var _ backend.GatewayInterface = (*Gateway)(nil)
```

---

## 7. 配置与热更新

- **冷启动**：`config.Load()` → `config.Set()` → 组件通过 `config.Get()` 读取
- **热更新**：`configWatcher` 监听文件 → `handleConfigUpdate` 应用差异
- **支持热更的参数**：限流阈值、黑名单、过载保护、JWT、灰度、镜像、降级、连接限制、连接级流控

---

## 8. 扩展点

| 扩展 | 方式 |
| --- | --- |
| 新过滤器 | 实现 `types.Filter`，在配置 `filterChain.filters` 中加载 |
| 新组件 | 实现 `Name/Order/Init/Start/Destroy`，在 main `container.Add` |
| 新推送策略 | 替换/包装 `ShardedWriteCoalescer` 或调整 `batchPush` |
| 新路由 | `routes` 添加命令码 + pipeline/handler 注册处理函数 |

---

## 9. 相关文档

- [README.md](../README.md) — 快速开始、配置详解、压测指南
- [benchmark-report.md](benchmark-report.md) — 压测数据与瓶颈分析
- [roadmap.md](roadmap.md) — 待办与已修复列表
- [AGENTS.md](../AGENTS.md) — AI 工程规范（日志等）
