# sgate 设计文档

## 范围

本文档描述仓库的默认构建，不将带 `legacy` build tag 的文件视为可运行实现。

默认网关只支持两种客户端 codec，且两者均运行在 TCP 监听器上：

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
                                | SessionManager / GroupManager     |
+----------------+  TCP :48081  |                                  |
| WebSocket      | -----------> | TCPCodec / WebSocketCodec         |
| client         | HTTP Upgrade |                                  |
+----------------+              +----------------------------------+
```

`TransportComponent` 为每个 transport 启动一个 gnet engine。当前两个 transport 的 `protocol` 都是 `tcp`，`type: websocket` 使该监听端口创建的 Session 使用 `WebSocketCodec`。启动选项启用 multicore、reuse-port、256 KiB gnet 读写缓冲、4 MiB socket 缓冲与 TCP_NODELAY。

## 连接与 Codec

`Gateway.OnOpen` 创建 `Session`，按本地监听端口选择 codec，然后以 session ID 与 `gnet.Conn` 双索引保存。

`Gateway.OnTraffic` 调用 Session 的 `Decode`，转发每一个完整的 `MessageFrame` payload。`Gateway.sendToSession` 通过同一 Session 的 `Encode` 下行，因此回复、个人推送、组广播和全服广播都维持客户端接入协议。

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

codec 会从 `X-Forwarded-For` 的第一个合法 IP 或 `X-Real-IP` 提取 IP。但默认 `Session.IP` 仍使用建连时的 TCP peer IP；若要信任代理头，需要补充显式的可信代理策略和 Session IP 覆盖逻辑。

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
  session_id = session.ID()
  user_key   = session.UserKey()
  cmd        = MessageFrame.cmd
  data       = MessageFrame.body
  client_ip  = session.IP()
}
```

gRPC 双向流传输 `StreamData`。下行 `StreamData` 带有 `session_id` 时，网关将它转换为 `MessageFrame{cmd, seq_id: 0, body: data}`，再通过目标 Session codec 下行。

## Session 生命周期

```text
OnOpen
  |
  v
StateConnected
  | LoginGateReq (cmd 1000001)
  v
StateBound
  | logic response with non-empty user_key
  v
StateAuthenticated
  | OnClose
  v
删除 session、删除组成员；仅 authenticated 时才通知 logic
```

`LoginGateReq` 携带目标 logic server ID。`Gateway.handleLoginGate` 在 `Config.LogicServer` 中校验 server ID，必要时以 `ConnectLogic` 建立 gRPC client stream，然后绑定 session 并下行 `LoginGateAck`（`cmd=1000002`）。未绑定连接除了 `LoginGateReq` 以外的帧都会被静默忽略。

gRPC 下行路径收到带非空 `user_key` 的同 session 消息时调用 `Session.Authenticate`；连接关闭时，已认证 session 会发送 `CmdUserOffline` 离线通知。

当前 logic 连接行为：

- 每个 logic server ID 对应一个 `LogicConn` 和容量为 1024 的上行 channel。
- channel 满时记录 warning 并丢弃该条上行消息。
- gRPC 接收循环依据 session ID 分发下行消息。
- stream 断开后会移除旧连接，并按 1、2、4 秒递增、最多 30 秒的退避策略自动重连；网关关闭时会停止重连。

## Logic 主动操作

Gateway gRPC service 当前实现以下 unary RPC。

| RPC | Gateway 行为 |
|---|---|
| `CloseSession`、`KickSession` | 关闭目标 gnet 连接。 |
| `SendToClient` | 对指定 session 编码并异步写入。 |
| `Broadcast` | 遍历请求中的每个组并下行给组成员。 |
| `BroadcastAll` | 遍历全部活跃 session 并下行。 |
| `JoinGroup`、`LeaveGroup` | 更新 `GroupManager` 和 Session 的 group set。 |
| `GetGroupInfo` | 返回当前成员数与 session ID。 |

组在第一次加入时隐式创建，最后成员离开或 session 关闭时隐式清理。

## 配置

`config.LoadConfig` 先创建硬编码默认值，再将所选 YAML 解码到该结构中。单个 YAML 字段没有环境变量覆盖；`PORT`、`LOG_LEVEL`、`GATEWAY_SERVER_ID` 仅在默认值构造时使用。

本地压测使用 `config/bench.yaml`：

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

该文件关闭 `etcd`、`discovery`、`configCenter`、`cluster` 和 monitoring。`config/config.yaml` 含外部集成配置，不能假定在未部署其依赖时可直接运行。

## 性能特征

默认路径利用 gnet event loop、单次流量事件的多帧解析、protobuf wire envelope 提取和异步客户端写入。当前未实现写合并、批量 gRPC 转发、客户端背压反馈或 TLS/WSS；TCP/WS 帧大小限制、空闲连接回收、stream 退避重连已实现。

因此容量由本地 logic server、gRPC stream、codec 分配、内核缓冲和客户端发送行为共同决定，不能仅根据 gnet 推导。

## 压测方法与结果

### 方法

记录日期：2026-09-04。环境：Windows、12 logical CPUs、Go 1.22.5。gateway、`examples/logic_server_min` 与压测客户端运行在同一主机。逻辑服对登录请求应答，对心跳请求回显。

| 项目 | TCP | WebSocket |
|---|---|---|
| Gateway 配置 | `config/bench.yaml` | `config/bench.yaml` |
| 监听地址 | `127.0.0.1:48080` | `ws://127.0.0.1:48081/` |
| 登录 server ID | `logic-1` | `logic-1` |
| 负载 | 登录后心跳双向回显 | HTTP Upgrade、登录后心跳双向回显 |
| 在途上限 | 每连接 8192 消息 | 每连接 8192 消息 |
| 隔离 | 每轮前重启 gateway 与 logic | 每轮前重启 gateway 与 logic |

命令：

```powershell
go run ./examples/logic_server_min
go run ./cmd/gateway -conf config/bench.yaml

go run ./examples/bench 127.0.0.1:48080 10 5 16 8192 127.0.0.1:8081 logic-1
go run ./examples/ws_bench ws://127.0.0.1:48081/ 10 5
```

TCP 和 WebSocket 不得并发运行；二者共享同一 gateway、logic process、gRPC stream 与 loopback socket，并发运行会使协议对比失效。

### 结果

| Transport | 连接数 | 标称时长 | 接收消息 | 平均接收 QPS | 认证失败 | 客户端丢弃 |
|---|---:|---:|---:|---:|---:|---:|
| TCP | 1 | 2.01 s | 344,220 | 171,577 | 0 | 0 |
| WebSocket | 1 | 2.00 s | 300,999 | 150,402 | 0 | 此工具未统计 |
| TCP | 10 | 5.00 s | 1,870,522 | 374,052 | 1 | 0 |
| WebSocket | 10 | 5.00 s | 1,321,790 | 264,186 | 0 | 此工具未统计 |

最新一轮 TCP 10 连接结果有 4 个客户端认证失败。该结果为保证透明度被保留，但不是零错误容量基准。WebSocket 工具统计写入错误和认证失败，以固定在途上限发送，并报告成功读取的回包数。两种工具均不测量 Pxx 延迟、CPU、内存、NIC 吞吐、丢包、GC pause、TLS/WSS 开销、业务 handler 成本、长稳泄漏或多主机表现。

这些数据仅用于同机 loopback 的协议量级比较，不能作为生产 QPS 承诺，也不能外推到不同主机、网络、payload、并发、logic 实现或业务逻辑。

## 可观测性

默认构建会在顶层 `port`（默认为 `:8081`）启动 HTTP 服务：`/health`、`/live`、`/ready`、`/stats` 和 `/metrics`。`/ready` 在没有 logic stream 时返回 `503`。

## 验证状态

TCP/WebSocket 实现修改后，已执行：

```powershell
go test ./...
go vet ./...
git grep -in udp
```

前两项通过，UDP 搜索没有结果。`go test -tags legacy ./...` 当前要求更新模块，legacy 不在本文档的已验证范围内。

## 已知缺口

- malformed WebSocket frame、fragmentation 与 Upgrade 半包的单元测试仍不完整。
- 默认 gnet 版本不支持 TLS listener，因此当前只能部署 TCP 和明文 WebSocket；需要 WSS 时必须升级或替换网络层。
- `ProtectionConfig` 中仍保留部分 legacy 导向的 WebSocket 心跳字段，默认 codec 不使用这些字段。
- logic stream 重连不会恢复断线期间已经丢弃的消息；需要业务幂等或持久化队列保证语义。
- Group/session 遍历已复制 session 列表后再执行下行写入，不再持有 manager 读锁；大规模 fan-out 仍应先 profiling。
