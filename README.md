# sgate

sgate 是一个基于 gnet v2 的高性能长连接网关。它负责承载 TCP 和 WebSocket 客户端连接，将客户端消息通过 gRPC 双向流转发给逻辑服，并将逻辑服的推送消息路由回目标客户端。

项目的核心目标是：保持逻辑层 API 简洁，把连接管理、协议编解码、服务发现、流量治理、可靠重连和高吞吐转发集中在网关层处理。

## 一、整体架构

```text
客户端 TCP/WebSocket
        │
        ▼
      sgate
  gnet 事件循环
  消息管道与安全检查
  连接管理与会话路由
  96 个 gRPC 流分片
        │
        ▼
逻辑服集群（etcd 服务发现）
```

### 1. 客户端到逻辑服

```text
客户端写入 MessageFrame
  → TCP/WebSocket 解码
  → 消息管道
  → 认证、限流、WAF、过滤器和完整性检查
  → 按会话分片进入 gRPC 发送队列
  → 转换为 StreamData
  → 逻辑服接收并处理
```

### 2. 逻辑服到客户端

```text
逻辑服发送 StreamData
  → sgate 接收 gRPC 流消息
  → 按 session_id 查找客户端连接
  → TCP/WebSocket 编码
  → 客户端接收 MessageFrame
```

启用 `stream.batchPush` 后，sgate 会在网关层把同一批次中发送给同一连接的消息合并成一个 `PushBatch`。逻辑服仍然只调用普通推送 API，不需要感知批量协议。

## 二、协议说明

协议定义位于外部模块 `github.com/streasure/protocol`，源文件为 `E:\protocol\gateway\gateway.proto`。

| 通信方向 | 消息 | 编码方式 |
| --- | --- | --- |
| 客户端 ↔ sgate | `MessageFrame` | TCP 使用 4 字节大端长度前缀；WebSocket 使用二进制帧 |
| sgate ↔ 逻辑服 | `StreamData` | gRPC 双向流中的 protobuf 消息 |
| sgate → 客户端批量推送 | `MessageFrame(CmdPushBatch)` | `Body` 中嵌套序列化后的 `PushBatch` |

批量协议定义如下：

```protobuf
message PushItem {
    string session_id = 1;
    int32  cmd        = 2;
    bytes  data       = 3;
    int64  seq_id     = 4;
}

message PushBatch {
    repeated PushItem items = 1;
}
```

主要命令号：

| 命令 | 数值 | 说明 |
| --- | ---: | --- |
| `CmdLoginGate` | `1000001` | 客户端登录网关 |
| `CmdLoginGateAck` | `1000002` | 登录应答 |
| `CmdHeartbeatReq` | `1100010` | 心跳请求 |
| `CmdUserOffline` | `1100012` | 用户下线通知 |
| `CmdPushBatch` | `9000002` | 网关批量推送 |

## 三、逻辑层 API

逻辑服通过 `logic.Server` 提供的 API 操作客户端连接：

```go
PushToConnection(sessionID, command, data)
SendToGroup(groupID, command, data)
Broadcast(command, data)
```

- `PushToConnection`：向指定会话发送一条业务消息。
- `SendToGroup`：向逻辑服维护的组成员发送消息，由逻辑层完成成员遍历。
- `Broadcast`：向所有网关实例发送广播消息。

批量推送属于 sgate 的传输优化，逻辑服不应直接依赖 `PushBatch`。

## 四、目录说明

```text
cmd/gateway/              网关程序入口和运行时调优
internal/frontend.go      TCP 接入、登录和客户端消息处理
internal/websocket.go     WebSocket 握手、帧解析和消息处理
internal/backend.go       gRPC 逻辑服连接、流分片、重连和反向推送
internal/pipeline.go      认证、安全、过滤和转发消息管道
internal/connection.go    客户端连接对象与连接管理器
internal/frame.go         MessageFrame 编解码
internal/config/           网关配置结构、默认值和校验
internal/security/         白名单、黑名单、限流、熔断和 WAF
internal/traffic/          灰度、镜像和降级流量组件
internal/cluster/           网关集群和负载均衡
internal/obs/               监控、追踪、性能剖析和健康检查
internal/codec/             TCP/WebSocket 编解码器
logic/                      逻辑服 SDK 和连接推送管理
bench/                      bench1、bench2 压测程序
config/                     网关示例配置
docs/                       设计说明和压测报告
```

## 五、环境要求

- Go 1.24 或更高版本。
- Windows、Linux 或 macOS；生产环境建议使用 Linux。
- 启用 etcd 服务发现时需要 etcd 3.5 或更高版本。
- 逻辑服和网关必须使用兼容版本的 `github.com/streasure/protocol`。

## 六、编译和启动

### 1. 编译网关

```powershell
go build -o sgate.exe .\cmd\gateway
```

Linux/macOS：

```bash
```

### 2. 启动 etcd

```powershell
E:\etcd-v3.5.18-windows-amd64\etcd.exe
```

### 3. 启动网关

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
```

`-conf` 指定网关 YAML 配置，`-config` 指定日志配置。服务启动后默认监听：

- TCP：`48080`
- WebSocket：`48081`
- gRPC：`50051`

## 七、完整配置列表

配置加载采用“默认值加 YAML 覆盖”语义。YAML 中未出现的字段保留代码默认值；显式填写的 `false`、`0` 或空字符串会覆盖默认值。

### 1. 顶层配置

| YAML 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `port` | 整数 | `8080` | 网关管理或基础监听端口。实际客户端端口由 `transports` 指定。 |
| `logLevel` | 字符串 | `info` | 日志等级，常用 `debug`、`info`、`warn`、`error`。 |
| `serverId` | 字符串 | `gateway-1` | 当前网关实例 ID，集群中必须唯一。也可由环境变量 `GATEWAY_SERVER_ID` 覆盖。 |
| `serverType` | 字符串 | `Gateway` | 当前服务类型。 |
| `zone` | 字符串 | `default` | 可用区或逻辑分区名称。 |
| `logicServerType` | 字符串 | `Logic` | 服务发现中逻辑服的服务类型。 |

### 2. `transports` 网络传输

`transports` 是数组，每一项描述一个客户端监听器。

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `protocol` | 字符串 | `tcp` | 底层协议，目前必须为 `tcp`。 |
| `port` | 整数 | `8080`、`8081` | 监听端口，每个端口必须唯一。 |
| `type` | 字符串 | 空 | 留空表示 TCP；填写 `websocket` 表示在 TCP 监听器上处理 WebSocket。 |

### 3. `etcd` 服务注册与发现

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `enabled` | 布尔值 | `true` | 是否启用 etcd。关闭后无法使用基于 etcd 的逻辑服发现。 |
| `endpoints` | 字符串数组 | `http://127.0.0.1:2379` | etcd 地址列表。 |
| `endpoint` | 字符串 | 空 | 单地址兼容配置。 |
| `username` | 字符串 | 空 | etcd 用户名。 |
| `password` | 字符串 | 空 | etcd 密码。 |
| `dialTimeout` | 字符串 | 空 | etcd 拨号超时时间，例如 `5s`。 |
| `servicePrefix` | 字符串 | `/services` | 服务注册键前缀。 |
| `leaseTTL` | 字符串 | `10s` | 服务租约有效期。 |

### 4. `discovery` 逻辑服和网关发现

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `enabled` | 布尔值 | `true` | 是否启用服务发现。 |
| `serviceName` | 字符串 | `logic` | 逻辑服服务名。 |
| `zone` | 字符串 | `default` | 优先选择的可用区。 |
| `heartbeatInterval` | 时间 | `3s` | 服务心跳间隔。 |
| `heartbeatTTL` | 时间 | `10s` | 心跳租约有效时间。 |
| `deregisterDelay` | 时间 | `5s` | 注销前等待时间。 |
| `scanInterval` | 时间 | `10s` | 服务列表扫描间隔。 |
| `gatewayDiscovery` | 布尔值 | `true` | 是否发现其他网关实例。 |

### 5. `grpc` gRPC 服务

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `port` | 整数 | `50051` | 网关 gRPC 监听端口。 |
| `windowSize` | 整数 | `16MiB` | HTTP/2 流控窗口大小。提高吞吐时可增大，但会增加内存占用。 |
| `maxMessageSize` | 整数 | `8MiB` | 单条 gRPC 消息最大字节数。批量消息必须小于此限制。 |

### 6. `stream` gRPC 流分片和队列

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `shardCount` | 整数 | 自动 | gRPC 流分片数。为 `0` 时由程序按 CPU 数计算。 |
| `sendChannelSize` | 整数 | `131072` | 每个正向流分片发送队列容量。过大将占用更多内存。 |
| `receiveBatchSize` | 整数 | `64` | 接收处理批次大小。 |
| `batchPush` | 布尔值 | `false` | 是否将逻辑服推送按连接合并为 `PushBatch`。开启后需要客户端支持 `CmdPushBatch`。 |
| `queuePolicy.policy` | 字符串 | `drop` | 队列满时策略：`drop`、`block`、`timeout`、`backpressure`。 |
| `queuePolicy.maxSize` | 整数 | `100000` | 重连期间缓存队列最大容量。 |
| `queuePolicy.blockTimeout` | 字符串 | `500ms` | `timeout` 策略最大等待时间。 |
| `queuePolicy.backpressureThreshold` | 小数 | `0.8` | `backpressure` 策略触发的队列填充比例。 |
| `queuePolicy.sendTimeout` | 字符串 | `200ms` | 分片发送通道满时的等待时间。 |

### 7. `protection` 连接和请求防护

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `maxFrameSize` | 整数 | `4MiB` | TCP 单帧最大载荷。 |
| `maxFrameBufSize` | 整数 | `4MiB` | TCP 帧缓冲区最大容量。 |
| `maxWSFrameSize` | 整数 | `4MiB` | WebSocket 单帧最大载荷。 |
| `maxWSBufferSize` | 整数 | `4MiB` | WebSocket 缓冲区最大容量。 |
| `cpuThreshold` | 小数 | `90` | CPU 过载阈值，单位为百分比。 |
| `dropOnOverload` | 布尔值 | `true` | 过载时是否丢弃新消息。 |
| `checkIntervalMs` | 整数 | `200` | 过载检查间隔，单位为毫秒。 |
| `wsHeartbeatTimeout` | 整数 | `60` | WebSocket 心跳超时，单位为秒。 |
| `wsCheckInterval` | 整数 | `30` | WebSocket 健康检查间隔，单位为秒。 |
| `connCheckInterval` | 字符串 | `5m` | 空闲连接扫描间隔。 |
| `connIdleTimeout` | 字符串 | `30s` | 连接无活动后关闭的时间。 |
| `verifyInbound` | 布尔值 | `false` | 是否验证入站消息完整性。 |
| `preAuthCommands` | 整数数组 | `[1000001]` | 完成认证前允许的命令列表。 |
| `loginAuth.mode` | 字符串 | `none` | 登录认证方式：`none`、`hmac`、`delegate`。 |
| `loginAuth.secret` | 字符串 | 空 | HMAC 登录认证密钥。 |
| `loginAuth.headerField` | 字符串 | 空 | 认证信息所在字段。 |

### 8. `security` 安全组件

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `enabled` | 布尔值 | `true` | 是否启用安全链。 |
| `whitelist` | 字符串数组 | 空 | 白名单地址或规则。 |
| `blacklist` | 字符串数组 | 空 | 黑名单地址或规则。 |
| `rateLimit.enabled` | 布尔值 | `true` | 是否启用令牌桶限流。 |
| `rateLimit.maxTokens` | 整数 | `1000000` | 令牌桶最大令牌数。 |
| `rateLimit.tokenRefresh` | 字符串 | `1s` | 令牌补充周期。 |
| `circuitBreaker.enabled` | 布尔值 | `true` | 是否启用熔断。 |
| `circuitBreaker.failureThreshold` | 整数 | `5` | 连续失败多少次后打开熔断。 |
| `circuitBreaker.successThreshold` | 整数 | `3` | 半开状态连续成功多少次后恢复。 |
| `circuitBreaker.timeout` | 字符串 | `30s` | 熔断打开后的恢复等待时间。 |

### 9. 其他可选组件

| 配置段 | 关键字段 | 作用 |
| --- | --- | --- |
| `waf` | `enabled`、`sqlPatterns`、`xssPatterns`、`maxPayloadSize`、`blockAction` | Web 应用防火墙，识别恶意载荷并丢弃或记录。 |
| `tls` | `enabled`、`certFile`、`keyFile`、`minVersion` | TLS 配置。当前 gnet v2 传输校验不支持启用 TLS/WSS。 |
| `cluster` | `enabled`、`nodeID`、`leaderElection`、`lockTTL` | 网关集群和 Leader 选举。 |
| `balancer` | `algorithm`、`failureThreshold`、`recoverInterval` | 逻辑服负载均衡，支持 `roundRobin`、`weighted`、`leastConn`、`consistent`。 |
| `jwtAuth` | `enabled`、`secret`、`issuer`、`headerField`、`skipRoutes` | JWT 鉴权。 |
| `canary` | `enabled`、`percent`、`headers`、`userIDs`、`targetRoute` | 灰度流量选择。 |
| `trafficMirror` | `enabled`、`percent`、`targetAddr`、`queueSize`、`workers` | 异步复制部分流量到镜像地址。 |
| `otelTracer` | `enabled`、`endpoint`、`serviceName`、`sampleRate`、`queueSize`、`workers` | OpenTelemetry/Zipkin 链路追踪。 |
| `configCenter` | `enabled`、`type`、`endpoint`、`dataID`、`group`、`token`、`username`、`password`、`pollInterval` | HTTP 配置中心兼容配置。 |
| `alert` | `enabled`、`webhooks`、`rateLimit` | 告警 Webhook。Webhook 项包含 `name`、`url`、`type`、`secret`。 |
| `degradation` | `enabled`、`rules` | 业务降级。规则包含 `route`、`errorThreshold`、`windowSize`、`fallbackData`、`coolDown`。 |
| `filterChain` | `enabled`、`filters` | SPI 过滤器链。过滤器项包含 `name` 和 `config`。 |
| `monitoring` | `pprofAddr`、`prometheus` | 性能剖析和 Prometheus 指标。Prometheus 包含 `enabled`、`addr`、`path`、`prefix`。 |

## 八、压测

项目提供两组压测：

- `bench1`：`bench1 → sgate → logic1`，测客户端到逻辑服的转发能力。
- `bench2`：`logic2 → sgate → bench2`，测逻辑服推送到客户端的能力。

**优先压测 WebSocket**，再压测 TCP。两者的传输协议不同但吞吐量接近，WebSocket 更贴近实际业务场景。

### 1. 编译压测程序

```powershell
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic1_tcp\logic1_tcp.exe .\bench\logic1_tcp
go build -o bench\logic1_ws\logic1_ws.exe .\bench\logic1_ws
go build -o bench\bench1_tcp\bench1_tcp.exe .\bench\bench1_tcp
go build -o bench\bench1_ws\bench1_ws.exe .\bench\bench1_ws
go build -o bench\logic2_tcp\logic2_tcp.exe .\bench\logic2_tcp
go build -o bench\logic2_ws\logic2_ws.exe .\bench\logic2_ws
go build -o bench\bench2_tcp\bench2_tcp.exe .\bench\bench2_tcp
go build -o bench\bench2_ws\bench2_ws.exe .\bench\bench2_ws
```

### 2. bench1 WebSocket（优先）

终端一启动网关：

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
```

终端二启动只接收并丢弃消息的逻辑服：

```powershell
.\bench\logic1_ws\logic1_ws.exe -port 50053 -id logic1-ws -config bench\logic1_ws\configs\log.yaml
```

终端三启动客户端：

```powershell
.\bench\bench1_ws\bench1_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100
```

### 3. bench1 TCP

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
.\bench\logic1_tcp\logic1_tcp.exe -port 50050 -id logic1-tcp
.\bench\bench1_tcp\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

### 4. bench2 WebSocket（优先）

分别使用 `config_batch_off.yaml` 和 `config_batch_on.yaml` 测量两种模式。

```powershell
# 启动网关，批量关闭
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml

# 启动逻辑服，push-interval=0 表示不人为插入休眠
.\bench\logic2_ws\logic2_ws.exe -port 50061 -id logic2-ws -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_ws\configs\logic2_ws_log.yaml

# 启动客户端
.\bench\bench2_ws\bench2_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100 -server-id logic2-ws -config bench\bench2_ws\configs\log.yaml
```

批量开启时只需要替换网关配置：

```powershell
.\sgate.exe -conf config\config_batch_on.yaml -config config\log.yaml
```

### 5. bench2 TCP

```powershell
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml
.\bench\logic2_tcp\logic2_tcp.exe -port 50060 -id logic2-tcp -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_tcp\configs\logic2_tcp_log.yaml
.\bench\bench2_tcp\bench2_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100 -server-id logic2-tcp -config bench\bench2_tcp\configs\log.yaml
```

### 6. 压测注意事项

- 每轮测试前确认 etcd、网关、逻辑服和端口没有残留进程。
- `bench2` 必须等待 `expected-members` 个连接进入逻辑服组后才开始推送。
- `push-interval` 必须明确设置为 `0` 或指定值。设置为 `1ms` 会主动限制逻辑服发送速率，不能用于测量网关极限吞吐。
- 每轮持续时间、并发连接数、载荷大小、工作协程数和机器负载应保持一致。
- 结果以 bench 客户端的 `avgReceiveRate` 为主要指标，同时记录逻辑服的 `pushed` 和 `rate`。

## 九、已验证结果

测试日期：2026-09-13。测试条件：100 个连接、64 字节推送载荷、12 个推送工作协程、持续 10 秒、`push-interval=0`、96 个流分片。

### bench1 转发

| 协议 | 总转发量(10s) | 峰值速率 |
| --- | ---: | ---: |
| **WebSocket** | **843 万** | **840K/s** |
| TCP | 770 万 | 767K/s |

### bench2 推送

| 模式 | TCP | WebSocket |
| --- | ---: | ---: |
| `batchPush=false` | 421K/s | 442K/s |
| `batchPush=true` | **853K/s** | **816K/s** |
| 提升 | 约 102% | 约 85% |

详细过程和历史数据见 [`docs/benchmark-report.md`](docs/benchmark-report.md)。

## 十、开发和验证

```powershell
```

修改协议定义后，需要在 `E:\protocol` 中执行项目提供的 protobuf 生成脚本，再回到本项目重新编译。

停止本地压测进程：

```powershell
Get-Process -Name sgate,logic1_tcp,logic1_ws,logic2_tcp,logic2_ws,bench1_tcp,bench1_ws,bench2_tcp,bench2_ws -ErrorAction SilentlyContinue | Stop-Process -Force
```

## 十一、设计约束和已知限制

- 逻辑层不负责批量编码，批量优化由 sgate 透明完成。
- 当前客户端接入协议是 TCP 和 WebSocket；配置校验会拒绝 UDP 等其他协议。
- 当前 gnet v2 传输配置不支持启用 TLS/WSS，生产环境应在前置代理完成 TLS 终止，或后续扩展传输层实现。
- `PushBatch` 是新增的客户端协议命令，所有真实客户端都必须在启用批量模式前增加兼容处理。
- 压测结果用于比较同一环境下的相对性能，不应直接当作跨机器的绝对容量承诺。
