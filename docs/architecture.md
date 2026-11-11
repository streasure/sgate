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
│  GRPCServer · GatewayInterface                            │
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
| 登录 | login.go | `handleLoginGate`、`initLoginService`（loginserver 发现+服务客户端，网关自持）、session 绑定 |
| 登出/管理端 | admin.go | `handleLogoutGate`（JWT Revoke+通知逻辑服+`SendThenClose` 可靠投递 ack 后断连）、`POST /admin/ratelimit`（Bearer，`admin.token` 空=关闭） |
| 维护模式 | maintenance.go | `handleAdminMaintenance`：按 serverId/zone 置维护，阻断新登录并踢出在线玩家（路由在 admin.go 注册） |
| 踢人执行器 | kick.go | `kickConn`/`pushKickNotify`：loginserver 单向下发 KickAccounts/KickIP 的本地执行端（sgate 无封禁存储）；KickAccounts 按账号索引踢**全部**在线连接（同账号多逻辑服连接不漏踢） |
| 登录周期校验 | batch_validate.go | BatchValidate 周期安全网（`loginValidation.batchInterval`，P2） |
| WebSocket | websocket.go | 握手升级、帧编解码 |
| 消息管道 | pipeline.go | 认证→安全→过滤→转发 |
| 过滤器 | filter.go | SPI 加载、`types.GetFilterChain()` |
| 监控 | monitor.go | `/stats`、配置热更新 |
| 过载保护 | overload.go | `OverloadProtector`、CPU 阈值 |
| 完整性 | integrity.go | `MessageIntegrity`（时间戳、按 connectionID 分 16 片去重，防全局锁串行化热路径） |
| 版本 | version.go | `Version`、`BuildInfo` |

**GatewayInterface 实现**：gateway 包通过编译期断言确保 `*Gateway` 实现 `backend.GatewayInterface`，供 backend 反向调用（查连接、取配置、推送计数等），避免 backend→gateway 的包依赖。

### 2.2 internal/backend

| 类型 | 文件 | 职责 |
| --- | --- | --- |
| `LogicClient` | client.go | 单逻辑服连接：流分片、发送、断线缓存 |
| `LogicClientPool` | pool.go | 多逻辑服管理：接入、负载均衡、地址缓存 |
| `StreamManager` / `StreamShard` | stream.go | gRPC 流分片：按 session 分片降低锁竞争，StreamBatch 合帧 |
| `LogicConnectionState` 等 | state.go | 连接状态机、`Err*` 错误定义 |
| `GRPCServer` | grpc_server.go | 逻辑服 flip 流入口、Gateway/Kick unary RPC |
| `GatewayInterface` | iface.go | backend 对 gateway 的能力依赖（消费方定义接口） |

**连接模型选型**（发现侧接入统一走 util 的 `ugrpc.Source` 插件契约，胶水在 `util/udiscovery`）：

| 场景 | 选择 | 说明 |
| --- | --- | --- |
| 逻辑服连接（分片、按 serverID 路由、会话粘性） | `LogicClientPool`（backend 自有） | flip 模式：logic 主动拨入，地址来自握手元数据，非 etcd 拨号 |
| 无状态服务、单连接多实例负载均衡（如 loginserver 校验） | `ugrpc.ServiceClient`（`udiscovery.DialEtcdService`） | resolver 热切换地址，round_robin；`gateway/login.go initLoginService` 即此形态 |
| etcd 拨号、每实例一条连接、按 instanceID 路由 | `ugrpc.Pool.Attach(source)` | 同一 `Source()` 可多路消费（sgate 现未使用，供扩展） |

### 2.3 internal/connection

| 类型 | 文件 | 职责 |
| --- | --- | --- |
| `Connection` | connection.go | 客户端连接：session、状态、消息速率计数 |
| `ConnectionManager` | manager.go | 分片存储、账号→连接集合索引（KickAccounts）、`ForEach`、空闲连接清理；session ID 生成/解析（`GenerateConnectionID`/`ParseSessionGateway`，自描述 ID §4.6） |
| `ConnectionGroup` | group.go | 组成员管理（广播/组播） |
| `ShardedMap` | sharded_map.go | 分片并发 Map（降低锁竞争） |
| `ShardedWriteCoalescer` | coalescer.go | 推送路径分片写合并（批量优化） |
| `EncodeWSFrame` | ws_frame.go | WebSocket 帧编码 |
| `LogicClientProvider` | iface.go | 逻辑服客户端能力接口（消费方定义） |

### 2.4 internal/routes

- `routes.go`：命令码常量（`CmdLoginGate`、`CmdPushBatch` 等）、路由解析、`ExtractMessageFrame`
- `frame.go`：`DecodeClientMessage` / `MarshalClientMessage` / `MarshalClientError` / `NewErrorResponse`
- `logicstream.go`：flip 握手元数据（`MetaLogicID`/`MetaShardIdx` 等）与 `LogicStreamMeta` 解析

routes 被 gateway 和 backend 共同依赖，承载协议帧编解码，避免循环依赖。

### 2.5 其他支撑包

| 包 | 职责 |
| --- | --- |
| `component` | 组件容器、扁平生命周期、全局资源（`resources.go`） |
| `config` | 配置结构、默认值、`config.Get()` 热读取 |
| `security` | 限流、熔断、WAF、JWT、黑白名单 |
| `traffic` | 灰度、镜像、降级、WASM |
| `cluster` | etcd 自注册（registerSelf）、配置中心、告警 webhook |
| `obs` | 追踪、pprof、健康检查、延迟分位数 |
| `types` | 过滤器链、公共类型定义 |

### 2.6 internal/logic（逻辑服侧嵌入库，网关不依赖）

`internal/logic` 是提供给逻辑服进程嵌入的 SDK，`internal/gateway`、`internal/backend` 均不 import 它（不参与上述分层依赖）。它封装：gRPC 服务端（注册 `GatewayStream` 服务）、etcd 注册（键 `{belong}/SERVER_TYPE_LOGICSERVER:{zone}`）、`Dialer` flip 拨入（logic 主动连网关，地址来自 etcd 发现或 `gateways` 静态配置），对外提供 `logic.NewService(...)` / `svc.Server()`。本仓由 bench 模拟逻辑服（`bench/logic1_*`、`bench/logic2_*` 等）消费；网关侧与之对接的是 `internal/backend` 的 `GRPCServer` flip 流入口。详见 [README §十](../README.md#十逻辑层-api)。

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
| 400 | Cluster | etcd 自注册、配置中心、告警 webhook（登录发现由 Gateway 自持，不在此组件） |
| 1000 | Gateway | 接入层最后启动 |

### 3.2 构造函数零参数

所有组件构造函数不接受参数，内部通过 `config.Get()` 读取配置：

```go
func NewGateway() *Gateway { ... }           // 无参数
func NewLogicClient(gateway GatewayInterface) *LogicClient { ... }  // 仅消费方接口
```

### 3.3 全局资源模式

组件 Init/Start 阶段经 `internal/component/resources.go` 的包内 setter（如 `setSecurityResources`，未导出）写入全局变量；外部（含 Gateway）只能通过同文件导出的 getter 读取：

```go
// 组件写入（component 包内部，security_component.go Init）
setSecurityResources(wb, waf, rl, jwt, cb)

// Gateway 读取（package component 导出的 getter，gateway.go Init）
component.RateLimiter().Allow(dimension, key)
```

例外：loginserver 发现不经全局资源——`Gateway.Start` 经 `udiscovery.DialEtcdService` 自持
（`login.go initLoginService`），`closeBody` 对称关闭。

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
2. TCP: 长度前缀切帧 / WS: 帧解析 (handlers.go / websocket.go)
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

每一步都可能丢（gRPC 流断、无连接、异步写失败），但**每一步的"成功"都不等于客户端收到**。因此丢不起的消息走端到端确认（§4.5）；其余消息依赖"下一帧覆盖"自愈。

### 4.5 可靠推送：端到端 ACK（logic ↔ 客户端）

**为什么 ACK 必须建在端到端**：投递好比寄挂号信——每段传送带（TCP、gRPC）只保证"把包裹交到下一段手里"，任何一段说"我送出去了"都不等于收件人签收。逐跳各建一层 ACK（网关停一层、gRPC 停一层）修不掉任何真实缺口，只是每层各造一套窗口/定时器/重传状态。唯一能证明送达的只有收件人回签收单，所以 ACK 建在 logic ↔ 客户端，**网关保持纯管道、零新状态**。

```text
PushReliable(sessionID, cmd, data)          ← logic 业务侧（opt-in）
  → 按会话单调分配 SeqId 写入 StreamData.SeqId   （复用既有字段，wire 零改动）
  → 入该会话未确认窗口（默认 64，满则返回 ErrReliableWindowFull 背压）
  → 现有下行链路原样转发（MarshalClientMessage / PushBatch 透传 SeqId）

客户端 → CmdMsgAck(1100013) MsgAck{seq_ids}  ← 普通上行走现有 pipeline（网关零改动）
  → logic builtinCommand 批量移出窗口（重复/迟到 ACK 幂等）

超时未收 ACK → janitor 扫描重传（指数退避 300ms~5s × 5 次，扫描式非每消息 timer）
  → 达上限放弃 + giveUps 计数告警

会话 Offline / 断流 → 清窗（droppedOffline 计数）
  → 会话内 at-least-once；跨会话不续传，重连后走登录全量/增量同步拉取
```

**观测**：logic `PushMetrics`（`acked`/`retransmits`/`giveUps`/`droppedOffline`）与网关 `/stats` 的 `asyncWriteFailed`（gnet 异步写完成回调计数，原 `NoopAsyncCallback` 吞错无感知）互补，覆盖丢包点表的全部环节。配置 `reliability.*`（行为进 yaml，号段与 seq 语义进代码）。详见 [README §10.1](../README.md#101-可靠推送端到端-ack)。

### 4.3 登录流程

```text
Client → CmdLoginGate(1000001) LoginGateReq（LoginGate 走捷径，不经 pipeline）
  → 过载检查 → IP 限流 → 请求解析
  → 并发登录槽 (loginSlots，满则 429)
  → worker 协程 finishLoginGate（避免阻塞 event loop）
      ├─ loginValidation.enabled=true 时经 loginserver ValidateLoginToken（失败 401，fail-closed）
      │    （loginserver 地址经 etcd 发现：initLoginService 单连接 + round_robin 热切换）
      ├─ 绑定 userUUID = {serverId}:{userId}、踢旧连接
      ├─ Client ← CmdLoginGateAck(1000002)（code=0 成功）
      └─ 原始 LoginGate StreamData 带 SessionId/UserKey 异步转发 logic
         （logic 直接读取原始 LoginGateReq，无 CmdLogicLoginReq/Ack 转换）
```

### 4.4 WS+UDP 关联通道（已支持）

**一个会话、两个通道**：WS 是会话与指令面（丢不起的消息），UDP 是状态同步面（高频可丢状态），关联生命周期挂在 WS 会话上。`udp.enabled: false`（默认）时所有下行回落 WS，零行为变化。完整设计与决策记录见 [udp-ws-design.md](udp-ws-design.md)。

**为什么不能让 UDP 单独建会话**：

- gnet UDP **不触发 `OnOpen`**（源码层面每个数据报新建 conn 对象），`ConnectionManager` 中不存在、也无法存在对应连接
- 曾把 `"udp_"+remoteAddr` 当 connectionID 投进 pipeline，必然 `unknown connection`——一次必然失败的完整往返（连 LoginGate 都过不去），且全仓无 UDP 测试
- 现在关闭态在源 IP 限速 + 帧校验 + 解码之后**明确拒绝**：回一帧 `udp transport not supported`（垃圾包静默丢，控制回包放大），计数 `droppedUnsupported` 计入 `/stats` 的 `droppedTotal`；不建连接、不投 pipeline（回归：`internal/gateway/udp_reject_test.go`）

**工作方式**（开启态）：

```text
1. WS/TCP 建连 → LoginGate 登录成功 → LoginGateAck 下发一次性 udpToken
2. 客户端自建 UDP socket（本地临时端口）→ 向网关固定 UDP 端口发 ASSOCIATE{udpToken, nonce}
   （NAT 映射只能由客户端先发包建立：服务器无法主动开 UDP；客户端监听固定端口需端口映射，不可行）
3. 网关验 token（一次性消费，防重放）→ 建关联表 connID ↔ (公网IP:port, lastSeen, seq 窗口)
   → 回 ASSOCIATE_OK → conn.SetUDPDownlink(gw) 注入下行通道
4. 上行：UDP 包按源地址反查真实 connID → seq 滑动窗口去重 → 以真实 connID 投现有
   worker pool，复用 pipeline 全链路（鉴权/限流/过滤器/转发不变，帧体与 TCP/WS 逐字节一致）
5. 下行：backend 各调用点只换 SendByCmd —— connection.SendByCmd 是全系统唯一分流点
   （号段内 && 有关联 && 新鲜 → sendUDPFrame，否则回落 WS，降级不丢）；
   UDP 消息不进 PushBatch、不走 coalescer（一包一消息，逐条发）
6. WS 断开 / UDP 空闲超时（janitor，默认 60s）→ 删除关联；NAT 保活靠客户端 15s 周期发包
```

**依赖方向**：`connection` 定义 `UDPDownlink` 接口（consumer-defined，同 `LogicClientProvider` 模式），`*gateway.Gateway` 实现并在 ASSOCIATE 成功时注入——`gateway → backend → connection` 单向依赖不变；号段判断收敛在 `routes.IsUDPCmd`（纯函数，热路径一次区间比较）。

**安全与工程要点**：token 一次性消费防重放、per-IP/总量双上限防关联表耗尽、未关联数据报静默丢弃（反放大：回包 ≤ 来包，垃圾包零回包）、seq 去重窗口、MTU ≤ 1400B 避免 IP 分片、下行新鲜度条件（`now-lastSeen ≤ idleTimeout/2` 才走 UDP，stale 黑洞窗口砍半且不发起注定投向死地址的 SendTo）。**存活判据只看 WS**：UDP 丢包不断线，弱网玩家不被误踢。

**测试**：`udp_reject_test.go`（关闭态拒绝）、`udp_assoc_test.go`（token 一次性/过期/撤销、关联生命周期、per-IP 与总量上限、空闲清扫、幂等替换）、`udp_uplink_test.go`（pipeline 转发/会话隔离/seq 去重/MTU/埋点）、`udp_send_test.go`（下行分流/新鲜度/回落/-race）、`backend/stream_udp_redirect_test.go`（批量绕过/广播分流/coalescer 绕过）、`connection/send_by_cmd_test.go`（唯一分流点）。

### 4.6 自描述 session ID（跨网关定位）

**问题**：session ID 原为 `conn_{UnixNano}_{counter}`，各网关本地生成——`counter` 每台进程各自从 1 起，两台网关在相同时钟读数下会发重号（虚拟化环境时钟粒度粗时并不罕见），logic 的 `sessions` 内存表会把消息送到错误网关（静默数据错乱）；且外部服务拿到 ID 无法定位所在网关。

**方案**（roadmap P3-1，不建 etcd session 表）：把网关身份直接编进 ID——

```text
格式：conn_{serverID}_{UnixNano}_{counter}
      ↑ config serverID（与 etcd 注册 instanceID 同一标识）

生成：connection.GenerateConnectionID（唯一入口，addConnection 调用）
      newGateway 构造时 connection.SetSessionGatewayID(cfg.ServerID) 注入一次
定位：connection.ParseSessionGateway(id) 解析出 serverID
      → 现有 etcd 服务发现 {belong}/SERVER_TYPE_SGATE:{zone}/{serverId} 取地址
```

**原理**：路由信息不放外部登记表，而是"信封上直接写收件部门"——表会过期、要心跳续租、崩溃后残留要等租约超时清理，且 etcd 定位是低频服务注册、扛不住百万级会话随连接频繁增删的写放大；写在 ID 里随会话同生共死，天然一致、零维护。

**解析规则**：从右取两段纯数字（`counter`、`UnixNano`），其余部分即 `serverID`——`serverID` 含下划线或以数字结尾均可正确还原。旧格式（无 `serverID` 段）或非法输入返回 `""`，不 panic。

**兼容**：`serverID` 为空（测试/工具进程）时 `SetSessionGatewayID` 存空串，生成退化为旧格式 `conn_{UnixNano}_{counter}`，零破坏。

**为什么不建 etcd 表（决策记录）**：消费方盘点——logic 推送靠 flip 模式内存 `sessions` 表（单 zone 已解决）、loginserver 踢人靠 Redis 三键索引 `sess:conn:{sgateId}:{connId}`（已有），etcd 建表无真实消费方；"网关重启需客户端重连"是 TCP 连接本性，属 P3-3 滚动升级，本项不解决也不声称解决。跨 zone 映射存储延后到 P3-2 有真实消费方再定（届时本格式与解析仍可复用）。

**测试**：`connection/session_id_test.go`（跨网关不重号、进程内 1000 条唯一、含下划线/数字结尾 serverID 往返、旧格式与非法输入返回空、未设置退化旧格式）。

---

## 5. 并发与性能设计

### 5.1 gRPC 流分片

- 网关默认 `shardCount = 96`（`config.DefaultStreamShardCount`）；logic 侧拨入默认 `NumCPU × 8`（12C 机器同为 96）
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

**好处**：backend 不 import gateway，避免循环依赖；测试时可 mock。

### 6.2 编译期断言

```go
// gateway/gateway.go
var _ backend.GatewayInterface = (*Gateway)(nil)
```

---

## 7. 配置与热更新

- **冷启动**：`config.Load()` → 全局 `conf` 指针 → 组件通过 `config.Get()` 读取（无 `config.Set`）
- **热更新**：`configWatcher` 监听 `config.Path()` 指定文件 → `handleConfigUpdate` 应用差异
- **支持热更的参数**：限流阈值、黑名单、过载保护、JWT、灰度、镜像、降级、连接限制、连接级流控
- **不可热切的开关**：`security/rateLimit/waf/jwtAuth/circuitBreaker` 的 `enabled`（组件与过滤器仅启动时创建，热切会静默 fail-open/fail-stale）——`handleConfigUpdate` 检测到开关变化整单拒绝并告警，要求重启生效；非法配置（含帧缓冲交叉校验 `maxFrameBufSize ≥ maxFrameSize+4`）同样整体拒绝、保留当前配置

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
