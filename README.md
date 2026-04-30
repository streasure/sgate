# SGate 高性能网关服务

SGate 是一个基于 gnet + gRPC 的高性能游戏网关服务，支持 TCP、UDP 和 WebSocket 协议，使用 Protocol Buffers 作为通信协议，通过 Multi-Stream Sharding 架构实现超高吞吐量。

## 核心特性

- **Multi-Stream Sharding**：gRPC stream 按 ConnectionId 哈希分片到 N 个 shard（默认=CPU核心数），消除单一 stream 瓶颈
- **超高吞吐量**：500 连接 QPS 达 175,000+，2000 连接 QPS 达 143,000+，成功率 100%
- **多协议支持**：TCP、UDP 和 WebSocket 协议
- **Protocol Buffers**：高效的序列化和反序列化，支持长度前缀帧协议
- **用户连接管理**：连接和用户 UUID 的映射管理
- **推送组功能**：支持用户绑定到推送组，组内广播
- **多维度限流**：IP 限流、用户维度限流、路由限流、全局限流
- **熔断器**：支持服务熔断和资源熔断，使用原子操作实现零锁竞争
- **Worker Pool 自适应**：渐进式扩缩容，避免 OOM
- **tlog 异步日志**：高性能异步日志，支持文件轮转、控制台双输出
- **链路追踪**：基于采样的分布式追踪
- **零拷贝**：使用 gnet 的 Writev 实现零拷贝传输
- **panic recovery**：关键路径添加 panic recovery 保护

## 架构

```
                    ┌─────────────────────────────────────────┐
                    │           SGate Gateway                  │
                    │                                         │
 Clients ──TCP/UDP/WS──► gnet ──► Worker Pool ──► RouteHandler│
                    │                              │          │
                    │                    ┌─────────▼──────┐   │
                    │                    │ StreamManager   │   │
                    │                    │ ┌─────────────┐ │   │
                    │                    │ │ Shard 0     │ │   │
                    │                    │ │ Shard 1     │ │   │
                    │                    │ │ ...         │ │   │
                    │                    │ │ Shard N-1   │ │   │
                    │                    │ └─────────────┘ │   │
                    │                    └────────┬────────┘   │
                    └────────────────────────────┼────────────┘
                                                 │ gRPC
                    ┌────────────────────────────▼────────────┐
                    │           Logic Server                   │
                    │         (gRPC :50052)                    │
                    └─────────────────────────────────────────┘
```

### Multi-Stream Sharding

网关与逻辑服之间建立 N 个 gRPC stream（默认 N = CPU 核心数），每个客户端连接按 ConnectionId 的 FNV-1a 哈希值分配到固定的 shard，每个 shard 独立收发，互不阻塞。

```
ConnectionId ──► FNV-1a Hash ──► shard[ hash % N ] ──► gRPC Stream
```

## 快速开始

### 1. 启动逻辑服

```bash
cd logic
go build -o logic.exe .
./logic.exe
```

逻辑服默认监听 gRPC 端口 `:50052`。

### 2. 启动网关

```bash
cd examples/high_concurrency_gateway
go build -o gateway.exe .
./gateway.exe
```

网关默认监听：
- TCP `:8083`
- UDP `:8084`
- WebSocket `:8085`

### 3. 性能测试

```bash
# 从项目根目录运行
go build -o loadtest.exe loadtest.go

# 默认：100 连接 × 100 请求
./loadtest.exe

# 自定义：500 连接 × 100 请求
./loadtest.exe 500 100

# 自定义：2000 连接 × 50 请求，指定服务器地址
./loadtest.exe 2000 50 localhost:8083
```

## 性能测试结果

测试环境：Windows, 12 CPU cores, gnet poll mode

| 连接数 | 每连接请求数 | 总请求数 | QPS | 平均延迟 | P50 | P95 | P99 | 成功率 |
|--------|-------------|---------|-----|---------|-----|-----|-----|--------|
| 10 | 100 | 1,000 | 127,559 | 75μs | <1ms | 524μs | 534μs | 100% |
| 100 | 100 | 10,000 | 135,457 | 664μs | 513μs | 2.1ms | 3.4ms | 100% |
| 200 | 100 | 20,000 | 160,969 | 1.1ms | 991μs | 3.6ms | 5.0ms | 100% |
| 500 | 100 | 50,000 | 175,577 | 2.5ms | 2.0ms | 7.2ms | 10.9ms | 100% |
| 1000 | 50 | 50,000 | 155,008 | 5.2ms | 4.5ms | 12.3ms | 17.5ms | 100% |
| 2000 | 50 | 100,000 | 143,340 | 12.1ms | 11.5ms | 21.9ms | 33.3ms | 100% |
| 5000 | 20 | 100,000 | 129,679 | 32.2ms | 29.7ms | 57.3ms | 104.1ms | 100% |

> 注意：Windows 下 gnet 使用 poll 模式（每个连接一个 goroutine），Linux 下使用 epoll 模式性能会显著提升。

### 性能优化技术

1. **Multi-Stream Sharding**：gRPC stream 按 ConnectionId 哈希分片，消除单一 stream 瓶颈
2. **长度前缀帧协议**：`[4字节大端长度][protobuf数据]`，自动兼容裸 protobuf 数据
3. **批量连接建立**：每批 50 连接，间隔 500ms，避免端口耗尽
4. **Worker Pool 渐进式扩缩容**：扩容 `current*0.3+1`（上限10），缩容 `(current-min)*0.2`（上限5）
5. **零拷贝**：使用 `Writev` 减少内存拷贝
6. **对象池**：消息对象复用，减少 GC 压力
7. **原子操作**：RateLimiter、CircuitBreaker 使用 `sync/atomic` 替代互斥锁
8. **sync.Map**：连接管理器使用 sync.Map 减少锁竞争
9. **tlog 异步日志**：异步批量写入，支持文件轮转

## 目录结构

```
sgate/
├── gateway/                    # 核心网关代码
│   ├── protobuf/              # Protocol Buffers 定义（内部）
│   ├── gateway.go             # 网关核心逻辑 + Worker Pool
│   ├── gateway_gnet.go        # gnet 事件处理
│   ├── grpc.go                # gRPC StreamManager + Multi-Shard
│   ├── connection.go          # 连接管理
│   ├── route.go               # 路由管理
│   ├── auth.go                # JWT 认证
│   ├── cache.go               # 缓存管理
│   ├── circuit_breaker.go     # 熔断器
│   ├── rate_limiter.go        # 速率限制
│   ├── load_balancer.go       # 负载均衡
│   ├── message_queue.go       # 消息队列
│   ├── metrics.go             # 指标收集
│   ├── health.go              # 健康检查
│   ├── heartbeat.go           # 心跳
│   ├── websocket.go           # WebSocket 支持
│   ├── whitelist_blacklist.go # 白名单和黑名单
│   └── ...                    # 其他模块
├── logic/                     # 逻辑服示例
│   └── main.go                # gRPC 逻辑服实现
├── protobuf/                  # 共享 Protocol Buffers 定义
│   ├── gateway.proto          # gRPC 服务定义
│   ├── gateway.pb.go          # 生成代码
│   ├── gateway_grpc.pb.go     # 生成代码
│   └── message.proto          # 消息定义
├── examples/
│   └── high_concurrency_gateway/  # 高并发网关示例
│       ├── main.go            # 入口
│       └── config/
│           └── config.yaml    # 网关配置
│   └── client/                # 客户端示例
│       └── main.go            # TCP 客户端
├── internal/
│   └── config/                # 配置管理
├── config/
│   └── tlog.yaml              # 日志配置
├── k8s/                       # Kubernetes 部署配置
├── loadtest.go                # 压测工具
└── README.md
```

## 配置说明

### 网关配置

配置文件位于 `examples/high_concurrency_gateway/config/config.yaml`：

```yaml
port: 8080
logLevel: info

transports:
  - protocol: tcp
    port: 8083
    type: ""
  - protocol: udp
    port: 8084
    type: ""
  - protocol: tcp
    port: 8085
    type: websocket

workerPool:
  minWorkers: 128
  maxWorkers: 1000
  queueSize: 500000
  queueSizeThreshold: 10000

rateLimiter:
  rate: 500000
  burst: 1000000
  window: 1s

security:
  authSecret: "your-secret-key"
  authRoutes:
    - "getConnections"
    - "broadcast"
```

### 日志配置

配置文件位于 `config/tlog.yaml`，支持控制台 + 文件双输出：

```yaml
log:
  level: info
  format: json
  async:
    enabled: true
    buffer_size: 10000
    batch_size: 100
    flush_interval: 100
    workers: 4
  console:
    enabled: true
    format: text
  file:
    enabled: true
    path: ./logs/app.log
    rotate:
      max_size: 100      # MB
      max_backups: 10
      max_age: 30        # days
      compress: true
```

日志文件位置取决于进程工作目录：
- 网关：`examples/high_concurrency_gateway/logs/app.log`
- 逻辑服：`logic/logs/app.log`
- 压测工具：`logs/app.log`

## 客户端示例

完整示例代码见 `examples/client/main.go`。

### TCP 客户端（Go）

网关支持长度前缀帧协议：`[4字节大端序长度][protobuf数据]`，也兼容裸 protobuf 数据。

```go
package main

import (
    "encoding/binary"
    "fmt"
    "net"
    "time"

    "github.com/streasure/sgate/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    conn, err := net.DialTimeout("tcp", "localhost:8083", 10*time.Second)
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    msg := &protobuf.Message{
        Route: "test",
        Payload: map[string]string{
            "data": "hello from client",
        },
    }

    data, _ := proto.Marshal(msg)

    buf := make([]byte, 4+len(data))
    binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
    copy(buf[4:], data)
    conn.Write(buf)

    readBuf := make([]byte, 4096)
    n, _ := conn.Read(readBuf)

    resp := &protobuf.Message{}
    proto.Unmarshal(readBuf[:n], resp)
    fmt.Printf("Response: route=%s payload=%v\n", resp.Route, resp.Payload)
}
```

### 运行客户端示例

```bash
cd examples/client
go run main.go localhost:8083
```

## 限流功能

### 多维度限流

1. **全局限流**：限制总的请求速率
2. **IP 维度限流**：限制单个 IP 的请求速率
3. **用户维度限流**：限制单个用户的请求速率（防作弊）
4. **路由维度限流**：限制单个路由的请求速率

### 用户维度限流配置

```yaml
rateLimiter:
  userRateLimit:
    enabled: true
    rate: 20
    burst: 30
    action: close
```

## 部署说明

### Kubernetes 部署

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/secret.yaml
kubectl apply -f k8s/deployment.yaml
```

### Linux 环境优化

```bash
ulimit -n 2000000
sysctl -w net.core.somaxconn=1000000
sysctl -w net.ipv4.tcp_max_syn_backlog=1000000
```

> Linux 下 gnet 使用 epoll 模式，性能远超 Windows poll 模式，建议生产环境部署在 Linux。

## 监控指标

- **连接指标**：总连接数、活跃连接数、连接超时数、连接错误数
- **消息指标**：接收消息数、处理消息数、失败消息数
- **性能指标**：平均处理时间、最大/最小处理时间、P50/P95/P99 延迟
- **队列指标**：队列长度、消息队列使用率
- **Worker Pool**：当前工作线程数、负载情况
- **Stream Shards**：各 shard 连接状态

## 安全特性

1. **JWT 认证**：支持 token 验证
2. **多维度限流**：IP、用户、路由、全局限流
3. **白名单/黑名单**：支持 IP 过滤
4. **熔断器**：防止级联故障
5. **消息完整性校验**：checksum 验证
6. **panic recovery**：关键路径添加 panic recovery 保护

## 故障排查

| 错误 | 原因 | 解决方案 |
|------|------|----------|
| `Route not found` | 路由不存在 | 检查路由名称是否正确 |
| `Rate limit exceeded` | 速率限制触发 | 减少请求频率或调整配置 |
| `IP address is blacklisted` | IP 在黑名单中 | 检查黑名单配置 |
| `Circuit breaker is open` | 熔断器打开 | 服务不可用，等待恢复 |
| 日志文件未生成 | tlog.yaml 中 file.enabled=false | 修改为 true 并重启服务 |
| 高连接数 OOM | Worker Pool 扩容过快 | 调整 minWorkers/maxWorkers |

## 许可证

MIT License
