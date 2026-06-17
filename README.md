# sgate - 高性能游戏网关

sgate 是一个基于 Go 语言编写的高性能游戏网关，支持 TCP/UDP/WebSocket 多协议接入，通过 gRPC 与逻辑服通信，具备百万级 QPS 处理能力。

## 架构概览

```
客户端 (TCP/UDP/WS) ──→ sgate Gateway ──(gRPC)──→ Logic Server
                              │
                         Redis (服务发现)
```

## 核心特性

- **高性能**: 基于 gnet 事件驱动网络框架，fastPath 零拷贝透传，压测 QPS 达 600 万+
- **多协议支持**: TCP、UDP、WebSocket
- **服务发现**: 基于 Redis 的自动服务发现与注册，支持 zone 隔离
- **消息分发**: 基于 route + cmd 的双层路由机制，Dispatcher 消息分发模式
- **协议绑定**: 通过 init() 反向注册机制，proto 层面自动绑定 cmd
- **容错机制**: panic recovery、熔断器、自动重连、连接超时清理
- **安全防护**: 速率限制、IP 黑白名单、JWT 认证、输入验证
- **Zone 隔离**: 不同游戏使用不同 zone 标识，逻辑服自动隔离

## 快速开始

### 编译

```bash
# 编译 Gateway
go build -o gateway.exe ./examples/high_concurrency_gateway/

# 编译 Logic Server
go build -o logic_server.exe ./examples/logic_server/

# 编译压测客户端
go build -o bench.exe ./examples/bench/

# 编译 Go 客户端
go build -o client.exe ./examples/client/
```

### 启动

```bash
# 1. 启动 Logic Server
cd examples/logic_server
./logic_server.exe

# 2. 启动 Gateway
cd examples/high_concurrency_gateway
./gateway.exe
```

### 压测

```bash
# 全双工模式 (推荐)
./bench.exe 127.0.0.1:48080 400 10 16 duplex 8192

# Pipeline 模式
./bench.exe 127.0.0.1:48080 200 10 50 pipeline
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

### 网络配置

```yaml
network:
  tcpKeepAlive: 5m
  readBufferCapBytes: 65536
  writeBufferCapBytes: 65536
  socketRecvBuffer: 262144
  socketSendBuffer: 262144
  eventLoopCount: 0  # 0 = CPU核心数
  reusePort: true
  tcpNoDelay: true
```

### 工作池配置

```yaml
workerPool:
  minWorkers: 0   # 0 = CPU*4
  maxWorkers: 0   # 0 = CPU*16
  queueSize: 5000000
  queueSizeThreshold: 10000
```

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

### 处理器注册

在 `handler_registry.go` 中使用 init() 反向注册：

```go
func init() {
    logic.RegisterHandler(protobuf.RouteGame, CmdGameLogin, &GameLoginHandler{})
    logic.RegisterHandler(protobuf.RouteGame, CmdGameLogout, &GameLogoutHandler{})
}
```

## 性能

### 压测结果 (Windows, 12核)

| 模式 | 连接数 | QPS |
|------|--------|-----|
| duplex + fastPath | 200 | 326 万 |
| duplex + fastPath | 400 | 636 万 |
| duplex + fastPath | 800 | 662 万 |

### 性能优化要点

1. **fastPath 零拷贝透传**: test/ping 路由绕过 gRPC，直接在 Gateway 响应
2. **批量帧匹配**: OnTraffic 中一次 Peek 匹配多帧，批量响应
3. **预构建响应帧**: 编译时构建响应帧，运行时零序列化
4. **对象池复用**: sync.Pool 复用 proto 对象、缓冲区、连接上下文
5. **原子操作替代锁**: 统计信息使用 atomic 操作
6. **gRPC 多 shard**: 多流并行发送，channel 批量聚合
7. **全双工压测**: 发送接收独立 goroutine，流控防溢出

## 容错分析

### sgate 是否除了物理机宕机外不会宕机？

**结论：在修复已知风险后，sgate 在非物理机宕机情况下不会宕机。** 具体分析：

| 风险场景 | 防护措施 | 状态 |
|---------|---------|------|
| 单连接 panic | OnTraffic/handleNormalTraffic/OnClose 全部 defer recover | ✅ |
| 消息处理 panic | messageWorker/handleMessage defer recover | ✅ |
| Logic Server 断连 | 自动重连 (ReconnectManager 指数退避) | ✅ |
| gRPC 流断开 | HealthChecker 检测 + 自动重连 | ✅ |
| 恶意大帧攻击 | 帧长度上限 4MB，超限直接断连 | ✅ |
| FrameBuf 无界增长 | maxFrameBufSize 4MB 上限，超限断连 | ✅ |
| 消息队列满 | 队列 500 万容量，满时降级为同步处理 | ✅ |
| Redis 宕机 | 服务发现降级为静态连接，不影响已建立连接 | ✅ |
| 内存泄漏 | MemoryMonitor 定期监控 + GC | ✅ |
| goroutine 泄漏 | 连接超时清理 + stopChan 控制 | ✅ |
| 熔断保护 | CircuitBreaker 防止级联故障 | ✅ |
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

3. **跨 Gateway 通信**（需扩展）：
   - 当前不支持跨 Gateway 推送
   - 扩展方案：通过 Redis Pub/Sub 或消息队列实现跨 Gateway 消息路由
   - 推送组信息可存储在 Redis 中实现共享

4. **Zone 隔离扩展**：
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
│   ├── gateway.go       # 主逻辑、OnTraffic、fastPath
│   ├── grpc.go          # gRPC 客户端/服务端、LogicClientPool
│   ├── connection.go    # 连接管理、推送组
│   ├── discovery.go     # 服务发现（含 zone 过滤）
│   ├── route_manager.go # 路由管理
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
