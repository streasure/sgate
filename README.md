# sgate

sgate 是一个基于 gnet v2 的高性能长连接网关。它承载 TCP 和 WebSocket 客户端连接，将消息通过 gRPC 双向流转发给逻辑服，并将逻辑服的推送消息路由回目标客户端。

核心设计原则：逻辑层 API 保持简洁（只管推送），连接管理、协议编解码、服务发现、流量治理和高吞吐转发集中在网关层处理。

---

## 目录

- [一、整体架构](#一整体架构)
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

**客户端 → 逻辑服**

```text
客户端写入 MessageFrame
  → TCP/WebSocket 解码
  → 消息管道（认证、限流、WAF、过滤器）
  → 按 session 分片进入 gRPC 发送队列
  → 转换为 StreamData
  → 逻辑服接收处理
```

**逻辑服 → 客户端**

```text
逻辑服发送 StreamData
  → sgate gRPC 流接收
  → 按 session_id 查找客户端连接
  → TCP/WebSocket 编码
  → 客户端接收 MessageFrame
```

---

## 二、环境要求

| 依赖 | 版本 | 必需 | 说明 |
| --- | --- | --- | --- |
| Go | ≥ 1.22 | 是 | 编译网关和压测程序 |
| etcd | ≥ 3.5 | 推荐 | 逻辑服服务发现；单体模式可选 |
| Windows / Linux / macOS | — | 是 | 生产建议 Linux |

外部依赖：
- `github.com/streasure/protocol` — 协议定义（MessageFrame、StreamData、PushBatch）
- `github.com/streasure/util` — 公共组件（etcd、日志、熔断等）

---

## 三、快速开始（3 分钟跑通）

### 前提

假设你已安装 Go 和 etcd，且 `etcd` 在 `http://127.0.0.1:2379` 运行。

### 步骤 1：编译

```powershell
cd E:\sgate

# 编译网关
go build -o sgate.exe .\cmd\gateway

# 编译压测程序（可选，用于验证）
go build -o bench\logic1_tcp\logic1_tcp.exe .\bench\logic1_tcp
go build -o bench\bench1_tcp\bench1_tcp.exe .\bench\bench1_tcp
```

### 步骤 2：启动逻辑服（模拟）

```powershell
.\bench\logic1_tcp\logic1_tcp.exe -port 50050 -id logic1-tcp
```

逻辑服启动后会在 etcd 注册 `Logic:default` 服务，网关通过服务发现找到它。

### 步骤 3：启动网关

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
```

看到以下日志表示启动成功：

```
gateway starting... version=1.0.0
config loaded port=8081
gnet 启动 12 个事件循环，监听 tcp://:48080, tcp://:48081
gRPC 服务器已启动 addr=:50051
```

此时网关已就绪，等待客户端连接：
- TCP 端口：`48080`
- WebSocket 端口：`48081`
- gRPC 端口：`50051`（逻辑服连接此端口）

### 步骤 4：压测验证（可选）

```powershell
.\bench\bench1_tcp\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

---

## 四、完整启动流程

### 4.1 命令行参数

```
sgate.exe [选项]

选项：
  -conf string     网关配置文件路径（默认 "config/config.yaml"）
  -config string   日志配置文件路径（默认 "config/log.yaml"）
  -version         显示版本号
```

示例：

```powershell
# 使用默认配置
.\sgate.exe

# 指定配置文件
.\sgate.exe -conf config\config_batch_on.yaml -config config\log.yaml

# 查看版本
.\sgate.exe -version
```

### 4.2 启动顺序

**单体模式**（默认）：

```
1. 启动 etcd（可选，逻辑服发现需要）
2. 启动逻辑服（在 etcd 注册 Logic:{zone} 服务）
3. 启动 sgate 网关
4. 客户端连接网关
```

**集群模式**：

```
1. 启动 etcd
2. 启动逻辑服
3. 启动 sgate 实例 1（-conf config/sgate1.yaml）
4. 启动 sgate 实例 2（-conf config/sgate2.yaml）
5. 客户端连接任一网关实例
```

### 4.3 启动日志说明

正常启动应看到以下关键日志：

| 日志 | 含义 |
| --- | --- |
| `gateway starting...` | 网关进程启动 |
| `config loaded` | 配置文件加载成功 |
| `Launching gnet with N event-loops` | gnet 事件循环启动 |
| `gRPC 服务器已启动` | gRPC 端口就绪，可接受逻辑服连接 |
| `etcd 注册成功` | 网关自身已注册到 etcd（standalone/cluster 均会注册） |
| `etcd 逻辑服务发现已启动` | 逻辑服发现正常 |

### 4.4 停止网关

按 `Ctrl+C` 或发送 `SIGTERM` 信号，网关会优雅关闭所有连接。

批量停止（Windows）：

```powershell
Get-Process -Name sgate -ErrorAction SilentlyContinue | Stop-Process -Force
```

---

## 五、运行模式

sgate 支持两种运行模式，通过 `cluster.mode` 配置切换：

### 5.1 单体模式（默认）

```yaml
cluster:
  enabled: false
  mode: "standalone"
```

**行为：**
- 在 etcd 注册网关自身连接信息（`Gateway:{zone}`），供 loginserver 等服务发现
- 保留：逻辑服发现（etcd）、负载均衡
- 跳过网关间发现（GatewayClientPool 不创建）
- 跳过 Leader 选举

**适用场景：** 单实例部署，不需要网关间协作，但需要向 loginserver 暴露连接地址。

### 5.2 集群模式

```yaml
cluster:
  enabled: true
  mode: "cluster"
```

**行为：**
- 在 etcd 注册网关自身（`Gateway:{zone}`）
- 发现同一 zone 的其他网关实例
- 启用 Leader 选举
- 创建网关间 gRPC 客户端池

**适用场景：** 多实例部署，需要网关间通信。

### 5.3 模式对比

| 特性 | standalone | cluster |
| --- | --- | --- |
| 逻辑服发现 | ✅ | ✅ |
| 负载均衡 | ✅ | ✅ |
| 网关自注册 | ✅ (registerSelf) | ✅ |
| 网关间发现 | ❌ | ✅ |
| Leader 选举 | ❌ | ✅ |
| GatewayClientPool | ❌ | ✅ |

---

## 六、配置详解

配置采用「默认值 + YAML 覆盖」语义。YAML 中未出现的字段保留代码默认值；显式填写的 `false`、`0` 或空字符串会覆盖默认值。

### 6.1 顶层配置

| 字段 | 类型 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `port` | int | `8080` | 网关基础端口。实际客户端端口由 `transports` 指定 |
| `logLevel` | string | `info` | 日志等级：`debug`、`info`、`warn`、`error` |
| `serverId` | string | `gateway-1` | 实例 ID，集群中必须唯一。也可由 `GATEWAY_SERVER_ID` 环境变量覆盖 |
| `serverType` | string | `Gateway` | 服务类型 |
| `zone` | string | `default` | 可用区名称 |
| `logicServerType` | string | `Logic` | 逻辑服在 etcd 中的服务类型 |

### 6.2 `transports` 客户端监听

```yaml
transports:
  - protocol: tcp
    port: 48080
  - protocol: tcp
    port: 48081
    type: websocket
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `protocol` | string | 必须为 `tcp` |
| `port` | int | 监听端口，每个端口唯一 |
| `type` | string | 空 = TCP；`websocket` = WebSocket |

### 6.3 `etcd` 服务注册

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 是否启用 etcd |
| `endpoints` | []string | `http://127.0.0.1:2379` | etcd 地址列表 |
| `servicePrefix` | string | `/services` | 服务注册键前缀 |
| `leaseTTL` | string | `10s` | 租约有效期 |

### 6.4 `discovery` 服务发现

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 是否启用逻辑服发现 |
| `serviceName` | string | `logic` | 逻辑服服务名 |
| `zone` | string | `default` | 优先选择的可用区 |
| `gatewayDiscovery` | bool | `true` | 是否发现其他网关（集群模式下生效） |
| `registerSelf` | bool | `true` | standalone 模式下是否向 etcd 注册网关自身连接信息（供 loginserver 发现） |

### 6.4.1 etcd 注册地址格式

网关注册到 etcd 的地址采用 JSON 格式，包含所有连接信息：

```json
{
  "ip": "192.168.1.100",
  "grpc": 50051,
  "tcp": "192.168.1.100:48080",
  "websocket": "192.168.1.100:48081"
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `ip` | string | 网关出口 IP |
| `grpc` | int | gRPC 端口（逻辑服连接此端口） |
| `tcp` | string | TCP 客户端连接地址（可选） |
| `websocket` | string | WebSocket 客户端连接地址（可选） |

loginserver 可通过 etcd watch `Gateway:{zone}` 前缀获取网关连接地址。

### 6.5 `cluster` 集群配置

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | 是否启用集群功能 |
| `mode` | string | `standalone` | `standalone` 单体模式；`cluster` 集群模式 |
| `nodeID` | string | 自动生成 | 节点 ID |
| `leaderElection` | bool | `false` | 是否启用 Leader 选举 |

### 6.6 `grpc` gRPC 服务

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `port` | int | `50051` | gRPC 监听端口 |
| `windowSize` | int | `16MiB` | HTTP/2 流控窗口 |
| `maxMessageSize` | int | `8MiB` | 单条消息最大字节数 |

### 6.7 `stream` 流分片和队列

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `shardCount` | int | 自动 | gRPC 流分片数，`0` 时按 CPU 数计算 |
| `sendChannelSize` | int | `131072` | 每个分片发送队列容量 |
| `receiveBatchSize` | int | `64` | 接收处理批次大小 |
| `batchPush` | bool | `false` | 是否将推送按连接合并为 PushBatch |

### 6.8 `protection` 连接防护

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `maxFrameSize` | int | `4MiB` | TCP 单帧最大载荷 |
| `maxFrameBufSize` | int | `64KiB` | TCP 帧缓冲区上限（百万连接场景需降低） |
| `maxWSFrameSize` | int | `4MiB` | WebSocket 单帧最大载荷 |
| `maxConnections` | int | `0` | 网关最大总连接数，`0`=不限制。生产环境建议设置 |
| `maxConnectionsPerIP` | int | `0` | 单 IP 最大连接数，`0`=不限制。防止单客户端耗尽连接 |
| `cpuThreshold` | float | `90` | CPU 过载阈值（%） |
| `dropOnOverload` | bool | `true` | 过载时丢弃新消息 |
| `wsHeartbeatTimeout` | int | `60` | WebSocket 心跳超时（秒） |
| `connIdleTimeout` | string | `30s` | 连接空闲超时 |
| `preAuthCommands` | []int | `[1000001]` | 认证前允许的命令 |

### 6.9 `security` 安全组件

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 是否启用安全链 |
| `rateLimit.enabled` | bool | `true` | 令牌桶限流 |
| `rateLimit.maxTokens` | int | `1000000` | 最大令牌数 |
| `circuitBreaker.enabled` | bool | `false` | 熔断器 |

### 6.10 其他可选组件

| 配置段 | 关键字段 | 说明 |
| --- | --- | --- |
| `waf` | `enabled` | Web 应用防火墙 |
| `tls` | `enabled`, `certFile`, `keyFile` | TLS 配置（gnet v2 当前不支持 WSS） |
| `balancer` | `algorithm` | 负载均衡：`roundRobin`、`weighted`、`leastConn`、`consistent` |
| `jwtAuth` | `enabled`, `secret` | JWT 鉴权 |
| `canary` | `enabled`, `percent` | 灰度流量 |
| `trafficMirror` | `enabled`, `targetAddr` | 流量镜像 |
| `otelTracer` | `enabled`, `endpoint` | OpenTelemetry 链路追踪 |
| `configCenter` | `enabled` | 配置中心 |
| `alert` | `enabled`, `webhooks` | 告警 Webhook |
| `degradation` | `enabled`, `rules` | 业务降级 |
| `monitoring` | `pprofAddr` | pprof 地址（如 `:6060`） |

---

## 七、压测指南

### 7.1 压测架构

项目提供两组压测：

| 压测 | 链路 | 测试目标 |
| --- | --- | --- |
| bench1 | 客户端 → sgate → 逻辑服 | 转发能力（上行） |
| bench2 | 逻辑服 → sgate → 客户端 | 推送能力（下行） |

**压测顺序：先 WebSocket，再 TCP。**

### 7.2 编译所有程序

```powershell
cd E:\sgate

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

### 7.3 bench1 WebSocket 压测

打开 3 个终端：

**终端 1 — 网关：**

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
```

**终端 2 — 逻辑服（只接收，不处理）：**

```powershell
.\bench\logic1_ws\logic1_ws.exe -port 50053 -id logic1-ws -config bench\logic1_ws\configs\log.yaml
```

**终端 3 — 压测客户端：**

```powershell
.\bench\bench1_ws\bench1_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100
```

### 7.4 bench1 TCP 压测

```powershell
# 终端 1
.\sgate.exe -conf config\config.yaml -config config\log.yaml

# 终端 2
.\bench\logic1_tcp\logic1_tcp.exe -port 50050 -id logic1-tcp

# 终端 3
.\bench\bench1_tcp\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

### 7.5 bench2 WebSocket 压测

需要分别测试 `batchPush=false` 和 `batchPush=true` 两种模式。

**batchPush 关闭：**

```powershell
# 终端 1：网关
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml

# 终端 2：逻辑服（push-interval=0 不人为限速）
.\bench\logic2_ws\logic2_ws.exe -port 50061 -id logic2-ws -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_ws\configs\logic2_ws_log.yaml

# 终端 3：客户端
.\bench\bench2_ws\bench2_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100 -server-id logic2-ws -config bench\bench2_ws\configs\log.yaml
```

**batchPush 开启：** 只需替换网关配置：

```powershell
.\sgate.exe -conf config\config_batch_on.yaml -config config\log.yaml
```

### 7.6 bench2 TCP 压测

```powershell
# batchPush 关闭
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml
.\bench\logic2_tcp\logic2_tcp.exe -port 50060 -id logic2-tcp -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_tcp\configs\logic2_tcp_log.yaml
.\bench\bench2_tcp\bench2_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100 -server-id logic2-tcp -config bench\bench2_tcp\configs\log.yaml

# batchPush 开启：替换网关配置
.\sgate.exe -conf config\config_batch_on.yaml -config config\log.yaml
```

### 7.7 压测参数说明

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-addr` | — | 网关客户端地址 |
| `-duration` | `10s` | 压测持续时间 |
| `-parallel` | `100` | 并发连接数 |
| `-push-interval` | — | 推送间隔，**必须设为 `0`** 才能测极限吞吐 |
| `-push-size` | `64` | 推送载荷字节数 |
| `-expected-members` | `100` | 逻辑服等待加入组的连接数 |
| `-push-workers` | `12` | 逻辑服推送工作协程数 |

### 7.8 压测注意事项

- 每轮测试前确认 etcd、网关、逻辑服端口无残留进程
- `push-interval` 设为 `1ms` 会限制逻辑服发送速率到 ~3.5K/s，不能用于测量极限吞吐
- 每轮持续时间、并发数、载荷大小、工作协程数应保持一致
- 以 bench 客户端的 `avgReceiveRate` 为主要指标

### 7.9 停止所有压测进程

```powershell
Get-Process -Name sgate,logic1_tcp,logic1_ws,logic2_tcp,logic2_ws,bench1_tcp,bench1_ws,bench2_tcp,bench2_ws -ErrorAction SilentlyContinue | Stop-Process -Force
```

---

## 八、性能数据

测试日期：2026-09-13/14。条件：100 连接、64B 载荷、12 推送协程、10s、`push-interval=0`、96 流分片。

### bench1 转发

| 协议 | 总转发量(10s) | 峰值速率 |
| --- | ---: | ---: |
| **WebSocket** | **941 万** | **920K/s** |
| TCP | 844 万 | 835K/s |

### bench2 推送

| 模式 | TCP | WebSocket |
| --- | ---: | ---: |
| `batchPush=false` | 421K/s | 442K/s |
| `batchPush=true` | **853K/s** | **816K/s** |
| 提升 | +102% | +85% |

详细报告见 [`docs/benchmark-report.md`](docs/benchmark-report.md)。

---

## 九、协议说明

协议定义位于 `github.com/streasure/protocol`（`E:\protocol\gateway\gateway.proto`）。

| 通信方向 | 消息 | 编码方式 |
| --- | --- | --- |
| 客户端 ↔ sgate | `MessageFrame` | TCP：4 字节大端长度前缀；WS：二进制帧 |
| sgate ↔ 逻辑服 | `StreamData` | gRPC 双向流 protobuf |
| sgate → 客户端批量推送 | `MessageFrame(CmdPushBatch)` | Body 嵌套 `PushBatch` |

主要命令号：

| 命令 | 数值 | 说明 |
| --- | ---: | --- |
| `CmdLoginGate` | `1000001` | 客户端登录网关 |
| `CmdLoginGateAck` | `1000002` | 登录应答 |
| `CmdHeartbeatReq` | `1100010` | 心跳请求 |
| `CmdUserOffline` | `1100012` | 用户下线通知 |
| `CmdPushBatch` | `9000002` | 网关批量推送 |

---

## 十、逻辑层 API

逻辑服通过 `logic.Server` 提供的 API 操作客户端连接：

```go
PushToConnection(sessionID, command, data)  // 向指定会话推送
SendToGroup(groupID, command, data)         // 向组内成员推送
Broadcast(command, data)                    // 向所有网关广播
```

批量推送是 sgate 的传输优化，逻辑服不需要感知 `PushBatch`。

---

## 十一、目录结构

```text
cmd/gateway/              网关程序入口
internal/frontend.go      TCP 接入、登录和客户端消息处理
internal/websocket.go     WebSocket 握手、帧解析
internal/backend.go       gRPC 逻辑服连接、流分片、重连、反向推送
internal/pipeline.go      认证、安全、过滤和转发管道
internal/connection.go    客户端连接对象与连接管理器
internal/frame.go         MessageFrame 编解码
internal/config/          配置结构、默认值、校验
internal/security/        白名单、黑名单、限流、熔断、WAF
internal/traffic/         灰度、镜像、降级
internal/cluster/         集群、负载均衡、Leader 选举
internal/obs/             监控、追踪、pprof
internal/codec/           TCP/WebSocket 编解码器
logic/                    逻辑服 SDK
bench/                    bench1、bench2 压测程序
config/                   配置文件
docs/                     设计文档和压测报告
```

---

## 十二、开发指南

### 编译

```powershell
go build -o sgate.exe .\cmd\gateway
```

### 检查代码

```powershell
go vet ./...
```

### 修改协议

协议定义在外部模块 `E:\protocol\gateway\gateway.proto`。修改后需：

1. 在 `E:\protocol` 执行 `generate_proto.bat` 重新生成 Go 代码
2. 回到 `E:\sgate` 执行 `go mod tidy` 更新依赖
3. 重新编译

### 日志配置

日志配置文件 `config/log.yaml`：

```yaml
log:
  level: info          # debug / info / warn / error
  format: json         # json / console
  console:
    enabled: false     # 控制台输出
  file:
    enabled: true
    path: logs/sgate.log
    rotate:
      max_size: 500    # MB
      max_backups: 10
      max_age: 30      # 天
  async:
    enabled: true
    buffer_size: 100000
```

---

## 十三、常见问题

### Q: 启动报 `connect: connection refused`

etcd 未启动。启动 etcd 或在配置中设置 `etcd.enabled: false`（此时无法发现逻辑服）。

### Q: 客户端连接后无响应

确认逻辑服已启动并在 etcd 注册。检查网关日志中是否有 `Logic:default` 服务发现记录。

### Q: 集群模式下报 `client X is closing`

这是网关间连接的瞬态错误，不影响功能。网关会自动重连。

### Q: pprof 端口冲突

多实例时每个实例配置不同的 `monitoring.pprofAddr`：

```yaml
# 实例 1
monitoring:
  pprofAddr: ":6060"

# 实例 2
monitoring:
  pprofAddr: ":6061"
```

### Q: 如何禁用 pprof

```yaml
monitoring:
  pprofAddr: ""
```

### Q: 如何禁用 etcd

```yaml
etcd:
  enabled: false
```

此时网关无法发现逻辑服，但可以作为独立的 TCP/WS 接入层使用。

### Q: batchPush 模式需要客户端做什么

客户端需要处理 `CmdPushBatch`（`9000002`）命令：解析 `MessageFrame.Body` 中的 `PushBatch`，遍历 `items` 逐条处理。

### Q: 如何在生产环境部署

1. 使用 Linux 服务器
2. 启动 etcd 集群（3 节点）
3. 配置 `cluster.mode: "cluster"` 启用多实例
4. 前置 Nginx/HAProxy 做 TLS 终止和负载均衡
5. 配置 `monitoring.pprofAddr` 和 Prometheus 指标
6. 配置告警 Webhook
