# sgate

`sgate` 是一个基于 gnet 的游戏网关。当前默认构建支持 TCP 和 WebSocket 客户端接入，以 gRPC 双向流连接逻辑服，并提供按会话推送、组广播和全服广播。

UDP 已从项目中移除。gnet 只承担 TCP stream 传输；WebSocket 是运行在独立 TCP 监听端口上的 RFC 6455 协议层。

## 当前能力

- TCP：4 字节大端长度前缀加 `MessageFrame` protobuf。
- WebSocket：HTTP Upgrade、二进制帧、客户端掩码、半包/粘包、分片、Ping/Pong、Close；文本帧和扩展帧会关闭连接。
- 会话：每条客户端连接生成 session ID；`LoginGateReq` 将 session 绑定到指定 logic server，logic 返回非空 `user_key` 后进入认证状态。
- 转发：绑定后，客户端 `MessageFrame` 被转换为 `StreamData` 并写入对应 logic server 的 gRPC 双向流。
- 下行：logic server 的 `StreamData` 或 Gateway unary RPC 被编码回原连接所属协议。
- 组能力：logic server 可调用 `JoinGroup`、`LeaveGroup`、`Broadcast` 和 `BroadcastAll`。

默认构建不保证 legacy 标签下的企业功能可用。`go test -tags legacy ./...` 当前会要求整理额外模块依赖，因此不能将 legacy 路径视为已验证的发布配置。

## 架构

```text
TCP client                         +-------------------+
  [len][MessageFrame]  ----------> |                   |
                                    |       sgate       | <---- gRPC bidirectional stream ----> Logic server
WebSocket client                   |                   |
  HTTP Upgrade                     | Session / Groups  | <---- Gateway unary RPC -----------> Logic server
  binary(MessageFrame) ----------> |                   |
                                    +-------------------+
```

客户端消息不会携带 `StreamData`。客户端 `MessageFrame.body` 是业务 protobuf 字节；网关仅使用 `cmd`、`seq_id` 与 `body` 构造内部 `StreamData`。

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

WebSocket payload 不包含 TCP 的 4 字节长度前缀。服务端下行会按 session 的 codec 自动采用 TCP 长度前缀或 WebSocket binary frame。

### 登录与转发

1. 客户端首先发送 `cmd=1000001` (`LoginGateReq`)，其中必须给出 `server_id`。
2. 网关在 `logicServers` 中查找同 zone 的 `server_id`，必要时连接该 logic server 并建立 gRPC stream。
3. 网关绑定 session，并回送 `cmd=1000002` (`LoginGateAck`)。
4. 之后的客户端消息被转为 `StreamData{session_id, user_key, cmd, data, client_ip}` 后异步写入该 logic stream。
5. logic server 返回带 `session_id` 的 `StreamData` 时，网关将 `cmd` 与 `data` 封装为 `MessageFrame` 下行。

连接关闭时，已认证 session 会向对应 logic server 发送 `cmd=1100012` 离线通知。

## 配置

`config/config.yaml` 是常规配置，`config/bench.yaml` 是本地压测配置。当前 transport 的 `protocol` 必须是 `tcp`；WebSocket 通过 `type: websocket` 区分。

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
  maxMessageSize: 8388608

logicServers:
  - serverId: "logic-1"
    serverType: "Logic"
    zone: "default"
    address: "localhost:50052"
```

| 配置 | 含义 |
|---|---|
| `transports[].port` | 客户端 TCP 监听端口；WebSocket 也使用 TCP 监听。 |
| `transports[].type` | 留空表示 TCP；`websocket` 表示 WebSocket。 |
| `logicServers` | `LoginGateReq.server_id` 到 logic server 地址的静态映射。 |
| `grpc.port` | logic server 调用 Gateway unary RPC 的监听端口。 |
| `protection.maxFrameSize` 等 | TCP 和 WebSocket codec 使用对应帧大小配置；WebSocket 最大值受 4 MiB 安全上限约束。 |

## 本地启动

以下流程使用压测回显逻辑服，不依赖 etcd、Nacos 或 Prometheus。

```powershell
# 终端 1：逻辑服，监听 :50052
go run ./examples/logic_server_min

# 终端 2：网关，TCP :48080、WebSocket :48081、gRPC :50051
go run ./cmd/gateway -conf config/bench.yaml
```

生产配置中的 etcd、discovery、configCenter 和 cluster 字段需按实际部署验证；本地压测配置均关闭这些外部依赖。配置中的 `tls.enabled` 当前会被启动校验拒绝，因为 gnet v2.9.7 没有 TLS listener 支持。

## 构建与检查

```powershell
go test ./...
go vet ./...

go build -o gateway.exe ./cmd/gateway
go build -o logic.exe ./examples/logic_server_min
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench
```

## 压测

### 工具

| 工具 | 场景 | 命令格式 |
|---|---|---|
| `examples/bench` | TCP 登录、登录、心跳双向回显 | `<addr> <conns> [duration] [batchSize] [inflight] [statsAddr] [serverId]` |
| `examples/ws_bench` | WebSocket Upgrade、登录、登录、心跳双向回显 | `<ws-url> <connections> <duration-seconds>` |
| `examples/push_bench` | TCP 的 personal/group/broadcast 推送 | 见工具 usage |

TCP 与 WebSocket 必须串行运行。并发向同一个本地 logic server 施压会共享 gRPC stream、CPU 与 socket 缓冲，不能用于协议性能比较。

```powershell
# TCP: 10 connections, 5 seconds, batch=16, max 8192 in-flight messages per connection
.\tcp_bench.exe 127.0.0.1:48080 10 5 16 8192 127.0.0.1:8081 logic-1

# WebSocket: 10 connections, 5 seconds, 8192 in-flight messages per connection (fixed in tool)
.\ws_bench.exe ws://127.0.0.1:48081/ 10 5
```

### 已记录结果

测试日期：2026-09-04。环境：Windows、本机 12 logical CPUs、Go 1.22.5；gateway、`logic_server_min` 与压测客户端在同一主机运行。使用 `config/bench.yaml`，业务负载是 18-byte TCP send frame 对应的 heartbeat protobuf；WebSocket 使用等价的 `MessageFrame` binary payload。每次测试前重启 gateway 和 logic server，TCP 与 WebSocket 串行执行。

| 协议 | 连接数 | 时长 | 接收总数 | 接收 QPS | 认证失败 | 客户端压测丢弃 |
|---|---:|---:|---:|---:|---:|---:|
| TCP | 1 | 2.01 s | 344,220 | 171,577 | 0 | 0 |
| WebSocket | 1 | 2.00 s | 300,999 | 150,402 | 0 | 不适用 |
| TCP | 10 | 5.00 s | 1,870,522 | 374,052 | 1 | 0 |
| WebSocket | 10 | 5.00 s | 1,321,790 | 264,186 | 0 | 不适用 |

这些数字是本机回显吞吐，不是生产容量承诺，也不代表网络延迟、TLS/WSS、业务处理、外部服务发现、消息大小变化或长时间稳定性。最新一轮 TCP 10 连接运行中出现 4 个客户端认证失败，因此该条仅用于量级比较，不能标注为零失败结果。WebSocket 工具的 QPS 是成功读取的服务端 binary 回包数；它通过每连接 8192 条在途消息限制发送背压。

默认网关会暴露 `/health`、`/ready`、`/live`、`/stats` 和 `/metrics`，地址由顶层 `port` 配置；`/ready` 在至少一个 logic stream 建立前返回 `503`。logic stream 断开后会按 1、2、4 秒递增、最多 30 秒的退避策略重连。压测结束后保留 gateway 进程时，logic 已停止，所以 `/ready` 返回 `503` 是预期结果。

## 项目目录

```text
cmd/gateway/                 Gateway CLI entrypoint
internal/gateway.go          gnet event handler, session routing and client framing
internal/codec/              TCP and WebSocket codecs
internal/grpc_server.go      logic stream management and Gateway RPC implementation
internal/session.go          session state and session index
internal/groups.go           group membership and broadcast iteration
examples/logic_server_min/   local echo logic server
examples/bench/              TCP duplex benchmark
examples/ws_bench/           WebSocket duplex benchmark
config/config.yaml           normal configuration
config/bench.yaml            local benchmark configuration
DESIGN.md                    implementation-oriented design document
```
