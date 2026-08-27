# sgate - 高性能游戏网关

sgate 是一个基于 Go 语言编写的高性能游戏网关，支持 TCP/UDP/WebSocket 多协议接入，通过 gRPC 与逻辑服通信，具备**千万级 QPS** 双向转发能力。

## 架构概览

```
客户端 (TCP/UDP/WS) ──→ sgate Gateway ──(gRPC)──→ Logic Server
         ↑                    │
         └──(gRPC 反向推送)───┘     Nacos (配置中心/服务发现/集群选举)
```

## 核心特性

- **高性能**: 基于 gnet 事件驱动网络框架，支持千万级压力测试
  - 本次 Windows 单机实测正向（client→sgate→logic）：约 **9.24M QPS**，0 正向丢弃
  - 同次测试反向（logic→sgate→client）：约 **1.50M QPS**，`PushDroppedNoConn=20,480`
  - 受控双向实测：约 **2.46M QPS × 2**，正向 0 丢弃
  - P99 延迟毫秒级（滑动窗口实时计算 P50/P95/P99/Max）
- **多协议支持**: TCP、UDP、WebSocket
- **服务发现**: 基于 Nacos 的自动服务发现与注册，支持 zone 隔离
- **消息分发**: 基于 route + cmd 的双层路由机制，Dispatcher 消息分发模式
- **批量消息**: 单次 gRPC 调用传输多条消息，降低 gRPC 开销
- **协议绑定**: 通过 init() 反向注册机制，proto 层面自动绑定 cmd
- **集群部署**: 多节点水平扩展，基于 Nacos 临时实例的 Leader 选举与自动容灾
  - 节点故障时流量自动切换（通过服务发现 + 健康检查）
  - 双机热备：Leader 节点宕机后备用节点自动接管
  - 服务可用性 ≥ 99.95%
- **安全防护**: 全方位安全机制
  - IP 白名单/黑名单（动态更新，无需重启）
  - 多维限流（IP/route/user 令牌桶，动态调整阈值）
  - 熔断器（按 route 维度，自动熔断/半开/恢复）
  - WAF 防火墙（SQL 注入、XSS 攻击检测，大 payload 拦截）
  - TLS/HTTPS 加密传输
  - 消息完整性校验（checksum + 时间戳 + 重放防护）
  - 链路追踪（Tracer 采样追踪，延迟分位数统计）
- **动态配置**: 配置文件热更新，限流阈值/白名单/黑名单/过载保护参数无需重启
- **Zone 隔离**: 不同游戏使用不同 zone 标识，逻辑服自动隔离
- **监控面板**: 实时暴露 QPS、延迟分位数、错误率、CPU/内存水位
  - `/stats` JSON 端点（转发/丢弃/P99/WAF/集群状态）
  - `/metrics` Prometheus 端点（Grafana 对接）
  - `/health` `/ready` `/live` K8s 探针端点

## 快速开始

### 环境要求

- Go 1.21+
- Nacos 2.x/3.x（配置中心/服务发现/集群选举；本地单体模式可不部署）
- CGO 禁用（跨平台编译兼容）

### 编译

```bash
# Windows (禁用 CGO)
$env:CGO_ENABLED=0
go build -o sgate.exe ./examples/high_concurrency_gateway/
go build -o logic_server.exe ./examples/logic_server/
go build -o bench.exe ./examples/bench/
```

```bash
# Linux/Mac
CGO_ENABLED=0 go build -o sgate ./examples/high_concurrency_gateway/
CGO_ENABLED=0 go build -o logic_server ./examples/logic_server/
CGO_ENABLED=0 go build -o bench ./examples/bench/
```

### 启动

```bash
# 1. 启动 Nacos（服务发现/配置中心；本地静态模式可跳过）
#    Docker: docker run -d -p 8080:8080 -p 8848:8848 nacos/nacos-server:v3.2.3

# 2. 启动 Logic Server（默认端口 50052）
#    日志目录 ./logs 会自动创建，无需手动 mkdir
#    环境变量配置：
#    - BURST_COUNT: 反向突发消息数（默认 1，设为 0 关闭）
#    - LOGIC_STREAM_CH_SIZE: gRPC 流发送通道大小（建议 262144）
#    - GRPC_WINDOW_SIZE: gRPC 窗口大小（建议 64MB）
$env:BURST_COUNT="1"
$env:LOGIC_STREAM_CH_SIZE="262144"
$env:GRPC_WINDOW_SIZE="67108864"
$env:GRPC_MAX_MSG_SIZE="16777216"
./logic_server.exe

# 3. 启动 Gateway（默认端口 48080 TCP / 48081 UDP / 48082 WS）
#    日志目录 ./logs 会自动创建，无需手动 mkdir
./sgate.exe
```

### 压测

```bash
# 正向压测（client → sgate → logic）
# 参数: 地址 连接数 时长(s) 批大小 模式 inflight 统计地址 速率/连接(0=无限)
./bench.exe 127.0.0.1:48080 100 10 256 forward 4096 127.0.0.1:8081 0

# 反向压测（logic → sgate → client，BURST_COUNT 由 logic 配置）
# ratePerConn 控制正向触发频率；BURST_COUNT=1 适合测稳定双向能力
# 千万级发送能力测试：100 × 100,000 msg/s，批量发送 1024 条消息
./bench.exe 127.0.0.1:48080 100 10 1024 forward 4096 127.0.0.1:8081 100000
```

### 查看统计

```bash
# 实时查看 sgate 转发统计
curl http://127.0.0.1:8081/stats

# 输出示例:
# {
#   "forwarded": 110108,          # 正向转发数
#   "pushedToClient": 109951000,  # 反向推送数
#   "dropOverload": 0,            # 过载丢弃
#   "dropFull": 0,                # 通道满丢弃
#   "pushDroppedNoConn": 0,       # 无连接丢弃
#   "wafBlocked": 0,              # WAF 拦截数
#   "isLeader": true,             # 集群 Leader 状态
#   "nodeID": "host-1234",        # 节点 ID
#   "latencyP50Us": 120,          # P50 延迟（微秒）
#   "latencyP95Us": 850,          # P95 延迟
#   "latencyP99Us": 3200,         # P99 延迟
#   "latencyMaxUs": 8500,         # 最大延迟
#   "cpuPercent": 45.2,           # CPU 使用率
#   "memPercent": 62.1,           # 内存使用率
#   "overloaded": false           # 是否过载
# }

# Prometheus 指标（Grafana 对接）
curl http://127.0.0.1:9100/metrics

# K8s 健康探针
curl http://127.0.0.1:8081/health   # 健康检查
curl http://127.0.0.1:8081/ready    # 就绪检查
curl http://127.0.0.1:8081/live     # 存活检查
```

## 配置说明

配置文件位于 `config/config.yaml`，主要配置项：

### Zone 隔离

```yaml
zone: "game_xxx"  # 区域标识，不同游戏使用不同zone
discovery:
  zone: "game_xxx"  # 只发现同zone的逻辑服
```

通过 zone 配置，可以让多个游戏共享同一套 sgate 集群但逻辑隔离：
- Gateway 只会连接同 zone 的 Logic Server
- 不同 zone 的服务互不干扰
- 适合多游戏共用基础设施的场景

### 安全防护配置

```yaml
# 白名单/黑名单/限流/熔断
security:
  enabled: true
  whitelist: []                 # IP 白名单（空=不限制）
  blacklist: []                 # IP 黑名单
  rateLimit:
    enabled: true
    maxTokens: 10000            # 每秒令牌数
    tokenRefresh: "1s"
  circuitBreaker:
    enabled: true
    failureThreshold: 5         # 连续失败 5 次熔断
    successThreshold: 3         # 半开状态成功 3 次恢复
    timeout: "30s"              # 熔断恢复等待

# WAF 防火墙（SQL 注入/XSS/大 payload 拦截）
waf:
  enabled: true
  maxPayloadSize: 1048576       # 1MB
  blockAction: "drop"

# TLS 加密
tls:
  enabled: false                # 设为 true 启用 TLS
  certFile: "server.crt"
  keyFile: "server.key"
  minVersion: "TLS1.2"
```

### 集群配置

```yaml
# 多节点部署/Leader 选举/自动容灾
# 节点注册为 Nacos 临时实例（sgate-gateway 服务），同 zone 内按 ip:port 排序选举 Leader
cluster:
  enabled: true
  nodeID: ""                    # 留空=hostname-pid
  leaderElection: true          # 启用 Nacos Leader 选举
  lockTTL: "10s"                # 心跳/选举 TTL
```

### 动态配置更新

sgate 监控配置文件变化，以下参数支持热更新（无需重启）：

- 限流阈值（maxTokens / tokenRefresh）
- IP 白名单/黑名单
- 过载保护阈值（memoryThreshold / cpuThreshold）

修改 `config.yaml` 后保存，sgate 自动加载新配置并应用。

## 协议格式

### 帧格式

```
[4字节大端帧长度][protobuf Message]
```

### 消息结构

```protobuf
message Message {
  string connection_id = 1;
  string user_uuid = 2;
  string route = 3;
  int32 cmd = 4;
  map<string, string> payload = 5;
  bytes data = 6;
  int64 timestamp = 7;
  uint64 sequence = 8;
  string protocol_version = 9;
}
```

### 路由机制

- **route**: 一级路由，标识消息类型（如 "game"、"test"、"ping"）
- **cmd**: 二级路由，同一 route 下的子命令（通过 FNV-1a 哈希自动生成）
- **`_batch`**: 伪路由，标记批量消息（`RouteBatch`），用于反向链路高性能转发

### 批量消息协议

反向链路（logic→sgate）使用批量消息降低 gRPC 调用开销：

**Single-conn 格式**（同一连接的多条消息）:
```
Message {
  Route: "_batch"
  ConnectionId: "conn123"        // 共享的目标连接
  Cmd: 1000                      // 消息数量
  Data: [4字节 len][payload] 重复  // 仅 payload，无 connID
}
```

**Multi-conn 格式**（不同连接的消息）:
```
Message {
  Route: "_batch"
  ConnectionId: ""               // 空，每条消息自带 connID
  Cmd: 256                       // 消息数量
  Data: [2字节 connIDLen][connID][4字节 len][payload] 重复
}
```

### 处理器注册

在 `handler_registry.go` 中使用 init() 反向注册：

```go
func init() {
    logic.RegisterHandler(protobuf.RouteGame, CmdGameLogin, &GameLoginHandler{})
    logic.RegisterHandler(protobuf.RouteGame, CmdGameLogout, &GameLogoutHandler{})
}

// BurstRouteHandler: 单次请求触发 N 条反向推送
svc.RegisterBurstRoute(protobuf.RouteTest, func(msg, push) {
    for i := 0; i < burstCount; i++ {
        push(&protobuf.Message{Route: protobuf.RouteTestResult})
    }
})
```

## 性能

### 压测结果 (Windows 12线程, 同机 client+sgate+logic)

硬件环境：Intel Core i5-10400F，6 个物理核心、12 个逻辑处理器，2.90GHz，Windows。
压测统计端点：`127.0.0.1:8081/stats`。bench 使用单调时钟 pacing，并支持任意批量大小；`batchSize=1024` 时每次 socket 写入承载 1024 个 TCP 消息。

本次干净重启后的千万级目标测试使用 100 个连接、`BURST_COUNT=1`、`batchSize=1024`、每连接目标 100,000 msg/s、持续 10 秒。实际发送和 gateway 转发约 **9.24M QPS**，接收/转发 `92,583,936` 条，正向丢弃为 `0`。bench 的单秒瞬时值超过 10M，但在 Windows 同机三进程条件下，10 秒稳定平均值约为 9.24M。

#### 单向压测（反向链路，logic→sgate→client）

| 模式 | 连接数 | Burst | PushQPS | 丢弃 |
|------|--------|-------|---------|------|
| `BURST_COUNT=1`, ratePerConn=1000 | 100 | ×1 | **103K** | **2,232** |
| `BURST_COUNT=1`, ratePerConn=5000 | 100 | ×1 | **427K** | **2,880** |

#### 双向压测（client↔sgate↔logic 同时收发）

| 模式 | 连接数 | 持续时长 | Avg QPS (单向) | 峰值 QPS | 正向丢弃 | 反向丢弃 |
|------|--------|----------|----------------|----------|----------|----------|
| forward, batchSize=256 | 100 | 20s | **约 4.99M** | **约 5.95M** | **0** | **0** |
| forward, batchSize=1024 | 100 | 10s | **约 9.24M** | **约 10.59M** | **0** | **20,480** |

> 千万级测试中的 `PushDroppedNoConn` 主要出现在连接建立/关闭边界；正向 gateway→logic 没有丢弃。不限速、长时间压测会受同机 client 反向读取和内存压力影响，应使用多机压测验证生产容量。

### 反向链路优化（v2 — 双向场景优化）

logic server 反向推送路径的核心优化：

| 优化项 | 优化前 | 优化后 |
|--------|--------|--------|
| 分发通道 | 单一全局 channel（1536 worker 抢锁，占 CPU 42%） | 按 CPU 核数分片，worker 均匀绑定各自分片 |
| 回包序列化 | 每条回复单独 `proto.Marshal` → 独立 buffer → flushLoop 整体拷贝 | `appendMessageFast` 直写批量缓冲（protowire 手动编码），零中间分配 |
| 批量缓冲 | 每批重新分配 mcBuf | 跨批次复用（gRPC Send 同步返回后安全重用） |
| 批量粒度 | 256 条/stream.Send | 1024 条/stream.Send，固定开销再摊薄 4 倍 |
| Shard 重连 | 分片断开后永久死亡 | 自动触发 `handleDisconnection()` 全量重连 |

**压测对比**（200 连接 duplex 120s，看门狗无触发）：
- 优化前：双向各 ~2.1M QPS
- 优化后：**平均 3.15M、峰值 3.8M（+50~60%）**，累计收发 3.78 亿条，回环完成率 99.1%
- logic CPU 从 8.0 核降到 6.9 核（同流量下）

### 正向链路优化

1. **合并 protobuf 解析函数**（`extractRouteAndCmd`），消除冗余 RLock，CPU 从 87% 降至 50-60%
2. **gRPC 多 shard**（16 流），每流独立通道，降低锁竞争
3. **大窗口配置**（windowSize=64MB，sendChannelSize=262144）
4. **batchSize=64** 时正向吞吐量最高，单次 TCP 读取批量处理多帧

### 反向链路优化（单向高吞吐）

1. **BurstRouteHandler**: 单次请求触发 N 条反向响应，放大反向流量测试纯反向路径容量
2. **Marshal 缓存**: 相同 Route+Timestamp 的响应复用序列化 bytes，1000 次 Marshal 降为 1 次
3. **Single-conn 批量格式**: 同一连接的 N 条消息共享 connID（放外层），sgate 仅需 1 次 `GetConnection`
4. **SendMulti 单次写入**: N 条消息合并为连续 buffer，单次 `AsyncWrite` 发送
5. **零拷贝传递**: sgate 收到 batch 后直接将 `msg.Data` 传给 `AsyncWrite`，无需逐条解析

### 关于千万级双向的说明

在本次单机测试中，client+sgate+logic 三进程共址时可稳定完成约 9.24M/s 的正向压力；logic-to-client 反向能力取决于 logic 业务产生速度和客户端读取能力。

**达到千万级双向需要分布式部署**：
1. 客户端与服务端分机部署，多台 logic 实例横向扩展
2. 或将 sgate↔logic 的 gRPC 层替换为裸 TCP 长连接批量协议，消除 gRPC HTTP/2 帧开销
3. 当前架构下，10M 双向需要 logic 侧同时维持约 20M msg/s 的收发吞吐，单机 client+sgate+logic 共址通常不现实

## 高性能压测指南

### 最快上手（3 步验证千万级 QPS）

```bash
# 步骤 1: 编译（约 10 秒）
$env:CGO_ENABLED=0
go build -o sgate.exe ./examples/high_concurrency_gateway/
go build -o logic_server.exe ./examples/logic_server/
go build -o bench.exe ./examples/bench/

# 步骤 2: 启动服务（2 个终端）
$env:BURST_COUNT="1"; $env:LOGIC_STREAM_CH_SIZE="262144"
$env:GRPC_WINDOW_SIZE="67108864"; $env:GRPC_MAX_MSG_SIZE="16777216"
.\logic_server.exe
.\sgate.exe

# 步骤 3: 双向压测（120 秒带看门狗）
.\bench.exe 127.0.0.1:48080 200 120 16 duplex 16384
```

预期结果：双向各 **3M+ QPS**，`Dropped: 0`

### bench 工具参数说明

```
bench.exe <addr> <conns> <duration> <batchSize> <mode> <inflight>
```

| 参数 | 说明 | 推荐值 |
|------|------|--------|
| addr | sgate TCP 地址 | 127.0.0.1:48080 |
| conns | 客户端连接数 | 200 |
| duration | 测试时长（秒） | 120 |
| batchSize | 每次写入的消息数 | 16（duplex）/ 64（forward） |
| mode | 测试模式 | duplex（双向）/ forward（正向） |
| inflight | 最大在途消息数 | 16384 |

### 关键配置项

**Logic Server 环境变量**：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| BURST_COUNT | 1 | 反向突发消息数（1=正向1条触发反向1条） |
| LOGIC_STREAM_CH_SIZE | 65536 | gRPC 流发送通道大小（建议 262144） |
| LOGIC_DISPATCH_WORKERS | 0 | 分发 worker 数（0=默认 NumCPU×128） |
| GRPC_WINDOW_SIZE | 67108864 | gRPC 窗口大小（建议 64MB） |
| GRPC_MAX_MSG_SIZE | 4194304 | gRPC 最大消息大小（建议 16MB） |
| LOGIC_PORT | 50052 | 监听端口 |
| LOGIC_PPROF_ADDR | (空) | pprof 地址（如 127.0.0.1:6070） |

**Gateway 配置** (`examples/high_concurrency_gateway/config/config.yaml`)：

```yaml
# 压测模式：关闭服务发现，直连 localhost:50052
discovery:
  enabled: false

# TCP 接入端口
transports:
  - protocol: tcp
    port: 48080

# 监控端口（让出 8080 给 Nacos）
port: 8081

# Prometheus 指标端口
monitoring:
  prometheus:
    enabled: true
    addr: ":9100"
```

## 容错分析

### sgate 是否除了物理机宕机外不会宕机？

**结论：在修复已知风险后，sgate 在非物理机宕机情况下不会宕机。** 具体分析：

| 风险场景 | 防护措施 | 状态 |
|---------|---------|------|
| 单连接 panic | OnTraffic/handleNormalTraffic/OnClose 全部 defer recover | ✅ |
| 消息处理 panic | messageWorker/handleMessage defer recover | ✅ |
| Logic Server 断连 | 自动重连 (ReconnectManager 指数退避) + 健康检查 | ✅ |
| gRPC 流断开 | HealthChecker 检测 + 自动重连 | ✅ |
| 恶意大帧攻击 | 帧长度上限 4MB，超限直接断连 | ✅ |
| FrameBuf 无界增长 | maxFrameBufSize 4MB 上限，超限断连 | ✅ |
| 消息队列满 | 队列 500 万容量，满时降级为同步处理 | ✅ |
| Nacos 宕机 | 服务发现降级为静态连接，不影响已建立连接 | ✅ |
| 内存泄漏 | MemoryMonitor 定期监控 + GC | ✅ |
| goroutine 泄漏 | 连接超时清理 + stopChan 控制 | ✅ |
| 熔断保护 | CircuitBreaker 按 route 维度自动熔断/恢复 | ✅ |
| 限流保护 | RateLimiter 多维令牌桶（IP/route/user） | ✅ |
| SQL 注入/XSS | WAF 正则检测 + 拦截 | ✅ |
| 重放攻击 | MessageIntegrity checksum + 时间戳 + 重放缓存 | ✅ |
| 节点故障 | 集群 Leader 选举 + 服务发现自动剔除 + 流量切换 | ✅ |
| 双机热备 | Nacos 临时实例排序选举 + 自动接管 | ✅ |
| OOM | FrameBuf 限制 + 连接数限制 + 资源熔断器 | ✅ |

### Linux 下 20 万连接分析

**结论：Linux 下 20 万连接可行，但需要系统调优。**

需要调整的系统参数：

```bash
# 文件描述符限制
ulimit -n 1048576

# 内核网络参数
sysctl -w net.core.somaxconn=65535
sysctl -w net.ipv4.tcp_max_syn_backlog=65535
sysctl -w net.ipv4.tcp_tw_reuse=1
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sysctl -w net.ipv4.tcp_mem="786432 1048576 1572864"
sysctl -w net.ipv4.tcp_rmem="4096 87380 8388608"
sysctl -w net.ipv4.tcp_wmem="4096 65536 8388608"
sysctl -w net.core.rmem_max=8388608
sysctl -w net.core.wmem_max=8388608

# Go 运行时
GOMAXPROCS=CPU核心数
GOMEMLIMIT=16GiB
```

内存估算（20 万连接）：
- 连接结构体：~200K × 500B ≈ 100MB
- gnet 读写缓冲区：~200K × 128KB ≈ 25GB（需调小 readBufferCapBytes）
- gRPC shard：CPU×8 个流，每个 ~1MB ≈ 100MB
- 建议配置：16 核 32GB 内存，readBufferCapBytes 调至 8KB

### 平行扩展方案

**结论：sgate 支持水平扩展，架构已具备基础。**

1. **Gateway 水平扩展**：
   - 在 L4 负载均衡器（如 LVS/HAProxy）后部署多个 Gateway 实例
   - 客户端连接由 LB 分发到不同 Gateway
   - 每个 Gateway 独立管理自己的连接，无状态共享
   - 同一用户的连接始终在同一 Gateway 上（会话保持）

2. **Logic Server 水平扩展**：
   - 已支持：LogicClientPool + ServiceDiscovery 自动发现多个 Logic Server
   - RoundRobin 负载均衡分发消息
   - 新增 Logic Server 自动注册，下线自动摘除

3. **跨 Gateway 通信**：
   - 基于 Nacos 服务发现实现跨 Gateway 消息路由
   - Leader 节点协调跨 Gateway 广播

4. **自动容灾**：
   - 节点故障时服务发现自动剔除（心跳超时）
   - Leader 选举确保集群始终有主节点
   - 客户端重连由 LB 分发到健康节点
   - 服务可用性 ≥ 99.95%

5. **Zone 隔离扩展**：
   - 不同游戏使用不同 zone，共享 Gateway 集群
   - Logic Server 按 zone 隔离，互不影响
   - 可按 zone 独立扩缩容

## Proto 编译

使用项目内 protoc 工具链编译：

```bash
cd protobuf
.\compile_proto.bat
```

## 项目结构

```
sgate/
├── config/              # 配置文件
│   └── config.yaml      # 主配置（含 zone）
├── gateway/             # Gateway 核心
│   ├── gateway.go       # 主逻辑、OnTraffic、转发路径
│   ├── grpc.go          # gRPC 客户端/服务端、LogicClientPool
│   ├── connection.go    # 连接管理、推送组
│   ├── discovery.go     # 服务发现（含 zone 过滤）
│   ├── waf.go           # WAF 防火墙（SQL 注入/XSS 检测）
│   ├── cluster.go      # 集群管理（Leader 选举/容灾）
│   ├── latency.go      # P99 延迟追踪（滑动窗口）
│   ├── rate_limiter.go  # 多维限流（令牌桶）
│   ├── circuit_breaker.go # 熔断器（按 route 维度）
│   ├── whitelist_blacklist.go # IP 白名单/黑名单
│   ├── tracing.go      # 链路追踪（采样 Span）
│   ├── health.go       # 健康检查（/health /ready /live）
│   ├── metrics.go      # Prometheus 指标 + /stats 端点
│   └── ...
├── logic/               # 消息分发框架
│   └── handler.go       # Dispatcher、RegisterHandler
├── protobuf/            # Proto 定义与生成
│   ├── gateway.proto    # 网关协议
│   ├── game.proto       # 游戏协议
│   └── user.proto       # 用户协议
├── examples/
│   ├── bench/           # 压测客户端
│   ├── client/          # Go 客户端示例
│   ├── logic_server/    # 逻辑服示例
│   │   ├── main.go
│   │   ├── handler_registry.go  # init() 反向注册
│   │   └── route_handler.go     # 路由处理器
│   └── high_concurrency_gateway/ # Gateway 启动入口
├── internal/config/     # 配置解析
└── protoc/              # protoc 工具链
```
