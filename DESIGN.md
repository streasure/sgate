# sgate 设计文档

## 范围

本文档描述仓库的默认构建。带 `//go:build simple` 标签的文件为简化替代实现，不视为生产默认路径。

默认网关支持两种客户端 codec，且两者均运行在 TCP 监听器上：

- TCP Length-Value 帧。
- RFC 6455 WebSocket 二进制帧。

UDP 已明确移除；项目中没有 UDP 源码、配置、说明或压测路径。

## 运行拓扑

```text
                               Gateway unary RPC
                       +--------------------------------+
                       | Close / Kick / Send / Broadcast |
                       | Join / Leave / GroupInfo        |
                       +----------------+---------------+
                                        ^
                                        | gRPC :50051
+----------------+  TCP :48080  +------+----------------------------+  gRPC stream  +----------------+
| TCP client     | -----------> | gnet event loops                  | <-----------> | logic server   |
+----------------+              |                                  |                +----------------+
                                 | ConnectionManager / GroupManager |
+----------------+  TCP :48081  | ConnectionManager                |
| WebSocket      | -----------> | TCPCodec / WebSocketCodec         |
| client         | HTTP Upgrade | Security / Observability          |
+----------------+              +----------------------------------+
```

`TransportComponent` 为每个 transport 启动一个 gnet engine。当前两个 transport 的 `protocol` 都是 `tcp`，`type: websocket` 使该监听端口创建的连接使用 `WebSocketCodec`。启动选项启用 multicore、reuse-port、256 KiB gnet 读写缓冲、4 MiB socket 缓冲与 TCP_NODELAY。

## 企业级特性

默认构建包含以下企业级组件：

### 安全防护

| 组件 | 功能 |
|---|---|
| `WhitelistBlacklist` | IP 白名单/黑名单 |
| `RateLimiter` | 令牌桶限流（按 IP / 路由） |
| `WAF` | Web 应用防火墙（规则匹配） |
| `CircuitBreakerManager` | 熔断器（按路由统计成功率） |
| `MessageIntegrity` | 消息完整性校验（防重放） |
| `JWTAuthFilter` | JWT 鉴权过滤器 |

### 观测性

| 组件 | 功能 |
|---|---|
| `ObservabilityComponent` | HTTP 健康检查端点 |
| `OTelTracer` | OpenTelemetry 分布式追踪 |
| `PrometheusExporter` | Prometheus 指标导出 |
| `LatencyTracker` | 延迟追踪 |

### 流量管理

| 组件 | 功能 |
|---|---|
| `TrafficMirror` | 流量镜像 |
| `DegradationManager` | 降级管理 |
| `OverloadProtector` | 过载保护（CPU 阈值） |

### 集群

| 组件 | 功能 |
|---|---|
| `Cluster` | etcd 集群管理 |
| `Balancer` | 负载均衡 + 故障节点摘除 |
| `ConfigCenter` | etcd 配置中心 |
| `ServiceDiscovery` | 服务发现 |

### 过滤器链

`FilterChain` 支持 SPI 模式的请求过滤，按阶段执行：

- `PhasePreAuth`：预鉴权（限流、WAF、熔断）
- `PhaseAuth`：鉴权（JWT）
- `PhaseForward`：转发（流量镜像、降级）

## 连接与 Codec

`Gateway.OnOpen` 创建 `Connection`，按本地监听端口选择 codec，然后以 connection ID 与 `gnet.Conn` 双索引保存；connection/session ID 只属于网关内部连接路由。

`Gateway.OnTraffic` 调用 codec 的 `Decode`，转发每一个完整的 `MessageFrame` payload。`Gateway.sendToSession` 通过同一连接的 `Encode` 下行，因此回复、个人推送、组广播和全服广播都维持客户端接入协议。

### Codec 抽象

```go
type Codec interface {
    Decode(ctx context.Context, conn gnet.Conn) ([][]byte, error)
    Encode(buf []byte) []byte
}
```

`NewCodec(protocol)` 根据协议字符串选择实现：

- `"tcp"` → `TCPCodec`（Length-Value 帧）
- `"websocket"` → `WebSocketCodec`（RFC 6455 二进制帧）

添加新协议只需实现 `Codec` 接口并注册到 `NewCodec`。

### TCP Codec

```text
+----------------------+-----------------------+
| uint32 big endian N  | N bytes MessageFrame  |
+----------------------+-----------------------+
```

`TCPCodec.Decode` 在一个流量事件中循环解析所有完整帧；不完整头和 payload 保留在 gnet 入站缓冲。完成的 payload 会复制后返回，避免 gnet 复用入站内存导致数据失效。

### WebSocket Codec

WebSocket codec 为每个连接维护 Upgrade 状态及可选的分片消息状态。

HTTP Upgrade 要求：

- 请求行以 `GET ` 开头。
- `Upgrade: websocket`。
- `Connection` 包含 `Upgrade`。
- `Sec-WebSocket-Version: 13`。
- `Sec-WebSocket-Key` 非空。

codec 只消费 HTTP `\r\n\r\n` 结束符之前的数据，写回 `101 Switching Protocols`，并保留同次 gnet 入站缓冲中可能已经到达的首个 WebSocket 帧。

数据帧约束：

- 客户端帧必须使用掩码。
- 只有 binary data message 会转发到网关。
- binary 分片消息会完成重组后再转发。
- 重组后消息上限为 4 MiB。
- Ping 返回相同 payload 的 Pong。
- Close 返回 Close，随后事件处理器关闭连接。
- text、extension、无掩码、无效控制帧和未知 opcode 都会被拒绝。

WebSocket payload 就是序列化后的 `MessageFrame`，不包含 TCP 4 字节长度前缀；服务端发送未掩码的 binary frame。

## 客户端与 Logic 协议

### 客户端 MessageFrame

```protobuf
message MessageFrame {
  int32 cmd = 1;
  int64 seq_id = 2;
  bytes body = 99;
}
```

`ExtractMessageFrame` 通过 `protowire` 扫描字段，不反序列化整个 envelope。有效帧必须含有非零 `cmd` 与非空 `body`。

### 内部 StreamData

网关把合法客户端帧转换为 protocol 模块的 `gateway.StreamData`：

```text
StreamData {
  session_id = connection.ID()
   user_key   = connection.GetUserUUID() // protocol 字段名沿用 user_key，语义为 userUUID
  cmd        = MessageFrame.cmd
  data       = MessageFrame.body
  client_ip  = connection.IP()
}
```

gRPC 双向流传输 `StreamData`。下行 `StreamData` 带有 `session_id` 时，网关将它转换为 `MessageFrame{cmd, seq_id: 0, body: data}`，再通过目标连接的 codec 下行。

## Connection 生命周期

```text
OnOpen
  |
  v
StateConnected
  | LoginGateReq (cmd 1000001)
  v
StateBound
  | logic response with non-empty user_key (userUUID)
  v
StateAuthenticated
  | OnClose
  v
删除 connection、删除组成员；仅 authenticated 时才通知 logic
```

`LoginGateReq` 携带目标 logic server ID。`Gateway.handleLoginGate` 校验目标 logic server ID，绑定连接并下行 `LoginGateAck`（`cmd=1000002`）。未绑定连接除了 `LoginGateReq` 以外的帧都会被静默忽略。

gRPC 下行路径收到带非空 `user_key`（业务语义为 userUUID）的同 session 消息时调用 `Connection.Authenticate`；连接关闭时，已认证连接会发送 `CmdUserOffline` 离线通知。

当前 logic 连接行为：

- 每个 logic server ID 对应一个 `LogicClient` 和容量为 1024 的上行 channel。
- channel 满时记录 warning 并丢弃该条上行消息。
- gRPC 接收循环依据 session ID 分发下行消息；业务单用户推送由 logic 先用 userUUID 映射到该 session ID。
- stream 断开后会移除旧连接，并按 1、2、4 秒递增、最多 30 秒的退避策略自动重连；网关关闭时会停止重连。

## Logic 主动操作

Gateway gRPC service 当前实现以下 unary RPC。

| RPC | Gateway 行为 |
|---|---|
| `CloseSession`、`KickSession` | 关闭目标 gnet 连接。 |
| `SendToClient` | 对指定 session_id 编码并异步写入；logic 业务层优先使用 `Server.SendToUser(userUUID, ...)`。 |
| `Broadcast` | 遍历请求中的每个组并下行给组成员。 |
| `BroadcastAll` | 遍历全部活跃 session 并下行。 |
| `JoinGroup`、`LeaveGroup` | 更新 `GroupManager` 和连接的 group set。 |
| `GetGroupInfo` | 返回当前成员数与 session ID。 |

组在第一次加入时隐式创建，最后成员离开或 session 关闭时隐式清理。

Logic 层单用户推送使用 `userUUID`，不直接要求业务代码提供 `sessionID`。
logic 维护 `userUUID -> sessionID` 映射，推送时先根据 userUUID 找到 sessionID，再通过 GatewayStream 发送；
sgate 的 `ConnectionManager` 负责本地 `userUUID ↔ sessionID` 映射和最终客户端写入。
组推送使用 `groupID`，组成员内部仍由 `serverID + userUUID` 关联到连接。

需要直接操作某个网关连接时，Gateway unary RPC 的 `SendToClient` 仍使用 `session_id`；这是底层管理接口，不是 logic 业务层单用户推送的首选入口。

## 配置

`config.LoadConfig` 先创建硬编码默认值，再将所选 YAML 解码到该结构中。单个 YAML 字段没有环境变量覆盖；`PORT`、`LOG_LEVEL`、`GATEWAY_SERVER_ID` 仅在默认值构造时使用。

本地压测使用唯一网关配置 `config/config.yaml`：

```yaml
transports:
  - protocol: tcp
    port: 48080
  - protocol: tcp
    port: 48081
    type: websocket

grpc:
  port: 50051

logicServers:
  - serverId: "logic-1"
    serverType: "Logic"
    zone: "default"
    address: "localhost:50052"
```

该配置默认关闭 `etcd`、`discovery`、`configCenter`、`cluster` 和 monitoring，可直接运行；生产部署在同一配置文件中覆盖外部依赖和安全参数。

## 性能特征

默认路径利用 gnet event loop、单次流量事件的多帧解析、protobuf wire envelope 提取、按连接写合并和 gRPC stream 分片；仍需要在多机环境验证客户端背压、TLS/WSS、跨网关 fan-out 和故障恢复。

因此容量由本地 logic server、gRPC stream、codec 分配、内核缓冲和客户端发送行为共同决定，不能仅根据 gnet 推导。

## 压测方法与结果

### 方法

记录日期：2026-09-08。环境：Windows、12 logical CPUs、Go 1.22.5。gateway、`examples/logic_server_min` 与压测客户端运行在同一主机。逻辑服对登录请求应答，对收到的 stream 消息回显。此次测试使用 `config/config.yaml`，关闭 etcd/discovery/cluster。

| 项目 | TCP | WebSocket |
|---|---|---|
| Gateway 配置 | `config/config.yaml` | `config/config.yaml` |
| 监听地址 | `127.0.0.1:48080` | `ws://127.0.0.1:48081/` |
| 登录 server ID | `logic-1` | `logic-1` |
| 负载 | 登录后心跳双向回显 | HTTP Upgrade、登录后心跳双向回显 |
| 在途上限 | 每连接 8192 消息 | 每连接 8192 消息 |
| 隔离 | 每轮前重启 gateway 与 logic | 每轮前重启 gateway 与 logic |

命令：

```powershell
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o sgate.exe ./cmd/gateway
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench

.\logic_server_min.exe
.\sgate.exe -conf config/config.yaml

.\tcp_bench.exe 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1
.\ws_bench.exe ws://127.0.0.1:48081/ 10 10
```

TCP 和 WebSocket 不得并发运行；二者共享同一 gateway、logic process、gRPC stream 与 loopback socket，并发运行会使协议对比失效。

### 结果

| Transport | 连接数 | 标称时长 | 接收消息 | 平均接收 QPS | 认证失败 | 客户端丢弃 |
|---|---:|---:|---:|---:|---:|---:|
| TCP，inflight=8192 | 10 | 10.03 s | 51,008 | 9,995 | 997 | 5 |
| TCP，inflight=256 | 10 | 10.02 s | 12,688 | 9,990 | 997 | 0 |
| WebSocket | 10 | 10.02 s | 91,920 | 10,000 | 998 | 0 |

TCP 稳定档的 sgate 统计为：接收 12,708、转发 10,000、转发丢弃 2,698、回推 10,000、回推无连接丢弃 0。TCP 高 inflight 档为：接收 51,028、转发 10,000、转发丢弃 41,018、回推无连接丢弃 10。WebSocket 客户端结果为发送 91,920、接收 10,000，认证失败 0。高 inflight 结果受 `logic_server_min` echo 能力和网关队列限制影响。

真实主动推送使用 `examples/push_driver`，结果如下：10 个客户端、10 秒、1,000 个逻辑事件/s，`SendToUser` 收到 6,530 条、约 653 QPS；10 人组 `SendToGroup` 收到 66,125 条、约 6,609 QPS；10 人 `Broadcast` 收到 65,731 条、约 6,569 QPS。

`examples/logic_noop` + `examples/forward_bench` 的 no-op 纯转发速率阶梯结果为：目标 5,000 msg/s 时实际 offered 3,312、转发 3,314、丢弃 0；目标 10,000 时实际 offered 6,631、转发 6,633、丢弃 0；目标 20,000 时实际 offered 13,300、转发 10,287、丢弃 15,110。Windows 10ms 批量调度使实际 offered load 约为目标的 2/3，本次无丢弃稳定转发约为 6.6K msg/s。

使用 `ghz v0.120.0`、20 并发对 Gateway unary `GetGroupInfo` 压测 5 秒：225,191 请求，45,040 req/s，平均 0.28ms，P95 1.00ms，P99 2.02ms；正常请求 225,179 次 OK，关闭连接阶段 12 次错误不计入正常吞吐。

使用生产配置启动 gateway 的验证中，gnet TCP、WebSocket 和 Prometheus 监听均成功；由于未启动 logic，`/health` 与 `/ready` 返回 503。etcd 可访问，但未启动两个 gateway 实例，因此没有执行 GatewayClientPool 的跨网关吞吐测试。

这些数据仅用于同机 loopback 的协议量级比较，不能作为生产 QPS 承诺，也不能外推到不同主机、网络、payload、并发、logic 实现或业务逻辑。完整判定标准和测试矩阵见 `docs/performance.md`。

## 可观测性

默认构建会在顶层 `port`（默认为 `:8081`）启动 HTTP 服务：`/health`、`/live`、`/ready`、`/stats` 和 `/metrics`。`/ready` 在没有 logic stream 时返回 `503`。

## 验证状态

```powershell
go build ./...
go test ./...
go vet ./...

go build -o sgate.exe ./cmd/gateway
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench
```

以上均为标准构建，不使用任何 Go build tag。压测使用上述普通构建产物，不使用 `go run` 或特殊编译参数。WebSocket 单元测试覆盖：Upgrade 握手（正常、半包、缺失字段、超大 header、非 GET、X-Forwarded-For）、畸形帧（未掩码、RSV 扩展、超大消息、控制帧无 FIN、不支持 opcode）、分片（多帧、三帧、超大分片、交织数据帧、文本拒绝、孤立 continuation）、控制帧（Ping/Pong、Close）、编码（小/16bit/64bit 长度、roundtrip）。

## 已知缺口

- malformed WebSocket frame、fragmentation 与 Upgrade 半包的单元测试已补充核心场景，边界 case 可继续扩展。
- 默认 gnet 版本不支持 TLS listener，因此当前只能部署 TCP 和明文 WebSocket；需要 WSS 时必须升级或替换网络层。
- logic stream 重连不会恢复断线期间已经丢弃的消息；需要业务幂等或持久化队列保证语义。
- Group/session 遍历已复制 session 列表后再执行下行写入，不再持有 manager 读锁；大规模 fan-out 仍应先 profiling。
- `go mod tidy` 已修复（移除了未使用的 `cilium/ebpf` 依赖）。
