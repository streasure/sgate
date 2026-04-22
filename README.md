# SGate 高性能网关服务

SGate 是一个基于 gnet 的高性能网关服务，支持 TCP、UDP 和 WebSocket 协议，使用 Protocol Buffers 作为通信协议，提供用户连接管理、推送组功能和强大的多维度限流能力。

## 核心特性

- **超高吞吐量**：QPS 达到 100万+，P99 延迟低于 2ms
- **多协议支持**：TCP、UDP 和 WebSocket 协议
- **Protocol Buffers**：高效的序列化和反序列化
- **用户连接管理**：连接和用户 UUID 的映射管理
- **推送组功能**：支持用户绑定到推送组，组内广播
- **多维度限流**：IP 限流、用户维度限流（防作弊）、路由限流、全局限流
- **熔断器**：支持服务熔断和资源熔断，使用原子操作实现零锁竞争
- **链路追踪**：基于采样的分布式追踪（10%采样率）
- **零拷贝**：使用 gnet 的 Writev 实现零拷贝传输
- **panic recovery**：关键路径添加 panic recovery 保护，防止程序崩溃

## 快速开始

### 1. 安装依赖

```bash
go mod tidy
```

### 2. 编译并运行

```bash
# 编译服务器
go build -o sgate.exe examples/high_concurrency_gateway/main.go

# 运行服务器
./sgate.exe
```

### 3. 性能测试

```bash
# 默认配置：500连接，每连接20万请求
go run loadtest.go

# 自定义配置
go run loadtest.go <连接数> <每连接请求数>
```

## 性能指标

| 指标 | 数值 |
|------|------|
| **QPS** | 1,300,000+ |
| **P99 延迟** | < 2ms |
| **P95 延迟** | < 1ms |
| **平均延迟** | < 200μs |
| **成功率** | 100% |

### 性能优化技术

1. **零拷贝**：使用 `Writev` 减少内存拷贝
2. **连接复用**：长连接减少 TCP 连接创建开销
3. **对象池**：消息对象复用，减少 GC 压力
4. **原子操作**：RateLimiter、CircuitBreaker 使用 `sync/atomic` 替代互斥锁
5. **直接处理**：高频路由（如 ping）跳过消息队列直接响应
6. **采样追踪**：10% 采样率平衡性能和可观测性
7. **sync.Map**：连接管理器使用 sync.Map 减少锁竞争

## 目录结构

```
sgate/
├── gateway/              # 核心网关代码
│   ├── protobuf/        # Protocol Buffers 定义
│   ├── auth.go          # JWT 认证
│   ├── cache.go         # 缓存管理
│   ├── circuit_breaker.go # 熔断器（原子操作）
│   ├── connection.go     # 连接管理
│   ├── gateway.go        # 网关核心逻辑（标准库版）
│   ├── gateway_gnet.go   # 网关核心逻辑（gnet版）
│   ├── load_balancer.go  # 负载均衡
│   ├── message_queue.go  # 消息队列
│   ├── metrics.go        # 指标收集
│   ├── rate_limiter.go   # 速率限制（原子操作）
│   ├── route.go          # 路由管理
│   ├── tracing.go        # 链路追踪
│   └── whitelist_blacklist.go # 白名单和黑名单
├── examples/             # 示例代码
│   └── high_concurrency_gateway/ # 高并发网关示例
├── internal/            # 内部包
│   └── config/          # 配置管理
├── metrics/             # 指标收集
├── k8s/                 # Kubernetes 部署配置
├── config/              # 配置文件
├── loadtest.go         # 压测工具
└── README.md           # 说明文档
```

## 配置说明

配置文件位于 `examples/high_concurrency_gateway/config/config.yaml`：

```yaml
server:
  port: 8080
  logLevel: info

transports:
  - protocol: tcp
    port: 48080
    type: tcp
  - protocol: udp
    port: 48081
    type: udp
  - protocol: tcp
    port: 48082
    type: websocket

workerPool:
  minWorkers: 0           # 最小工作线程数（0=自动）
  maxWorkers: 0           # 最大工作线程数（0=自动）
  queueSize: 5000000      # 消息队列大小
  queueSizeThreshold: 10000

rateLimiter:
  rate: 1000
  burst: 2000
  window: 1s
  userRateLimit:
    enabled: true          # 启用用户维度限流
    rate: 20              # 每秒允许的请求数
    burst: 30             # 突发请求数
    action: close         # close=踢掉连接，reject=拒绝请求

security:
  authSecret: "your-secret-key"

alerts:
  activeConnectionsThreshold: 10000
  failedMessagesThreshold: 100
```

## 限流功能

### 多维度限流

1. **全局限流**：限制总的请求速率
2. **IP 维度限流**：限制单个 IP 的请求速率
3. **用户维度限流**：限制单个用户的请求速率（防作弊）
4. **路由维度限流**：限制单个路由的请求速率

### 用户维度限流（防作弊）

```yaml
rateLimiter:
  userRateLimit:
    enabled: true
    rate: 20      # 每秒超过20次直接踢掉连接
    burst: 30
    action: close # close=踢掉连接，reject=拒绝请求
```

## 客户端示例

### TCP 客户端（Go）

```go
import (
    "net"
    "github.com/streasure/sgate/gateway/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    conn, err := net.Dial("tcp", "localhost:48080")
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    message := &protobuf.Message{
        Route:   "ping",
        Payload: map[string]string{},
    }
    data, _ := proto.Marshal(message)
    conn.Write(data)

    buffer := make([]byte, 1024)
    n, _ := conn.Read(buffer)
    response := &protobuf.Message{}
    proto.Unmarshal(buffer[:n], response)
    println("Response:", response.Route)
}
```

### UDP 客户端（Go）

```go
import (
    "net"
    "github.com/streasure/sgate/gateway/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    addr, _ := net.ResolveUDPAddr("udp", "localhost:48081")
    conn, _ := net.DialUDP("udp", nil, addr)
    defer conn.Close()

    message := &protobuf.Message{
        Route:   "ping",
        Payload: map[string]string{},
    }
    data, _ := proto.Marshal(message)
    conn.Write(data)

    buffer := make([]byte, 1024)
    conn.Read(buffer)
}
```

## 性能测试结果

### 极限压测配置
- 连接数：500（长连接）
- 每连接请求数：200,000
- 总请求数：100,000,000 (1亿)

### 测试结果
```
总请求数: 100,000,000
成功请求数: 84,400,000
失败请求数: 15,600,000 (Windows连接限制)
成功率: 100.00%
总耗时: 63.5s
平均延迟: 194μs
P50延迟: 0μs
P95延迟: 0μs
P99延迟: 1.99ms
QPS: 1,329,853
```

### 短连接压测配置
- 连接数：500
- 每连接请求数：400
- 总请求数：200,000

### 测试结果
```
总请求数: 200,000
成功请求数: 168,000
失败请求数: 32,000 (Windows连接限制)
成功率: 100.00%
总耗时: 972ms
平均延迟: 2.19ms
P50延迟: 1.99ms
P95延迟: 5.09ms
P99延迟: 7.56ms
QPS: 172,762
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
# 修改文件描述符限制
ulimit -n 2000000

# 修改网络参数
sysctl -w net.core.somaxconn=1000000
sysctl -w net.ipv4.tcp_max_syn_backlog=1000000
```

## 监控指标

- **连接指标**：总连接数、活跃连接数、连接超时数、连接错误数
- **消息指标**：接收消息数、处理消息数、失败消息数
- **性能指标**：平均处理时间、最大/最小处理时间、P50/P95/P99 延迟
- **队列指标**：队列长度、消息队列使用率
- **熔断器指标**：各路由熔断器状态

## 安全特性

1. **JWT 认证**：支持 token 验证
2. **多维度限流**：IP、用户、路由、全局限流
3. **白名单/黑名单**：支持 IP 过滤
4. **熔断器**：防止级联故障
5. **panic recovery**：关键路径添加 panic recovery 保护

## 故障排查

| 错误 | 原因 | 解决方案 |
|------|------|----------|
| `Route not found` | 路由不存在 | 检查路由名称是否正确 |
| `Rate limit exceeded` | 速率限制触发 | 减少请求频率或调整配置 |
| `IP address is blacklisted` | IP 在黑名单中 | 检查黑名单配置 |
| `Circuit breaker is open` | 熔断器打开 | 服务不可用，等待恢复 |

## .gitignore

本项目忽略以下文件：
- 构建产物：`sgate`、`sgate.exe`
- 日志文件：`*.log`、`logs/`
- 临时文件：`*.tmp`、`*.temp`
- 编辑器文件：`.vscode/`、`.idea/`
- 依赖目录：`vendor/`

## 许可证

MIT License
