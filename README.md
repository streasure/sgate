# sgate

`sgate` 是一个基于 gnet 的高性能游戏网关。默认构建支持 TCP 和 WebSocket 客户端接入，以 gRPC 双向流连接逻辑服，并提供按会话推送、组广播和全服广播。

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
                                | SessionManager / GroupManager     |
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
3. 网关绑定 session，回送 `cmd=1000002` (`LoginGateAck`)。
4. 后续消息被转为 `StreamData{session_id, user_key, cmd, data, client_ip}` 异步写入 logic stream。
5. logic server 返回带 `session_id` 的 `StreamData` 时，网关封装为 `MessageFrame` 下行。
6. 连接关闭时，已认证 session 发送 `cmd=1100012` 离线通知。

### Logic 推送

Logic server 通过 Gateway unary RPC 推送，使用 `userKey` 或 `groupID` 标识目标：

| RPC | 行为 |
|---|---|
| `SendToClient` | 按 session 推送 |
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

`config/config.yaml` 为生产配置（含 etcd、discovery），`config/bench.yaml` 为本地压测配置（关闭外部依赖）。

## 快速开始

```powershell
# 终端 1：逻辑服
go run ./examples/logic_server_min

# 终端 2：网关
go run ./cmd/gateway -conf config/bench.yaml
```

## 构建

```powershell
go build -o sgate.exe ./cmd/gateway
```

无需特殊编译参数。`go build` 产出的二进制可直接用于生产。如需检测数据竞态：

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

| Transport | 连接数 | 时长 | 接收消息 | 平均 Recv QPS | Auth 失败 | 丢弃 |
|---|---:|---:|---:|---:|---:|---:|
| TCP | 10 | 10s | 3,486,555 | **348,621** | 6 | 0 |
| WebSocket | 10 | 10s | 2,314,021 | **231,335** | 0 | 0 |

> 这些数字是本机回环吞吐，不是生产容量承诺。TCP Auth 失败为压测工具启动时序导致，非网关瓶颈。

### 工具

```powershell
# TCP
go run ./examples/bench 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1

# WebSocket
go run ./examples/ws_bench ws://127.0.0.1:48081/ 10 10
```

TCP 与 WebSocket 必须串行运行，并发运行会使协议对比失效。

## 项目结构

```text
cmd/gateway/                  CLI 入口
internal/
  frontend.go                 Gateway 主逻辑（legacy 默认构建）
  backend.go                  gRPC stream 管理
  connection.go               ConnectionManager
  session.go                  Session 状态管理
  groups.go                   组管理
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
config/
  config.yaml                 生产配置
  bench.yaml                  压测配置
DESIGN.md                     设计文档
```

## 已知限制

- gnet v2 不原生支持 TLS，当前仅支持明文 TCP 和 WebSocket。需要 WSS 时需升级或替换网络层。
- logic stream 重连不会恢复断线期间丢弃的消息；需要业务幂等或持久化队列。
- WebSocket 单元测试已覆盖核心场景（畸形帧、分片、Upgrade 半包），边界 case 可继续扩展。
