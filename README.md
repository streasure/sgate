# sgate

sgate 是一个基于 gnet v2 的高性能长连接网关。它夹在**客户端**与**游戏逻辑服**之间：北向承接 TCP / WebSocket / UDP 客户端连接，南向通过 gRPC 双向流连接逻辑服，负责登录鉴权、消息管道、流量治理、会话路由与高吞吐转发。

核心设计原则：**逻辑层 API 保持简洁（只管推送），连接管理、协议编解码、服务发现、流量治理和高吞吐转发全部集中在网关层**；新功能一律通过"收敛点"进入系统，不产生旁路、不为单个功能到处撒特殊逻辑。

---

## 功能一览

| 类别 | 能力 |
| --- | --- |
| 接入 | TCP（4 字节长度前缀帧）与 WebSocket（gnet v2 事件驱动，握手/半包累积/分片重组/强制 mask） |
| 双通道 | WS 会话 + UDP 关联通道（`udp.enabled` 默认关闭）：号段 `1200000~1299999` 走 UDP，其余走 WS，降级自动回落 |
| 转发 | gRPC 双向流，96 分片 × 4 条独立 TCP 连接（`connGroupCount`），`StreamBatch` 合帧；logic 以 flip 模式主动拨入网关 |
| 登录鉴权 | `LoginGate`（1000001）走捷径不进管道；可选 loginserver `ValidateLoginToken`（fail-closed）；周期 `BatchValidate` 安全网；`LogoutGate` + JWT Revoke |
| 消息管道 | 过载检查 → 认证 → 安全链（限流/WAF/JWT/熔断）→ 过滤器链 → 连接级流控；同步（默认）/ 异步可选 |
| 踢人维护 | loginserver 单向 `KickService`（sgate 无封禁存储）；维护模式阻断新登录并踢出在线玩家 |
| 可靠推送 | `PushReliable` 端到端 ACK + 超时重传 + 窗口背压（logic SDK 实现，**网关零状态**）；`PushToConnection` 尽力而为照旧 |
| 下行优化 | 分片写合并（ShardedWriteCoalescer）、`batchPush` 合帧 `PushBatch`、分片并发广播 |
| 跨网关 | 自描述 session ID `conn_{serverID}_{UnixNano}_{counter}`，零登记表定位所在网关 |
| 集群 | etcd 注册/服务发现；多实例**对等部署**（无网关间协同、无 Leader 选举）；跨实例一致性由 loginserver 权威库 + Kick 广播覆盖 |
| 运维可观测 | `/stats`、`/health`、pprof、tlog 结构化日志、热配置更新、流量镜像、灰度、告警 Webhook、连接时长 P50/P95/P99 |
| 连接防护 | `maxConnections` / `maxConnectionsPerIP`、帧大小上限（启动交叉校验）、WS 心跳、空闲超时、CPU 过载丢弃 |

当前能力全集与待办见 [`docs/roadmap.md`](docs/roadmap.md)。

---

## 目录

- [一、整体架构与设计思路](#一整体架构与设计思路)
- [二、环境要求](#二环境要求)
- [三、快速开始（3 分钟跑通）](#三快速开始3-分钟跑通)
- [四、完整启动流程](#四完整启动流程)
- [五、运行模式](#五运行模式)
- [六、配置详解](#六配置详解)
- [七、压测指南](#七压测指南)
- [八、性能数据](#八性能数据)
- [九、协议说明](#九协议说明)
- [十、逻辑层 API](#十逻辑层-api)
- [十一、目录结构](#十一目录结构)
- [十二、开发指南](#十二开发指南)
- [十三、常见问题](#十三常见问题)

---

## 一、整体架构与设计思路

### 1.1 sgate 在系统中的位置

把 sgate 想象成**邮局分拣中心**：玩家（客户端）把信投进邮局柜台，分拣员验身份、安检、按收件部门分拣后装车送往对应处理中心（逻辑服）；逻辑服要回信时，邮局查账本找到收件人地址，装进信封送回玩家手里。邮局自己不写信、不处理业务，只保证**信能又快又准地到**。

```text
        玩家们                                游戏逻辑服们
   ┌──────────┐                          ┌──────────────────┐
   │ TCP/WS 连接│                          │ logic（可多实例）  │
   └────┬─────┘                          └────▲─────────┘
        │ ①收信·验身份·安检·分拣                    │ ③ gRPC 流（96 分片×4 连接）
┌───────▼──────────────────────────┐    ┌─────────┴─────────┐
│  gateway  接入层（邮局柜台）         │    │  backend  后端层    │
│  登录/踢人/管道/过载/监控/UDP 关联   │───▶│  LogicClientPool   │
└───────┬──────────────────────────┘    │  StreamManager     │
        │ 依赖（严格单向）                  │  GRPCServer        │
┌───────▼──────────────────────────┐    └───────────────────┘
│  connection  连接层（账本）          │
│  ConnectionManager·分片 Map·写合并   │◀── 两层通过消费方接口反向调用，不产生反向依赖
└──────────────────────────────────┘
        共享协议帧：internal/routes（gateway 和 backend 都用，防循环依赖）
```

### 1.2 分层结构

| 层 | 包 | 职责 |
| --- | --- | --- |
| 组装 | `cmd/gateway/main.go` | **唯一组装点**：容器 + 过滤器链 + 版本 |
| 接入层 | `internal/gateway` | gnet 事件、登录/登出、消息管道、踢人、维护、过载、监控、UDP 关联 |
| 后端层 | `internal/backend` | LogicClient、LogicClientPool、StreamManager（分片/合帧）、GRPCServer（flip 流入口） |
| 连接层 | `internal/connection` | Connection、ConnectionManager（分片存储）、Group、写合并器、session ID 生成/解析 |
| 共享 | `internal/routes` | 命令码、MessageFrame 编解码——被 gateway 和 backend **共同依赖**，避免循环引用 |

### 1.3 依赖方向与"消费方定义接口"

包依赖是一条**严格单向的直线**：`gateway → backend → connection`。禁止 `connection` 引用上层、禁止 `backend` 引用 `gateway`。

但上层经常需要回调下层能力（backend 要查连接、connection 要转发消息），怎么在不反向 import 的前提下做到？答案是**由消费方定义接口、生产方实现**：

| 接口 | 定义在（消费方） | 实现方 | 用途 |
| --- | --- | --- | --- |
| `backend.GatewayInterface` | backend | `*gateway.Gateway` | 查连接、取配置、推送计数 |
| `connection.LogicClientProvider` | connection | `*backend.LogicClient` | pipeline 转发消息 |
| `connection.UDPDownlink` | connection | `*gateway.Gateway` | UDP 下行通道注入 |

编译期断言（`var _ backend.GatewayInterface = (*Gateway)(nil)`）保证实现不漂移；测试时可 mock。**好处**：依赖永远是一条直线，永远不会出现"A 引 B、B 引 A"的纠缠。

### 1.4 装配与生命周期

所有组件在 `cmd/gateway/main.go` 平铺创建，按 Order 排序启动：

```text
uperf（GOGC/GOMEMLIMIT 软上限，最先）
  → Security(100) 安全链就绪
  → Obs(200)      日志/追踪/指标
  → Traffic(300)  流量治理
  → Cluster(400)  etcd 自注册、配置中心、告警
  → Gateway(1000) 接入层最后启动
```

三条配套约定：

1. **构造函数零参数**：所有组件 `NewXxx()` 不接收参数，内部通过 `config.Get()` 读配置——组件不携带环境信息，天然可测试。
2. **全局资源模式**：组件在 Init 阶段把能力写入 `internal/component/resources.go`（如 `setSecurityResources(...)`），Gateway 只能通过导出 getter 读取（如 `component.RateLimiter().Allow(...)`）——写入点收在 component 包内部，读取点全部显式。
3. **全局过滤器链**：main 里 `types.InitFilterChain()` 初始化一次，组件用 `types.GetFilterChain().AddFilter(...)` 注册，配置 `filterChain.filters` 按名加载 SPI 过滤器。

### 1.5 上行 / 下行数据流

**上行（玩家说话）——一关关过，每关都有名有姓：**

```text
1. gnet OnTraffic 收到字节
2. 切帧：TCP 按 4 字节长度前缀 / WS 按帧解析
3. 解码 MessageFrame（routes.ExtractMessageFrame）
4. 消息管道（pipeline）：
   ├─ 过载检查（CPU 超阈值 + dropOnOverload → 丢弃新消息）
   ├─ 认证/会话（未登录只放行 preAuthCommands=[1000001]）
   ├─ 安全链（限流令牌桶 / WAF / JWT / 熔断）
   ├─ 过滤器链（业务 SPI）
   └─ 连接级流控（每连接每秒消息数，protection.maxMessagesPerConn）
5. LogicClientProvider.SendMessage（connection 接口）
6. 按 sessionID 哈希选分片入队（同连接消息严格有序）
7. gRPC stream.Send → 逻辑服
```

**下行（逻辑服推消息）——查账本、编码、合并写：**

```text
1. GRPCServer.OnData 收到 StreamData
2. 查账本：connectionManager.Get(sessionID)（分片 Map，热读几乎无锁）
3. 编码：routes.MarshalClientMessage / EncodeWSFrame
4. 可选合并：ShardedWriteCoalescer 短窗口合并为一次系统调用；
   batchPush=true 时多条打包成 PushBatch（9000002）
5. gnet 异步写回客户端（写失败计入 /stats 的 asyncWriteFailed）
```

**每一步都可能丢**（gRPC 流断、无连接、异步写失败），但每一步的"成功"都不等于客户端收到——这就是 1.7 三个关键设计要解决的问题。

### 1.6 登录流程

```text
Client → CmdLoginGate(1000001) LoginGateReq
  → 过载检查 → IP 限流 → 并发登录槽（满则 429）
  → worker 协程 finishLoginGate（不阻塞 gnet 事件循环）
      ├─ loginValidation.enabled=true → loginserver ValidateLoginToken
      │    （失败 401 fail-closed；地址经 etcd 发现，round_robin 热切换）
      ├─ 绑定 userUUID = {serverId}:{userId}、踢旧连接
      ├─ Client ← CmdLoginGateAck(1000002)（code=0 成功，UDP 开启时附带 udpToken）
      └─ 原始 LoginGate StreamData 异步转发 logic（logic 直接读 LoginGateReq）
```

- **LoginGate 走捷径不经 pipeline**（登录本身不能被"未认证"规则拦住），其余命令必须过完整管道
- `loginValidation.enabled` 是唯一开关：**不以"是否传了 loginKey"决定是否校验**（否则不传 key 即可绕过）；压测恒为 `false`
- 登出 `CmdLogoutGate(1000003)`：撤销 JWT → 通知逻辑服 → `SendThenClose` 可靠投递 ack 后断连

### 1.7 三个关键设计（为什么这样设计）

这三处是全项目最重要的设计决策，共同点是：**新复杂度被收敛到单点或推给正确的归属，网关自己保持纯管道**。

#### 1.7.1 可靠推送：端到端 ACK（`PushReliable`）

**问题**：下行推送原本是"本地入队即算成功"——gRPC 流断、网关异步写失败、客户端收到没执行，业务全都无感知。

**原理**：投递好比寄挂号信，每段传送带（TCP、gRPC）只保证"把包裹交到下一段手里"，任何一段说"我送出去了"都不等于收件人签收。逐跳各建一层 ACK 修不掉任何真实缺口，只是每层各造一套窗口/定时器/重传状态。**唯一能证明送达的只有收件人回签收单**，所以 ACK 建在端到端（logic ↔ 客户端）：

```text
logic  PushReliable(sessionID, cmd, data)
  → 按会话单调分配 SeqId 写入 StreamData.SeqId   （复用既有字段，wire 零改动）
  → 入该会话未确认窗口（默认 64，满则返回 ErrReliableWindowFull 背压，绝不静默丢）
  → 现有下行链路原样转发
客户端 → CmdMsgAck(1100013)（普通上行走现有 pipeline，可攒批）
  → logic 批量移出窗口
超时未收 ACK → janitor 扫描重传（指数退避 300ms~5s × 5 次）→ 达上限放弃 + giveUps 告警
会话 Offline / 断流 → 清窗（droppedOffline）→ 会话内 at-least-once，跨会话靠重连拉取
```

**网关零改动、零新状态**——ACK 是普通上行，gRPC 断、网关崩、写失败、客户端没执行，最终都表现为"没收到 ACK"，被同一机制兜住。按需 opt-in：只有丢不起的消息（结算/掉落/交易）走 `PushReliable`，其余照旧 `PushToConnection`（全量 ACK 等于在应用层重造 TCP）。详见 [§10.1](#101-可靠推送端到端-ack)。

#### 1.7.2 WS + UDP 双通道（`udp.enabled` 默认关闭）

**一个会话、两个通道**：WS 是会话与指令面（登录/交易/结算，丢不起），UDP 是状态同步面（位置/AOI，高频可丢）。分工由**命令号段**静态分配，判据一句话："这条消息丢了，客户端能否靠下一帧自愈？"能 → UDP 段；不能 → WS 段。

工作方式：WS 登录后下发一次性 `udpToken` → 客户端自建 UDP socket 发 `ASSOCIATE`（NAT 映射只能由客户端先发包建立）→ 网关建 `connID ↔ 公网地址` 关联表 → 后续 UDP 包按源地址反查真实 connID，**复用现有 pipeline 全链路**；回程在 `connection.SendByCmd` **全系统唯一分流点**按号段路由，无关联/不新鲜/发送失败一律回落 WS（降级不丢）。存活判据只看 WS——UDP 丢包不断线，弱网玩家不被误踢。详见 [§9.1](#91-udp-关联通道udpenabled-开关默认关闭)。

#### 1.7.3 自描述 session ID（跨网关定位）

session ID 由网关生成，格式 **`conn_{serverID}_{UnixNano}_{counter}`**——把网关身份直接编进 ID，像"信封上直接写收件部门"：

- **定位**：`connection.ParseSessionGateway(id)` 解析出 `serverID` → 走**现有** etcd 服务发现取网关地址——零新增状态、零登记表（表会过期、要心跳续租、崩溃残留要清理；写在 ID 里随会话同生共死）
- **顺带根治跨网关重号**：`counter` 进程内自增，跨网关唯一性由 `serverID` 段保证
- **兼容**：`serverID` 为空（测试进程）退化为旧格式，解析返回空串不 panic

不建 etcd session 表的原因：消费方盘点后确认无真实需求（logic 推送靠 flip 内存表、loginserver 踢人靠 Redis 三键索引），且百万会话随连接频繁增删对 etcd 是写放大。详见 [§9.2](#92-自描述-session-id跨网关定位)。

### 1.8 高吞吐并发设计

| 机制 | 解决什么问题 |
| --- | --- |
| **gRPC 流分片 `shardCount=96`** | 单条流一把锁会串行；按 sessionID 哈希分片，同连接有序、不同连接并行 |
| **多 TCP 连接组 `connGroupCount=4`** | HTTP/2 同一 TCP 连接上的所有 stream **共享写锁**（96 个 shard 实际并行度=1）；建 4 条独立连接各承载 1/4 分片，真正并行写，实测吞吐 +76% |
| **`StreamBatch` 合帧** | 多条 StreamData 打包进单个 gRPC 帧，摊薄每条消息的系统调用与头开销（固定开启，无配置） |
| **分片写合并 ShardedWriteCoalescer** | 下行短时间内多条推送合并为一次系统调用；`batchPush=true` 再叠加 `PushBatch` 应用层合帧 |
| **分片连接表 ShardedMap** | 按 sessionID 查连接是热路径，单把锁会成瓶颈；分片后读几乎无锁 |
| **分片并发广播** | `BroadcastAll` 先快照再 32 worker 分片投递，百万连接广播不阻塞 gRPC handler |
| **同步 pipeline（默认）** | A/B 实测同步 498K/s > 异步 482K/s（异步另有队列丢弃），按 connectionID 哈希的异步 worker 仅作可选 |
| **uperf 软上限** | 启动最先应用 GOGC=300 / GOMEMLIMIT=80% 总内存，业务组件分配大内存前先定 GC 策略 |

### 1.9 设计原则小结

1. **一条直线依赖**：`gateway → backend → connection`，反向调用走消费方接口
2. **一条数据管道**：上行下行各一条，没有旁路；新功能全部通过收敛点进入（UDP→`SendByCmd`、可靠 ACK→网关零状态、session→两个函数）
3. **契约进代码、行为进配置**：命令号段、seq 语义是三端共享 wire 契约（`routes` 常量）；阈值/开关/时长进 yaml
4. **复杂度有明确归宿**：协议归 `routes`、可靠性状态归 logic SDK、连接账本归 `connection`、UDP 归一个分流点——每坨复杂度只允许住在一个房间
5. **默认安全**：token 空=接口关闭（fail-closed）、登录校验 fail-closed、非法配置整体拒绝保留当前值
6. **如实计数**：每种丢弃都有独立计数器（droppedTotal/pushDroppedNoConn/asyncWriteFailed/…），对账能精确闭合

详细包职责、数据流与扩展点见 [`docs/architecture.md`](docs/architecture.md)。

---

## 二、环境要求

| 依赖 | 版本 | 必需 | 说明 |
| --- | --- | --- | --- |
| Go | ≥ 1.26 | 是 | go.mod 指定 1.26.5，编译网关与压测程序 |
| etcd | ≥ 3.5 | 是 | 服务注册/发现、loginserver 发现——etcd 默认接入无关闭开关，必须先启动 |
| Windows / Linux / macOS | — | 是 | 生产建议 Linux（Windows 本地连接天花板约 16K，见 §八） |
| loginserver | — | 可选 | `loginValidation.enabled: true` 或需要 Kick 下发时必需；否则可不启动 |
| Redis / MySQL | — | 可选 | 仅 loginserver 侧使用（token 缓存、封禁 ACL），sgate 不直连 |

外部依赖模块（go.mod replace 指向本地仓）：

- `github.com/streasure/protocol` — 协议定义（MessageFrame、StreamData、PushBatch、命令码）
- `github.com/streasure/util` — 公共组件（tlog 日志、etcd、gRPC 插件、uperf、panicutil 等）

---

## 三、快速开始（3 分钟跑通）

前提：已安装 Go 和 etcd，且 etcd 在 `http://127.0.0.1:2379` 运行。

### 步骤 1：编译

```powershell
# 网关
go build -o bin\gateway.exe .\cmd\gateway

# 压测用逻辑服 + 客户端（可选，用于验证）
go build -o bin\logic1_tcp.exe .\bench\logic1_tcp
go build -o bin\bench1_tcp.exe .\bench\bench1_tcp
```

### 步骤 2：启动逻辑服（模拟）

```powershell
.\bin\logic1_tcp.exe -port 50052 -id logic1-tcp
```

逻辑服启动后在 etcd 注册 `default/SERVER_TYPE_LOGICSERVER:default`，并**主动拨入**网关（flip 模式，地址来自 etcd 发现的网关注册）。

### 步骤 3：启动网关

```powershell
# 推荐：启动脚本（自动编译，支持环境变量覆盖）
.\start.bat

# 或直接命令
.\bin\gateway.exe -conf config\gateway.yaml -logger config\log\default.yaml
```

启动成功的关键日志（业务日志全部走 tlog → `logs/sgate.log`，`config/log/default.yaml` 默认 `level: warn`、`console.enabled: false`，要看到 Info 级启动日志需调 `log.level: info` 和/或打开 console；gnet 自身日志直接打 stdout）：

```text
[gnet] Launching gnet with 12 event-loops, listening on: tcp://:48080
[gnet] Launching gnet with 12 event-loops, listening on: tcp://:48081
gateway starting http port:8081 version=... cpu=12 GOMAXPROCS=12
starting gateway transport addr=tcp://:48080 type=
starting gRPC server port=:50051
gRPC server started port=:50051
etcd 注册成功 serviceID=default/SERVER_TYPE_SGATE:default instanceID=gateway-1 address=...
```

此时网关已就绪：

- TCP 客户端端口：`48080`
- WebSocket 客户端端口：`48081`
- UDP 关联端口：`48082`（`udp.enabled: false` 时收到业务包明确拒绝）
- gRPC 端口：`50051`（逻辑服连接此端口）
- HTTP 端口：`8081`（`/stats`、`/health`、`/debug/pprof/`、`/admin/*`）

### 步骤 4：压测验证（可选）

```powershell
.\bin\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

看到 `avg rate: ... msg/s` 即链路打通。完整压测见 [§七](#七压测指南)。

---

## 四、完整启动流程

### 4.1 命令行参数

```text
sgate.exe [选项]

  -conf string     网关配置文件路径（默认 "config/gateway.yaml"）
  -logger string   日志配置文件路径（默认 "config/log/default.yaml"）
  -version         显示版本号后退出
```

示例：

```powershell
# 使用默认配置（等价 start.bat）
.\bin\gateway.exe -conf config\gateway.yaml -logger config\log\default.yaml

# 指定压测配置
.\bin\gateway.exe -conf config\bench\batch-on.yaml -logger config\log\bench.yaml

# 查看版本
.\bin\gateway.exe -version
```

启动脚本（自动编译，环境变量覆盖）：

```powershell
# Windows
.\start.bat
$env:CONF="config\cluster\gateway1.yaml"; $env:LOG_CONF="config\log\bench.yaml"; .\start.bat

# Linux
./start.sh
CONF=config/cluster/gateway1.yaml ./start.sh
```

环境变量：`CONF`（网关配置，默认 `config/gateway.yaml`）、`LOG_CONF`（日志配置，默认 `config/log/default.yaml`）。

### 4.1.1 配置文件选择

| 文件 | 用途 |
| --- | --- |
| `config/gateway.yaml` | 开发/单机默认（TCP 48080 / WS 48081 / UDP 48082 / gRPC 50051 / HTTP 8081） |
| `config/cluster/gateway1.yaml` / `gateway2.yaml` | 多实例对等部署（端口、serverId 错开） |
| `config/bench/batch-off.yaml` / `batch-on.yaml` | 压测 `batchPush` 关/开（`benchMode`，关限流与登录校验） |
| `config/bench/rate-limit.yaml` | 限流下扫压测 |
| `config/bench/1m.yaml` | 百万连接压测 |
| `config/log/default.yaml` | 常规日志（默认 `-logger`） |
| `config/log/bench.yaml` | 压测/排障日志（路径 `./logs/bench.log`，含采样） |

配置原则：**yaml 只写环境相关项**（身份、端口、etcd 地址、特性开关、环境差异覆盖）；「基本不改」的数值（`shardCount`、帧尺寸、限流默认、GOGC 等）在 `internal/config/defaults.go` + `ApplyRuntimeDefaults` 补齐，无需在 yaml 重复。详见 [§六](#六配置详解)。

### 4.2 启动顺序

**单体模式**（默认）：

```text
1. 启动 etcd（必需：网关注册、服务发现、loginserver 发现均依赖）
2. 启动逻辑服（etcd 注册 SERVER_TYPE_LOGICSERVER，并拨入网关）
3. 启动 sgate 网关（.\start.bat 或 sgate.exe -conf ...）
4. 客户端连接网关
```

> logic 与网关谁先谁后不敏感：flip 拨入带自动重连（日志 `gateway session ended, redialing`），任一侧重启都会自动恢复。但 etcd 必须先起（etcd 不可达网关启动失败）。

**集群模式**：

```text
1. 启动 etcd
2. 启动 loginserver（封禁权威库 + Kick 下发 + ValidateLoginToken）
3. 启动逻辑服
4. 启动 sgate 实例 1（CONF=config\cluster\gateway1.yaml .\start.bat）
5. 启动 sgate 实例 2（CONF=config\cluster\gateway2.yaml .\start.bat）
6. 客户端连接任一网关实例
```

### 4.3 启动日志说明

| 日志 | 含义 |
| --- | --- |
| `load config failed error=...` | 配置加载失败，**非零退出** |
| `gateway starting http port:... version=...` | 配置加载成功、进程启动（tlog） |
| `Launching gnet with N event-loops, listening on: ...` | gnet 事件循环启动（gnet 自身日志，直接打 stdout，每个监听端口一条） |
| `starting gRPC server port=:50051` → `gRPC server started port=:50051` | gRPC 端口就绪，可接受逻辑服拨入 |
| `etcd 注册成功 serviceID=... instanceID=... address=...` | `discovery.registerSelf: true`（默认配置如此）时网关已注册到 etcd |
| `etcd 自注册未启用（registerSelf=false），跳过 etcd 注册` | 跳过自注册（loginserver 将发现不到本网关） |
| `login service ready serviceID=...` | loginserver 发现 + 服务客户端已建立（`initLoginService`；etcd 不可达则启动失败） |

### 4.4 停止网关

`Ctrl+C` 或 `SIGTERM`，容器按 Order 逆序优雅关闭所有连接。批量停止（Windows）：

```powershell
Get-Process -Name gateway -ErrorAction SilentlyContinue | Stop-Process -Force
```

---

## 五、运行模式

sgate **无网关间协同**：多实例为对等部署，每个实例独立承接客户端、独立连逻辑服。

### 5.1 部署形态

实例之间**无直接协作**（无网关间发现、无 Leader 选举、无互踢 RPC）。多实例部署时每个实例：

- 向 etcd 自注册（`discovery.registerSelf: true`，供 loginserver 发现并下发 Kick / 对账会话）
- 逻辑服发现并拨入各实例
- 跨实例一致性由 **loginserver 权威库 + `KickService` 广播**覆盖

注册键 `/services/{belong}/SERVER_TYPE_SGATE:{zone}/{instanceId}`，值为 JSON（ip / grpc / tcp / websocket），loginserver watch 该前缀即可拿到全部网关地址。

**适用场景**：单实例，或多实例对等水平扩展（前置 LB 按端口或 IP 分流；注意 TCP 无 HTTP 头，反代取真实 IP 仅 WS 支持 `trustedProxies`）。

### 5.2 多实例

复制 `config/cluster/gateway1.yaml` / `gateway2.yaml`，改 `serverId` 与端口即可。要点：

- `serverId` 集群内**必须唯一**——它同时是 etcd instanceID 和 session ID 的身份段（[§9.2](#92-自描述-session-id跨网关定位)）
- `security.kickInternalToken` 与 loginserver `sgate.internalToken` 保持一致（空 = 拒绝全部 Kick RPC）
- `monitoring.pprofAddr` 各实例错开

---

## 六、配置详解

配置采用「代码默认值 + YAML 覆盖」语义：加载后 `ApplyRuntimeDefaults` 为零值字段补齐默认。**yaml 应只写环境相关项**；未出现的字段保持代码默认。显式填写的 `false`、`0` 或空字符串会覆盖默认值（bool 的 `true` 默认无法用零值表达，需在 yaml 显式写出，如 `security.enabled: true`）。数值默认清单见 `internal/config/defaults.go`。

### 6.1 顶层配置

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `httpPort` | int | 必填（`config/gateway.yaml` 为 `8081`） | stats/admin/健康检查 HTTP 端口。客户端端口由 `transports` 指定 |
| `belong` | string | 必填（`default`） | 所属应用/团队标识，与 serverType+zone+serverId 唯一确定一个服务 |
| `serverId` | string | 必填（`gateway-1`） | 实例 ID，集群中必须唯一；同时是 etcd instanceID 与 session ID 身份段 |
| `serverType` | string | 必填（`Gateway`） | 服务类型 |
| `zone` | string | 必填（`default`） | 可用区名称 |

### 6.2 `transports` 客户端监听

```yaml
transports:
  - port: 48080            # 空 type = TCP
  - port: 48081
    type: websocket
  - port: 48082
    type: udp              # 需 udp.enabled: true 才建立关联
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `port` | int | 监听端口，每个端口唯一 |
| `type` | string | 空 = TCP；`websocket` = WebSocket；`udp` = UDP 关联通道（见 [§9.1](#91-udp-关联通道udpenabled-开关默认关闭)）。`udp.enabled: true` 时必须存在 type=udp 监听，否则启动 Validate fail-fast |

### 6.3 `etcd` 服务注册

etcd 默认接入（无启用开关），启动网关前必须先启动 etcd。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `endpoints` | []string | `http://127.0.0.1:2379` | etcd 地址列表 |
| `servicePrefix` | string | `/services` | 服务注册键前缀 |
| `leaseTTL` | string | `10s` | 租约有效期（自动续期） |

### 6.4 `discovery` 服务发现

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `registerSelf` | bool | `false`（`config/gateway.yaml` 显式 `true`） | 是否向 etcd 注册网关自身连接信息（供 loginserver / logic 发现） |

> loginserver 发现无需配置：网关启动时按 `{belong}/SERVER_TYPE_LOGINSERVER:{zone}` 自动发现并保持服务客户端（`initLoginService`，etcd 不可达则启动失败）。逻辑服不走此发现——**logic 以 flip 模式主动拨入网关**，地址来自 etcd 发现的网关注册或 `gateways` 静态配置。

**网关注册地址格式**（`registerSelf: true` 时，JSON）：

```json
{
  "ip": "192.168.1.100",
  "grpc": 50051,
  "tcp": "192.168.1.100:48080",
  "websocket": "192.168.1.100:48081"
}
```

etcd key：`/services/{belong}/SERVER_TYPE_SGATE:{zone}/{instanceId}`，lease TTL 10s 自动续期。

### 6.5 `cluster` 实例身份

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `nodeID` | string | 自动生成（hostname-pid） | 节点 ID |

### 6.6 `grpc` gRPC 服务

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `port` | int | `50051` | gRPC 监听端口（逻辑服拨入此端口） |
| `windowSize` | int | `524288`（512KiB 零值回退；`config/gateway.yaml` 显式 `64MiB`） | HTTP/2 流控窗口 |
| `maxMessageSize` | int | `4MiB` | 单条 gRPC 消息最大字节数 |

### 6.7 `stream` 流分片和队列

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `shardCount` | int | `96` | gRPC 流分片数（零值补齐 96）；logic 侧拨入默认 `NumCPU×8` |
| `connGroupCount` | int | `4` | gateway→logic 独立 TCP 连接组数。每组承载 `shardCount/N` 个 stream、各自独立 HTTP/2 写锁，实现并行写（实测 +76%） |
| `sendChannelSize` | int | `65536` | 每分片发送队列容量，按 shardCount 预分配——调大慎行（`1048576×96` 约 768MB） |
| `queuePolicy` | string | `drop` | 队列满策略（丢最旧）；另有 block 模式超时 `sendTimeout=200ms` |
| `batchPush` | bool | `false` | 下行推送按连接合并为 `PushBatch`（9000002）；客户端需解析该命令 |

> gateway↔logic 数据流固定以 `StreamBatch` 合帧（多条 StreamData 打包进单个 gRPC 帧），无开关、两端无需配置。

### 6.8 `protection` 连接防护

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `maxFrameSize` | int | `4MiB` | TCP 单帧最大载荷 |
| `maxFrameBufSize` | int | `4MiB+4`（零值回退；百万连接建议显式 `64KiB` 并同步下调 maxFrameSize） | TCP 帧缓冲上限。**必须 ≥ maxFrameSize+4**（4B 长度前缀），否则大帧静默断连（启动 Validate 强制） |
| `maxWSFrameSize` | int | `4MiB` | WebSocket 单帧最大载荷 |
| `maxWSBufferSize` | int | `4MiB+14` | WS 帧缓冲上限。**必须 ≥ maxWSFrameSize+14**（最长 WS 头），否则大帧静默断连（启动 Validate 强制） |
| `maxConnections` | int | `0` | 网关最大总连接数，`0`=不限制。生产建议设置 |
| `maxConnectionsPerIP` | int | `0` | 单 IP 最大连接数，`0`=不限制 |
| `maxMessagesPerConn` | int | `0` | 连接级流控：每连接每秒消息数上限，`0`=不限制（pipeline 阶段 2.5 检查） |
| `cpuThreshold` | float | `90`（gateway.yaml 显式 `92`） | CPU 过载阈值（%），200ms 检查一次 |
| `dropOnOverload` | bool | `false`（gateway.yaml 显式 `true`） | 过载时丢弃新消息 |
| `wsHeartbeatTimeout` | int | `60` | WebSocket 心跳超时（秒），检查间隔 30s |
| `connIdleTimeout` | string | `5m`（gateway.yaml 显式 `30s`） | 连接空闲超时，检查间隔 5m |
| `preAuthCommands` | []int | `[1000001]` | 认证前（未绑定 serverId 前）允许通过 pipeline 的命令。`CmdLoginGate` 本身走捷径不经 pipeline |

**管理端接口**（挂载在 `httpPort`）：

| 端点 | 鉴权 | 说明 |
| --- | --- | --- |
| `GET /stats` | 无 | 全量指标 JSON（连接数、转发/推送/各类丢弃计数、msgRate、连接时长分位数、udpAssoc*、mirror* 等） |
| `GET /health` `/ready` `/live` | 无 | 健康检查 |
| `GET /debug/pprof/` | 无 | `monitoring.pprofAddr` 开启时可用 |
| `POST /admin/ratelimit` | Bearer | 限流阈值热更 |
| `POST /admin/maintenance` | Bearer | 维护模式：按 serverId/zone 置维护，阻断新登录并踢出在线玩家 |

`admin.token` 为空 = `/admin/*` 接口关闭（fail-closed）。封禁管理不在 sgate——已收敛到 loginserver ACL API（`/api/v1/acl/blacklist`、`/api/v1/acl/banned-role`），sgate 只执行 loginserver 单向下发的 Kick。

### 6.9 `security` 安全组件

**热切换限制**：`security/rateLimit/waf/jwtAuth/circuitBreaker` 的 `enabled` 开关**仅启动时生效**——组件与过滤器不随热更重建，配置中心改开关会被 `handleConfigUpdate` 整单拒绝（告警 `security enable flags cannot hot-toggle`），需重启。其余参数（限流阈值、黑白名单、JWT secret、过载阈值、连接限制等）支持热更；非法配置（如 `maxFrameBufSize < maxFrameSize+4`）同样整体拒绝、保留当前配置。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `false`（gateway.yaml 显式 `true`） | 安全链总开关 |
| `kickInternalToken` | string | `""` | KickService（loginserver→sgate）鉴权 token；空=拒绝全部 Kick RPC。与 loginserver `sgate.internalToken` 一致 |
| `trustedProxies` | []string | `[]`（不信任任何代理头） | **仅 WebSocket**：握手直连对端命中 IP/CIDR 时从 X-Real-IP / X-Forwarded-For 取真实客户端 IP。TCP 无 HTTP 头不支持 |
| `rateLimit.enabled` | bool | `false`（gateway.yaml 显式 `true`） | 令牌桶限流 |
| `rateLimit.maxTokens` | int | `10000` | 每秒令牌数（按 IP/路由）；下扫膝点 ~600K，默认覆盖 NAT 多客户端共享 IP。全局硬顶 = maxTokens×10，路由上限 = maxTokens×4。热更 `POST /admin/ratelimit` |
| `circuitBreaker.enabled` | bool | `false` | 熔断（连续失败 5 次开、半开成功 3 次恢复、30s 超时） |

**客户端登出**：`CmdLogoutGate(1000003)` → 撤销 JWT → 通知逻辑服 → `SendThenClose` 可靠投递 ack 后断连；应答 `CmdLogoutGateAck(1000004)`。

### 6.9.1 `loginValidation` 登录校验开关

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | `true`：LoginGate 必须经 loginserver `ValidateLoginToken`（**空 loginKey 也不能绕过**；loginserver 不可达或 gRPC 失败 → 401 fail-closed）。`false`：永远放行（压测/本地免登），周期安全网不跑 |
| `batchInterval` | string | `"60s"` | 周期 `BatchValidateLoginTokens` 安全网间隔：收集全部在线会话批量重校验，失效/被封禁的踢下线（收口 mid-login 竞态与会话 TTL 过期踢不中）。空/`0`=关闭 |
| `batchSize` | int | `200` | 单次批量校验条数（loginserver 硬上限 500，留余量），超过分片多次 RPC |

```yaml
loginValidation:
  enabled: true        # 生产开启
  batchInterval: "60s"
  batchSize: 200
# 压测配置必须为 false（见 AGENTS.md Benchmarks）
```

- **是否校验只看本开关**，不以「是否传了 loginKey」决定——否则不传 key 即可绕过
- 周期安全网校验时 sgate 留存 `accountId+loginKey`（仅用于校验，不转发不落盘）；校验期间 token 已换（重登）的跳过不踢

### 6.10 其他可选组件

| 配置段 | 关键字段 | 说明 |
| --- | --- | --- |
| `udp` | `enabled`, `associateTimeout`, `idleTimeout`, `maxPerIP`, `maxAssociations`, `seqWindow`, `maxDatagram` | WS+UDP 关联通道，默认全关（见 [§9.1](#91-udp-关联通道udpenabled-开关默认关闭)）；默认值 30s/60s/4/100000/1024/1400 |
| `waf` | `enabled` | Web 应用防火墙（payload 上限 1MB，命中动作 drop/log） |
| `tls` | `enabled`, `certFile`, `keyFile` | TLS 配置（gnet v2 当前不支持 WSS） |
| `jwtAuth` | `enabled`, `secret` | JWT 鉴权（header 字段 `X-Auth-Token`） |
| `canary` | `enabled`, `percent` | 灰度流量 |
| `trafficMirror` | `enabled`, `percent`, `targetAddr`, `queueSize`, `workers` | 流量镜像：按 percent 稳定采样上行，经 `GatewayStream.OnData` 真实转发到**独立测试环境 logic**（enabled=true 时 targetAddr 必填；队列满/目标故障只丢弃不阻塞，`/stats` 输出 mirrorForwarded/mirrorDropped/mirrorFailed；热更仅 percent） |
| `otelTracer` | `enabled`, `endpoint` | OpenTelemetry 链路追踪（采样率倒数默认 100） |
| `configCenter` | `enabled` | 配置中心 |
| `alert` | `enabled`, `webhooks` | 告警 Webhook（频控 30 条/分钟） |
| `degradation` | `enabled`, `rules` | 业务降级 |
| `monitoring` | `pprofAddr`, `disableMetricsLog`, `disableTracer` | pprof 地址（gateway.yaml 默认 `127.0.0.1:6060`）；`disableMetricsLog=true` 关闭每秒 metrics 日志；`disableTracer=true` 关闭每消息内部 Tracer（热路径零 span 分配，吞吐压测建议开启） |
| `perf` | `gcPercent`, `memoryLimitPercent` | GOGC（默认 300）、GOMEMLIMIT 占总内存百分比（默认 80），由 uperf 组件最先应用 |
| `wasmRuntime` | `enabled`, `modules` | WASM 运行时：启用后加载模块供 filterChain 的 wasm-filter 引用；加载失败 fail-fast 终止启动 |
| `filterChain` | `enabled`, `filters` | 过滤器链：按名加载 SPI 过滤器 |

### 6.11 `pipeline` 异步消息管道

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `asyncEnabled` | bool | `false` | 异步 pipeline（worker pool）。A/B 实测同步 498K/s > 异步 482K/s（异步另有队列丢弃），默认同步 |
| `workerShards` | int | `0`（= CPU×4） | worker 分片数；按 connectionID 哈希分片保证同连接严格有序 |
| `workerQueueSize` | int | `65536`（gateway.yaml 显式 `262144`） | 任务队列**总大小**（全池共享，启动按分片均分）。仅 asyncEnabled=true 时生效 |

---

## 七、压测指南

### 7.1 压测原则（重要）

1. **单协议分开测**：每轮压测只跑一种协议（一轮纯 TCP、一轮纯 WS），不混测——协议差异才有干净的对照结论
2. **登录校验全关**：压测配置必须 `loginValidation.enabled: false`，只走 LoginGate（cmd 1000001），不调 login HTTP API、不做 token 校验；成功的标志是全程**无 401 / authfail / login-key 拒绝**
3. **TCP 轮次间必须等 TIME_WAIT**：回环场景下客户端主动关闭产生的 TIME_WAIT 四元组全部指向 `127.0.0.1:48080`（同目标不可复用），Windows 残留 240s——**两轮 TCP 测试之间间隔 >240s**，否则端口池耗尽，第 N 个连接后全部 `connectex: Only one usage of each socket address`（实测首轮未等，1091 个之后 13,909 连接全灭）
4. **bench 场景各自独立 main**：`bench/*` 每场景一个独立程序，不合并成多命令二进制（保证隔离与公平采样）
5. 每轮持续时间、并发数、载荷、协程数保持一致；以 bench 客户端 `avg rate` / `avgReceiveRate` 为主指标，绝对值受本机后台负载影响，同批内部对比为准

### 7.2 工具矩阵

| 方向 | 工具 | 配套 logic | 说明 |
| --- | --- | --- | --- |
| 上行 client→sgate→logic | `bench1_tcp` / `bench1_ws` | `logic1_tcp` / `logic1_ws` | 转发能力；进度打 stdout（fmt.Printf 面向使用者） |
| 下行 logic→sgate→client | `bench2_tcp` / `bench2_ws` | `logic2_tcp` / `logic2_ws` | 推送能力；进度走 tlog → `logs/bench.log` |
| 连接极限 | `bench_1m` | 任意 | 分批建连（`-batch` 200 防 Windows 半开保护）、登录、保持 `-stay` |
| 可靠推送 | `client_ack` | `logic_reliable` | ACK/去重窗口/故障注入（`-drop-ack`/`-no-ack`/`-ack-interval`） |
| 登录校验 | — | `logic_logincheck` | loginserver 校验链路 |
| 连接抖动长稳 | `churn30` | — | 30 分钟 churn，见 `Bench.md` |

按协议独立成对：`bench1_tcp`↔`logic1_tcp`、`bench1_ws`↔`logic1_ws`、`bench2_tcp`↔`logic2_tcp`、`bench2_ws`↔`logic2_ws`。

### 7.3 编译

```powershell
go build -o bin\gateway.exe      .\cmd\gateway
go build -o bin\bench1_tcp.exe   .\bench\bench1_tcp
go build -o bin\bench1_ws.exe    .\bench\bench1_ws
go build -o bin\bench2_tcp.exe   .\bench\bench2_tcp
go build -o bin\bench2_ws.exe    .\bench\bench2_ws
go build -o bin\logic1_tcp.exe   .\bench\logic1_tcp
go build -o bin\logic1_ws.exe    .\bench\logic1_ws
go build -o bin\logic2_tcp.exe   .\bench\logic2_tcp
go build -o bin\logic2_ws.exe    .\bench\logic2_ws
go build -o bin\bench_1m.exe     .\bench\bench_1m
```

### 7.4 场景一：上行转发（bench1）

每轮开三个进程（gateway + logic1 + bench1），协议二选一（**单协议**）：

```powershell
# 网关（压测配置：benchMode 关限流/登录校验）
.\bin\gateway.exe -conf config\bench\batch-on.yaml -logger config\log\bench.yaml

# 逻辑服（只收不处理）
.\bin\logic1_ws.exe -port 50053 -id logic1-ws          # WS 轮
.\bin\logic1_tcp.exe -port 50052 -id logic1-tcp        # TCP 轮

# 客户端
.\bin\bench1_ws.exe  -addr 127.0.0.1:48081 -duration 10s -parallel 100
.\bin\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

### 7.5 场景二：下行推送（bench2）

需分别测 `batchPush=false`（`config/bench/batch-off.yaml`）与 `true`（`config/bench/batch-on.yaml`）两种模式：

```powershell
# 网关
.\bin\gateway.exe -conf config\bench\batch-on.yaml -logger config\log\bench.yaml

# 逻辑服（-expected-members 0 = 登录即开推；非 0 会等满员再 sleep 10s）
.\bin\logic2_ws.exe -port 50061 -id logic2-ws -push-size 64 -expected-members 0 -push-workers 12

# 客户端
.\bin\bench2_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100 -server-id logic2-ws
```

TCP 轮同理换 `-addr 127.0.0.1:48080` 与 `logic2_tcp` / `bench2_tcp`；**TCP 轮与上一轮间隔 >240s**（§7.1 第 3 条）。

### 7.6 场景三：连接极限（bench_1m）

```powershell
.\bin\gateway.exe -conf config\bench\1m.yaml -logger config\log\bench.yaml
.\bin\bench_1m.exe -addr 127.0.0.1 -ports 48080 -connections 16384 -batch 200 -stay 60s
```

Windows 临时端口池（49152 起共 16384 个）是本机硬天花板，更大规模需 Linux（epoll）。

### 7.7 常用参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:48080` | 网关客户端地址（WS 用 48081） |
| `-duration` | `10s`（bench2 `30s`） | 压测持续时间 |
| `-parallel` | `100` | 并发连接数 |
| `-login-batch` | `100` | 批量并发登录批大小（bench1_tcp 为串行登录，15K 约 4s） |
| `-server-id` | — | 登录绑定的 logic 实例 ID（bench2 用） |
| `-push-size` | `64` | 推送载荷字节数 |
| `-expected-members` | `0` | 等组员达到该数再推；**压测必须 0**（非 0 会满员后再等 10s） |
| `-push-workers` | `128` | logic 推送工作协程数 |
| `-shardCount` | `0` | logic→网关流数，`0`=`NumCPU×8` |
| `-batch` / `-stay` | `200` / `30s` | bench_1m 每批建连数 / 保持时长 |
| `-pprof` | 空 | logic 侧 pprof 监听地址（如 `127.0.0.1:6063`），空=关闭 |

### 7.8 停止所有压测进程

```powershell
Get-Process -Name gateway,logic1_tcp,logic1_ws,logic2_tcp,logic2_ws,bench1_tcp,bench1_ws,bench2_tcp,bench2_ws,bench_1m -ErrorAction SilentlyContinue | Stop-Process -Force
```

每轮测试前确认 etcd 之外无残留 gateway/logic/bench 进程（etcd 可常驻复用）。

---

## 八、性能数据

完整测量条件、逐轮对账与瓶颈分析见 [`docs/benchmark-report.md`](docs/benchmark-report.md)，限流下扫与 churn 长稳见 [`Bench.md`](Bench.md)。以下为当前实测摘要。

**测量环境**：Windows 10/11 · i5-10400F 6C/12T · 32GB · 1Gbps · go1.26 · gnet v2.9.7。`loginValidation.enabled: false`、仅 LoginGate、`shardCount=96`、`connGroupCount=4`、`disableTracer/disableMetricsLog: true`。

### 8.1 100 连接基线（10s，64B 载荷）

| 方向 | 协议 | 平均速率 | 失败 |
| --- | --- | ---: | ---: |
| 上行 bench1 | TCP | **456,501 msg/s** | 0 |
| 上行 bench1 | WebSocket | **647,095 msg/s** | 0 |
| 下行 bench2（batch on） | TCP | **1,277,758 msg/s** | 0 |
| 下行 bench2（batch on） | WebSocket | **1,174,324 msg/s** | 0 |
| 下行 bench2（batch off） | TCP | 428,333 ~ 485,659 msg/s | 0 |
| 下行 bench2（batch off） | WebSocket | 381,512 msg/s | 0 |

`batchPush=false` 为非合帧逐条写出路径，低于合帧路径属预期，主路径以 batch-on 为准。

### 8.2 16K 极限压测（15000~16384 连接，场景 A~F）

| 场景 | 协议/方向 | 关键结果 |
| --- | --- | --- |
| **A 连接极限** | TCP | 16,071~16,095 / 16,384（失败全为 Windows 端口池耗尽，非网关拒绝）；建连+登录 **6,583~6,605 conn/s**；60s 保持**零掉线**；RSS 1,341→1,342MB（**~75KB/连接**）；保持期 CPU ≈0.3% |
| **B 上行** | WS ×15K | **724,469 ~ 759,359 msg/s**；客户端 sent 与网关 forwarded 逐条对上；管道延迟 P50 0.5µs / P99 1.2µs；洪峰 CPU ≈3 核 |
| **C 下行** | WS ×15K | 三方精确闭合 `22,754,772 = pushedToClient 22,635,259 + pushDroppedNoConn 113,029 + Send失败 6,484`；logic 稳态 **≈630K msg/s**；RSS 峰值 6,582MB（alloc 809MB）回落 467MB；业务零丢弃 |
| **D 对称双向** | WS 7500+7500 | 上行 370,145 msg/s（fwd=sent 分毫不差）+ 下行 ≈600K；重叠窗口总吞吐 **≈667K（最佳 788K）msg/s**；网关 CPU 仅 2.6 核；下行对账误差 0.02% |
| **E TCP 上行** | TCP ×15K | **565,639 msg/s**（窗口 628K）；sent 6,311,345 = forwarded 精确；CPU 2.14 核；RSS 1,953MB |
| **F TCP 下行** | TCP ×15K | logic 稳态 **≈505K msg/s**（网关分发 ≈476K）；三方对账误差 0.013%；CPU 3.68 核；15000/15000 零失败 |

**TCP vs WS 速率矩阵（15000 连接）**：上行 TCP 565K vs WS 724~759K（TCP 慢 ~26%）；下行 TCP ≈505K vs WS ≈607~627K（TCP 慢 ~17~20%）。注意反直觉现象：100 连接时 TCP 下行反而更快，15000 连接时 WS 反超——连接数放大后相对性能反转，可复现、归因待 pprof（报告如实记录）。

### 8.3 结论

- **业务零丢弃、对账分毫不差**：六轮全部 `droppedTotal=0`、`failed=0`，每种差值都能被独立计数器精确解释
- **网关 CPU 全程 ≤3.8/12 核，从未饱和**——本机测试瓶颈在压测工具两端（bench 发送节奏、logic 推送循环、客户端读取速度），sgate 自身远未到极限；纯上限需多机部署（网关独占 CPU + 真实网卡）钉死
- **单核 ≈25 万条/s**；1Gbps 网卡典型消息（~100B）理论上限 ≈62~125 万条/s
- **Windows 16K 是端口池天花板**，百万连接与 epoll 差异需 Linux 环境（roadmap P2-4）

---

## 九、协议说明

协议定义位于 `github.com/streasure/protocol`（go.mod replace 指向本地协议仓）。

| 通信方向 | 消息 | 编码方式 |
| --- | --- | --- |
| 客户端 ↔ sgate | `MessageFrame` | TCP：4 字节大端长度前缀；WS：二进制帧；UDP：4 字节大端长度前缀（单数据报 = 单帧） |
| sgate ↔ 逻辑服 | `StreamData`（打包为 `StreamBatch` 合帧） | gRPC 双向流 protobuf |
| sgate → 客户端批量推送 | `MessageFrame(CmdPushBatch)` | Body 嵌套 `PushBatch` |

主要命令号：

| 命令 | 数值 | 说明 |
| --- | ---: | --- |
| `CmdLoginGate` | `1000001` | 客户端登录网关（走捷径不经 pipeline） |
| `CmdLoginGateAck` | `1000002` | 登录应答（UDP 开启时附带 udpToken） |
| `CmdLogoutGate` | `1000003` | 登出请求（撤销 JWT、通知逻辑服、可靠投递 ack 后断连） |
| `CmdLogoutGateAck` | `1000004` | 登出应答 |
| `CmdKickNotify` | `1100009` | 踢下线通知（封禁/IP/维护/会话失效共用） |
| `CmdHeartbeatReq/Ack` | `1100010` / `1100011` | 心跳 |
| `CmdUserOffline` | `1100012` | 用户下线通知 |
| `CmdMsgAck` | `1100013` | 可靠推送端到端 ACK（C→L，body 为 `MsgAck{repeated int64 seq_ids}`，批量确认） |
| `CmdPushBatch` | `9000002` | 网关批量推送 |

**命令号段分配约定**：`1200000~1299999` 为 UDP 通道段（`routes.CmdUDPRangeStart/End`，含段内传输控制命令 `1200000~1200005`），该段消息按号段分流到 UDP 关联通道；**其余号段（含 ≥1300000）一律 WS**。判据："这条消息丢了，客户端能否靠下一帧自愈？"能 → UDP 段（位置/AOI/血条等状态同步）；不能（结算/掉落/交易）→ 留在 WS 段。号段是三端（sgate/logic/客户端）共享的 wire 契约，分配走代码评审与版本发布，**不进 yaml 配置**。

### 9.1 UDP 关联通道（`udp.enabled` 开关，默认关闭）

一个会话、两个通道：WS 是会话与指令面（丢不起的消息），UDP 是状态同步面（高频可丢状态），**关联生命周期挂在 WS 会话上**。

工作方式：

```text
1. WS/TCP 建连 → LoginGate 登录成功 → LoginGateAck 下发一次性 udpToken
2. 客户端自建 UDP socket → 向网关固定 UDP 端口发 ASSOCIATE{udpToken, nonce}
   （NAT 映射只能由客户端先发包建立：服务器无法主动开 UDP）
3. 网关验 token（一次性消费防重放）→ 建关联表 connID ↔ (公网IP:port, lastSeen, seq 窗口)
   → 回 ASSOCIATE_OK → 注入下行通道
4. 上行：UDP 包按源地址反查真实 connID → seq 滑动窗口去重 → 投现有 worker pool，
   复用 pipeline 全链路（鉴权/限流/过滤器/转发不变，帧体与 TCP/WS 逐字节一致）
5. 下行：connection.SendByCmd 是全系统唯一分流点——
   号段内 && 有关联 && 新鲜 → sendUDPFrame，否则回落 WS（降级不丢）
6. WS 断开 / UDP 空闲超时（janitor，默认 60s）→ 删除关联；NAT 保活靠客户端 15s 周期发包
```

要点：

- **默认关闭**：`udp.enabled: false` 时无关联、所有下行回落 WS，零行为变化（feature flag，关开关即回滚）；关闭态 UDP 端口对业务消息明确拒绝（`droppedUnsupported` 计入 droppedTotal），不建连接、不投 pipeline
- **降级不丢**：无关联 / 关联不新鲜（超过 `idleTimeout/2` 未收包）/ 发送失败 → 自动回落 WS
- **存活判据只看 WS**：UDP 丢包不断线，弱网玩家不被误踢；`/stats` 暴露 `udpAssoc*`、`udpSweptWsSurviving` 等计数
- **安全**：token 一次性防重放、反放大（回包 ≤ 来包，垃圾包零回包）、per-IP（默认 4）/总量（默认 100000）双上限、seq 去重窗口（1024）、MTU ≤ 1400B
- 号段常量 `routes.IsUDPCmd`（纯函数，热路径一次区间比较）

完整设计与决策记录见 [architecture §4.4](docs/architecture.md#44-wsudp-关联通道已支持) 与 [docs/udp-ws-design.md](docs/udp-ws-design.md)。**阶段 4 压测数据待 Linux 环境**（roadmap P2-3）。

### 9.2 自描述 session ID（跨网关定位）

session ID 由网关生成，格式 **`conn_{serverID}_{UnixNano}_{counter}`**——网关身份（config `serverId`，与 etcd 注册的 instanceID 同一标识）直接编在 ID 里：

- **定位**：`connection.ParseSessionGateway(sessionID)` 解析出 `serverID`，再走现有 etcd 服务发现（`{belong}/SERVER_TYPE_SGATE:{zone}/{serverId}`）取网关地址——**零新增状态、零外部登记表**（路由信息写在"信封"上，随会话同生共死：不过期、不需心跳续租、不需清理）
- **解析规则**：从右取两段纯数字（`counter`、`UnixNano`），其余部分即 `serverID`——因此 `serverID` 含下划线或以数字结尾也能正确还原
- **全局唯一**：`counter` 是进程内自增，跨网关唯一性由 `serverID` 段保证（没有该段时，两台网关在相同时钟读数下会发重号，logic 的 sessions 表会把消息送到错误网关）
- **兼容**：`serverID` 为空（测试/工具进程）时退化为旧格式 `conn_{UnixNano}_{counter}`，`ParseSessionGateway` 返回空串不 panic

消费方：logic 推送靠 flip 模式内存表、loginserver 踢人靠 Redis 三键索引，均已覆盖单 zone 场景；本约定服务跨 zone / 跨服战（P3-2、P3-4）的定位需求。详见 [architecture §4.6](docs/architecture.md#46-自描述-session-id跨网关定位)。

---

## 十、逻辑层 API

逻辑服通过 `logic.Server` 提供的 API 操作客户端连接：

```go
PushToConnection(sessionID, command, data)  // 向指定会话推送（尽力而为，下一帧可自愈的消息）
PushReliable(sessionID, command, data)      // 可靠推送（丢不起的消息：ACK + 超时重传）
SendToGroup(groupID, command, data)         // 向组内成员推送
Broadcast(command, data)                    // 向所有网关广播
```

批量推送是 sgate 的传输优化，逻辑服不需要感知 `PushBatch`。

### 10.1 可靠推送（端到端 ACK）

`PushToConnection` 是"本地入队即算成功"——gRPC 流断、网关异步写失败、客户端没执行，业务都无感知。丢不起的消息（结算/掉落/交易）改用 `PushReliable`：

- **端到端 ACK，网关零改动**：logic 按会话单调分配 `SeqId` 写入 `StreamData.SeqId`（wire 零改动），消息进该会话**未确认窗口**（默认 64 条）；客户端处理后回 `CmdMsgAck(1100013)`（body 为重复 seq 列表，可攒批），logic 批量移出窗口。ACK 是普通上行走现有 pipeline，网关保持纯管道、零新状态——任何一段丢包最终都表现为"没收到 ACK"，被同一机制兜住
- **超时重传**：janitor 扫描式（非每消息一个 timer），指数退避 300ms 起、上限 5s、默认最多 5 次；达上限放弃并计 `giveUps` 告警
- **窗口满背压**：返回 `ErrReliableWindowFull`，由业务决定等待/降级，**绝不静默丢**
- **断线清窗**：会话 Offline/断流即清空窗口——本机制只保证**会话内 at-least-once**，不跨会话续传；断线补齐靠重连后的登录全量/增量同步（业务现有流程）。重复投递由客户端滑动窗口去重（窗口 1024，同 UDP seq 位图模式）
- **顺序语义**：ACK 层只保证"到达 + 去重"，不保证相对顺序（补发可能晚于新消息），顺序由业务消息自带版本号兜底
- **配置** `reliability.*`（`windowSize` / `scanInterval` / `initialBackoff` / `maxBackoff` / `maxRetries`，零值回退默认）；**观测** logic `PushMetrics` 的 `acked` / `retransmits` / `giveUps` / `droppedOffline`，网关 `/stats` 的 `asyncWriteFailed`

按需 opt-in：只有丢不起的消息走 `PushReliable`，其余照旧 `PushToConnection`（全量 ACK 等于在应用层重造 TCP）。

**验收实测**（50 连接四场景，详见 roadmap P2-1）：基线 `pushed = acked + droppedOffline` 严格等式；`-drop-ack 10%` 时重传全部补齐、`giveUps=0`；完全不 ACK 时窗口背压不 panic；杀网关断流 `droppedOffline` 如实清窗、重启后新推送全部收齐且去重不重复执行。

**`internal/logic` 的定位**：逻辑服侧的嵌入库，网关侧不引用它（`internal/gateway`、`internal/backend` 均不 import）。它封装 gRPC 服务端（注册 `GatewayStream` 服务）、etcd 注册（键 `{belong}/SERVER_TYPE_LOGICSERVER:{zone}`）和 `Dialer` flip 拨入（logic 主动连网关，地址来自 etcd 发现或 `gateways` 静态配置），对外提供 `logic.NewService(...)` / `svc.Server()`。本仓中由 bench 模拟逻辑服（`bench/logic1_*`、`bench/logic2_*`、`bench/logic_reliable` 等）消费，真实逻辑服进程同样以此库嵌入；网关侧与之对接的是 `internal/backend` 的 flip 流入口 `GRPCServer`。

---

## 十一、目录结构

```text
cmd/gateway/              网关程序入口（唯一组装点：Container + FilterChain + Version）

internal/gateway/         接入层（package gateway）
  gateway.go                Gateway 结构体、生命周期、组件编排
  handlers.go               gnet 事件回调（TCP/WS 读写、关闭）
  login.go                  登录流程（LoginGateReq/Ack、initLoginService）
  admin.go                  登出、/admin/maintenance、/admin/ratelimit（Bearer）
  kick.go                   踢人执行器（loginserver 单向 Kick 的本地执行端）
  maintenance.go            维护模式（阻断新登录 + 踢出在线玩家）
  batch_validate.go         BatchValidate 周期安全网
  websocket.go              WebSocket 握手、帧解析
  pipeline.go               认证、安全、过滤、转发管道
  filter.go                 过滤器链 SPI
  monitor.go                /stats、/health、配置热更新
  overload.go               过载保护
  integrity.go              消息完整性（时间戳/去重，按连接分 16 片）
  udp_*.go                  UDP 关联（assoc/uplink/send/reject）
  version.go                版本信息

internal/backend/         后端层（package backend）
  client.go                 LogicClient（状态机、断线缓存、SendMessage）
  pool.go                   LogicClientPool（logic 接入、负载均衡）
  stream.go                 StreamManager、StreamShard（分片、StreamBatch 合帧）
  state.go                  连接状态、错误定义
  grpc_server.go            GRPCServer（OnData flip 流、Gateway/Kick unary RPC）
  iface.go                  GatewayInterface（消费方定义接口）

internal/connection/      连接层（package connection）
  connection.go             客户端连接对象
  manager.go                ConnectionManager（分片存储、账号→连接索引、session ID 生成/解析）
  group.go                  连接分组（广播/组播）
  sharded_map.go            分片并发 Map
  coalescer.go              分片写合并器（批量推送优化）
  ws_frame.go               WebSocket 帧编码
  iface.go                  LogicClientProvider、UDPDownlink 接口

internal/routes/          路由与协议帧（gateway 与 backend 共同依赖，防循环）
  routes.go                 路由常量、命令码、UDP 号段（IsUDPCmd）、帧解析
  frame.go                  MessageFrame 编解码、错误响应构造
  logicstream.go            flip 握手元数据

internal/component/       组件容器（Security/Obs/Traffic/Cluster + resources.go 全局资源）
internal/config/          配置结构、defaults.go 默认值、校验、热更新
internal/security/        白名单、限流、熔断、WAF、JWT
internal/traffic/         灰度、镜像、降级、WASM
internal/cluster/         etcd 自注册、配置中心、告警 webhook（无 Leader 选举）
internal/obs/             监控、追踪、pprof、延迟分位数
internal/types/           过滤器链、公共类型
internal/logic/           逻辑服侧嵌入库（网关不依赖，见 §十）

bench/                    每场景独立 main：bench1/2（tcp/ws）、bench_1m、
                          logic1/2（tcp/ws）、logic_reliable、client_ack、
                          logic_logincheck、churn30、logutil
config/                   gateway.yaml 单机默认 · cluster/ 集群 · bench/ 压测 · log/ 日志
docs/                     architecture / benchmark-report / roadmap / udp-ws-design
start.bat · start.sh      启动脚本（CONF / LOG_CONF 环境变量覆盖）
Bench.md                  限流下扫、churn 等历史压测明细
```

---

## 十二、开发指南

### 12.1 门禁（每次改动必须全绿）

```powershell
gofmt -l internal config cmd bench        # 期望无输出
go build -p 1 -ldflags="-s -w" ./...      # rc=0
go vet ./...                              # rc=0
go test -p 1 ./... -count=1               # rc=0
staticcheck ./...                         # rc=0
```

### 12.2 编码规范（AGENTS.md 摘要）

- **日志一律 `github.com/streasure/util/tlog`**（`tlog.Debug/Info/Warn/Error(ctx, ...)`），禁止 `fmt.Print*` / `log.*` 输出日志；`fmt` 仅用于非日志场景（CLI 版本输出、序列化、错误构造）
- **依赖方向严格单向**：`gateway → backend → connection`，共享帧工具只进 `routes`；反向能力一律走消费方接口
- **panic 处理用 `panicutil`**（`Go/GoLoop/Recover/Safe`），不手写 recover 模板；tag 约定 `package.component`
- **`component.NewContainer()` 只能在 `main.go` 调用**；组件构造零参数、经 `config.Get()` 读配置
- **bench 规则**：`loginValidation.enabled: false`、不调 login HTTP API、每场景独立 main、成功标志是无 auth 失败

### 12.3 修改协议

协议定义在外部模块 `github.com/streasure/protocol`：

1. 本地协议仓执行 `generate_proto.bat` 重新生成 Go 代码
2. 回本仓 `go mod tidy` 更新依赖
3. 重新编译（跨仓改动先行合入，再改 sgate 侧，同 UDP/MsgAck 的跨仓模式）

### 12.4 日志配置

`config/log/default.yaml`（`config/log/bench.yaml` 为压测变体，路径 `./logs/bench.log`）：

```yaml
log:
  level: warn          # debug / info / warn / error（默认 warn：Info 级启动日志不输出）
  format: json         # json / console
  console:
    enabled: false     # 控制台输出（网关默认无标准输出，全部落文件）
  file:
    enabled: true
    path: logs/sgate.log
    rotate:
      max_size: 100    # MB
      max_backups: 3
      max_age: 7       # 天
  async:
    enabled: true
    buffer_size: 100000
    batch_size: 1000
    flush_interval: 100
```

### 12.5 扩展点

| 扩展 | 方式 |
| --- | --- |
| 新过滤器 | 实现 `types.Filter`，配置 `filterChain.filters` 按名加载 |
| 新组件 | 实现 `Name/Order/Init/Start/Destroy`，main 里 `container.Add` |
| 新推送策略 | 替换/包装 `ShardedWriteCoalescer` 或调整 `batchPush` |
| 新路由 | `routes` 加命令码 + pipeline/handler 注册处理函数 |
| 新命令号段 | 三端 wire 契约，走代码评审与版本发布，不进 yaml |

---

## 十三、常见问题

### Q: 启动报 `connect: connection refused`

etcd 未启动。etcd 默认接入（无关闭开关），先启动 etcd 再启动网关。

### Q: 客户端连接后无响应 / 登录无 Ack

确认逻辑服已启动并在 etcd 注册（`SERVER_TYPE_LOGICSERVER`）。logic 拨入网关是 flip 模式，检查网关日志 `logic stream attached serviceID=...`。另确认命令号未落在错误号段（UDP 段消息在 `udp.enabled: false` 时会被拒绝）。

### Q: `loginValidation.enabled: true` 但没起 loginserver，全部 401

设计如此（fail-closed）：校验开启后 LoginGate 必须经 loginserver `ValidateLoginToken`，连不上或 gRPC 失败一律拒绝。本地/压测用 `false`。

### Q: 配置改了 shardCount / 限流阈值但不生效

「基本不改」项已进代码默认（`internal/config/defaults.go` + `ApplyRuntimeDefaults`）：yaml 未显式覆盖时以代码默认为准，显式写了以 yaml 为准。bool 的 `true` 默认无法用零值表达，需在 yaml 显式写出。

### Q: 改了 `security.enabled` / `rateLimit.enabled` 等开关不生效

`enabled` 类开关**仅启动时生效**，热更会被整单拒绝并告警 `security enable flags cannot hot-toggle`——需重启网关。其余参数（阈值、名单、secret 等）支持热更。

### Q: `maxFrameBufSize` / `maxWSBufferSize` 启动校验失败

帧缓冲必须 ≥ 帧上限 + 前缀长度（TCP +4、WS +14），否则大帧会静默断连——这是启动 Validate 的强制交叉校验，非法配置整体拒绝并保留当前值。

### Q: batchPush 模式需要客户端做什么

客户端需处理 `CmdPushBatch`（`9000002`）：解析 `MessageFrame.Body` 中的 `PushBatch`，遍历 `items` 逐条处理（item 携带 SeqId，可靠推送的 ACK 也由此发起）。

### Q: pprof 端口冲突 / 如何禁用

多实例各配不同 `monitoring.pprofAddr`（如 `:6060` / `:6061`）；置空 `""` 关闭。

### Q: Redis 挂了会怎样（封禁/会话）

封禁权威库在 loginserver（MySQL ACL），sgate 不连 Redis；loginserver 的 Redis 仅用于 token 缓存与会话索引，故障时降级回源 MySQL 并告警，sgate 不受影响（`ValidateLoginToken` gRPC 失败时 fail-closed 拒绝新登录）。

### Q: 压测出现 `connectex: Only one usage of each socket address`

Windows 临时端口池耗尽——上一轮 TCP 测试的 TIME_WAIT（240s）未过期。**两轮 TCP 测试间隔 >240s**（见 §7.1 第 3 条），或等 `netstat` 中 TIME_WAIT 归零再跑。

### Q: 如何在生产环境部署

1. Linux 服务器（Windows 连接天花板 16K）
2. etcd 集群（3 节点）
3. `loginValidation.enabled: true` + loginserver（封禁/Kick/校验）
4. 多实例对等部署（`discovery.registerSelf: true`，各自独立 `serverId`/端口），前置 Nginx/HAProxy（反代取真实 IP 仅 WS 支持 `trustedProxies`）
5. `protection.maxConnections` / `maxConnectionsPerIP` 设上限，`admin.token` 与 `kickInternalToken` 配好
6. 配置 `monitoring.pprofAddr`、告警 Webhook，日志接入采集

---

## 相关文档

- [`docs/architecture.md`](docs/architecture.md) — 架构权威文档（包职责、数据流、扩展点）
- [`docs/benchmark-report.md`](docs/benchmark-report.md) — 压测报告（场景 A~F、对账与瓶颈分析）
- [`docs/roadmap.md`](docs/roadmap.md) — 能力清单与待办（P2/P3）
- [`docs/udp-ws-design.md`](docs/udp-ws-design.md) — WS+UDP 双通道设计稿
- [`Bench.md`](Bench.md) — 限流下扫、churn 等历史压测明细
- [`AGENTS.md`](AGENTS.md) — AI 工程规范（日志、依赖方向、门禁、bench 规则）





