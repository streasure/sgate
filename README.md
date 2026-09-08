# sgate

`sgate` 是一个基于 gnet 的高性能游戏网关。默认构建支持 TCP 和 WebSocket 客户端接入，以 gRPC 双向流连接逻辑服，并提供按 userUUID 推送、组广播和全服广播。

## 核心特性

- **双协议接入**：TCP（4 字节长度前缀）和 WebSocket（RFC 6455 二进制帧），通过 codec 策略模式实现协议热插拔。
- **企业级安全**：IP 黑白名单、令牌桶限流、WAF、熔断器、消息完整性校验（防重放）、JWT 鉴权。
- **可观测性**：HTTP 健康检查（`/health`、`/ready`、`/live`、`/stats`）、Prometheus 指标、OpenTelemetry 分布式追踪。
- **流量管理**：流量镜像、降级管理、过载保护（CPU 阈值）。
- **集群支持**：etcd 服务注册与发现、负载均衡、配置中心。
- **过滤器链**：SPI 模式请求过滤（预鉴权 → 鉴权 → 转发）。

## 架构

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

## 客户端协议

### MessageFrame

```protobuf
message MessageFrame {
  int32 cmd = 1;
  int64 seq_id = 2;
  bytes body = 99;
}
```

| 接入类型 | 线上格式 |
|---|---|
| TCP | `[4-byte big-endian MessageFrame length][MessageFrame protobuf]` |
| WebSocket | RFC 6455 binary message，payload 直接为 `MessageFrame protobuf` |

### 登录与转发

1. 客户端发送 `cmd=1000001` (`LoginGateReq`)，指定 `server_id`。
2. 网关在 `logicServers` 中查找同 zone 的 `server_id`，必要时建立 gRPC stream。
3. 网关绑定 connection，并回送 `cmd=1000002` (`LoginGateAck`)。
4. 后续消息被转为 `StreamData{session_id, user_key, cmd, data, client_ip}` 异步写入 logic stream，其中协议字段 `user_key` 承载业务 `userUUID`。
5. logic server 返回带 `session_id` 的 `StreamData` 时，网关封装为 `MessageFrame` 下行。
6. 连接关闭时，已认证 session 发送 `cmd=1100012` 离线通知。

### Logic 推送

Logic 层单用户推送使用 `userUUID`，组推送使用 `groupID`。logic 内部维护
`userUUID -> sessionID` 映射，但 `sessionID` 只是网关连接的内部路由标识，不要求业务调用方提供。
logic 通过 `SendToUser(userUUID, ...)` 解析目标后，经 GatewayStream 将消息发送到 sgate。
Gateway unary RPC 仍提供按 `session_id` 的底层 `SendToClient` 接口，主要用于网关内部或需要精确连接控制的场景。

| RPC | 行为 |
|---|---|
| `SendToClient` | 按 session_id 的底层推送；logic 业务层优先使用 `SendToUser(userUUID, ...)` |
| `Broadcast` | 按组广播 |
| `BroadcastAll` | 全服广播 |
| `JoinGroup` / `LeaveGroup` | 组管理 |

## 配置

```yaml
transports:
  - protocol: tcp
    port: 48080
  - protocol: tcp
    port: 48081
    type: websocket

grpc:
  port: 50051
  windowSize: 67108864

logicServers:
  - serverId: "logic-1"
    serverType: "Logic"
    zone: "default"
    address: "localhost:50052"
```

| 配置 | 含义 |
|---|---|
| `transports[].protocol` | 必须为 `tcp`；WebSocket 通过 `type: websocket` 区分。 |
| `transports[].port` | 客户端监听端口。 |
| `transports[].type` | 留空为 TCP；`websocket` 为 WebSocket。 |
| `logicServers` | `LoginGateReq.server_id` 到 logic server 的静态映射。 |
| `grpc.port` | logic server 调用 Gateway unary RPC 的端口。 |
| `grpc.advertiseAddr` | 注册到 etcd、供其他网关访问的 gRPC 地址。 |

`config/config.yaml` 为生产配置（含 etcd、discovery），`config/bench.yaml` 为本地压测配置（关闭外部依赖）。

## 快速开始

```powershell
# 先构建所有普通构建产物
go build ./...
go build -o sgate.exe ./cmd/gateway
go build -o logic_server_min.exe ./examples/logic_server_min

# 终端 1：逻辑服
.\logic_server_min.exe

# 终端 2：网关
.\sgate.exe -conf config/bench.yaml
```

## 构建

```powershell
go build -o sgate.exe ./cmd/gateway
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench
```

以上均为普通 `go build`，不需要 `-tags`、`simple` 或 `legacy` 参数。`sgate.exe` 是生产网关构建产物，压测必须使用该产物，不使用 `go run` 临时编译。

如需检测数据竞态，可在支持 CGO 的 64 位 C 工具链环境执行：

```powershell
go build -race -o sgate.exe ./cmd/gateway
```

## 测试

```powershell
go test ./...
go vet ./...
```

## 压测

### 环境

- Windows，12 logical CPUs，Go 1.22.5
- gateway、`logic_server_min`、压测客户端同一主机
- 使用 `config/bench.yaml`（关闭 etcd/discovery/cluster）
- 逻辑服对登录请求应答，对心跳请求回显

### 结果

本次实际执行日期：2026-09-08。环境为 Windows、12 logical CPUs、Go 1.22.5，gateway、logic 和压测工具运行在同一主机。TCP/WS 测试均使用 10 个连接、10 秒、batchSize=16；TCP 稳定档使用 inflight=256。结果中的 `Recv QPS` 是客户端成功收到的回包速率，不是网关理论最大吞吐。

| 场景 | 连接数 | 时长 | 客户端发送 | 客户端接收 | 发送 QPS | 接收 QPS | 认证失败 |
|---|---:|---:|---:|---:|---:|---:|---:|
| TCP，inflight=8192 | 10 | 10.03s | 51,008 | 9,995 | 5,085 | 997 | 5 |
| TCP，inflight=256 | 10 | 10.02s | 12,688 | 9,990 | 1,266 | 997 | 0 |
| WebSocket | 10 | 10.02s | 91,920 | 10,000 | 9,174 | 998 | 0 |

TCP 稳定档对应的 sgate `/stats` 为：接收 12,708、转发 10,000、客户端回推 10,000、转发丢弃 2,698、回推无连接丢弃 0。高 inflight 档对应：接收 51,028、转发 10,000、转发丢弃 41,018、回推无连接丢弃 10。高 inflight 档说明示例 logic echo 处理能力不足时会触发网关队列丢弃，不能作为无丢包性能结果。

`push_bench` 在 `personal`、`group`、`broadcast` 三种模式下均得到相近结果：

| 模式 | 连接数 | 时长 | 客户端发送 | 客户端接收 | 平均发送 QPS | 平均接收 QPS |
|---|---:|---:|---:|---:|---:|---:|
| personal | 10 | 10.02s | 12,656 | 9,990 | 1,263 | 997 |
| group | 10 | 10.02s | 12,704 | 9,990 | 1,268 | 997 |
| broadcast | 10 | 10.02s | 12,656 | 9,990 | 1,263 | 997 |

上述三个模式是旧版 `push_bench` 的 stream echo 结果，不代表主动 fan-out 性能。真实主动推送结果见下方“纯 sgate 转发与主动推送”部分。

### 纯 sgate 转发与主动推送

为隔离 logic 性能，新增 `examples/logic_noop` 和 `examples/forward_bench`。no-op logic 只接收并丢弃 `StreamData`，不解析、不回包；转发结果以 sgate `/stats` 的 `forwarded` 和 `dropped` 为准。

10 个 TCP 客户端、5 秒速率阶梯测试：

| Driver 目标 | 实际 offered load | sgate 转发 | 转发丢弃 |
|---:|---:|---:|---:|
| 5,000 msg/s | 3,312 msg/s | 3,314 msg/s | 0 |
| 10,000 msg/s | 6,631 msg/s | 6,633 msg/s | 0 |
| 20,000 msg/s | 13,300 msg/s | 10,287 msg/s | 15,110 |

本次无丢弃稳定转发约为 6.6K msg/s；Windows 10ms 批量调度使实际 offered load 约为目标值的 2/3。目标 100,000 msg/s 的过载轮实际 offered 70,976 msg/s，sgate 转发 10,341 msg/s，丢弃 606,851，因此不作为稳定性能结果。

真实主动推送使用 `examples/push_driver`，10 个客户端登录后建立 `userUUID -> sessionID` 映射，连续 10 秒、目标 1,000 个逻辑事件/s：

| 模式 | 实际发送到客户端 | 客户端收到 | 接收 QPS |
|---|---:|---:|---:|
| `SendToUser` 单用户 | 6,540 | 6,530 | 653 |
| `SendToGroup` 10 人组 | 66,200 | 66,125 | 6,609 |
| `Broadcast` 10 人全体 | 65,800 | 65,731 | 6,569 |

这些结果是实际 sgate 下行写出和 fan-out 结果，不是 logic echo 性能。另用 `ghz v0.120.0` 对 Gateway unary `GetGroupInfo` 做 5 秒、20 并发测试：225,191 请求，45,040 req/s，平均延迟 0.28ms，P95 1.00ms，P99 2.02ms；关闭连接阶段产生的 12 次错误不计入正常请求。

启用 `config/config.yaml` 的 etcd 配置启动验证时，gateway 成功监听 TCP `:48080`、WebSocket `:48081` 和 Prometheus `:9101`；由于本次未启动 logic server，HTTP `/health` 和 `/ready` 均返回 `503`，与当前 readiness 语义一致。未进行双 gateway 的 GatewayClientPool 吞吐压测。

> 这些数字是本机 loopback 测试，不是生产容量承诺。测试未覆盖多机网络、TLS/WSS、长稳运行和更大 payload。

### 工具

```powershell
# 构建压测产物
go build -o sgate.exe ./cmd/gateway
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench
go build -o logic_noop.exe ./examples/logic_noop
go build -o forward_bench.exe ./examples/forward_bench
go build -o push_driver.exe ./examples/push_driver

# 终端 1：逻辑服
.\logic_server_min.exe

# 终端 2：网关
.\sgate.exe -conf config/bench.yaml

# TCP
.\tcp_bench.exe 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1

# WebSocket
.\ws_bench.exe ws://127.0.0.1:48081/ 10 10

# push stream echo 场景；personal/group/broadcast 依次串行执行
.\push_bench.exe 127.0.0.1:48080 10 10 personal 16 256
.\push_bench.exe 127.0.0.1:48080 10 10 group 16 256
.\push_bench.exe 127.0.0.1:48080 10 10 broadcast 16 256

# 纯转发：先启动 logic_noop 和 sgate，再执行
.\logic_noop.exe
.\forward_bench.exe 127.0.0.1:48080 10 5 127.0.0.1:8081 10000

# 真实主动推送：driver 内嵌 logic，先启动 sgate
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 personal 10 10 1000
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 group 10 10 1000
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 broadcast 10 10 1000
```

TCP 与 WebSocket 必须串行运行，并发运行会使协议对比失效。

## 项目结构

```text
cmd/gateway/                  CLI 入口
internal/
  frontend.go                 Gateway 主逻辑
  backend.go                  gRPC stream 管理
  connection.go               ConnectionManager、用户与组管理
  codec/                      TCP / WebSocket codec（策略模式）
  security/                   JWT、限流、WAF、熔断
  obs/                        OpenTelemetry、延迟追踪
  traffic/                    流量镜像、降级、eBPF stub
  cluster/                    集群、负载均衡、告警
  config/                     配置加载与校验
examples/
  logic_server_min/           本地回显逻辑服
  bench/                      TCP 压测工具
  ws_bench/                   WebSocket 压测工具
  logic_noop/                 no-op logic 转发测试服务
  forward_bench/              纯 sgate 上行转发压测工具
  push_driver/                真实主动推送压测工具
config/
  config.yaml                 生产配置
  bench.yaml                  压测配置
DESIGN.md                     设计文档
```

## 已知限制

- gnet v2 不原生支持 TLS，当前仅支持明文 TCP 和 WebSocket。需要 WSS 时需升级或替换网络层。
- logic stream 重连不会恢复断线期间丢弃的消息；需要业务幂等或持久化队列。
- logic 单用户推送以 `userUUID` 为业务目标，sgate/stream 内部再解析为 `sessionID`；同一 userUUID 在同一逻辑服上应保持唯一在线连接。
- WebSocket 单元测试已覆盖核心场景（畸形帧、分片、Upgrade 半包），边界 case 可继续扩展。
