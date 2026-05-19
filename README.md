# SGate - 高性能游戏网关

SGate 是一个基于 **gnet + gRPC** 的高性能游戏网关，支持 TCP/UDP/WebSocket 协议，使用 Protocol Buffers 通信，通过 Multi-Stream Sharding 架构和 Redis 服务发现实现超高吞吐量与高可用。

## 核心特性

| 特性 | 说明 |
|------|------|
| **超高吞吐量** | 超级快速路径 + 零拷贝，8 进程 QPS 达 **1090万+**，成功率 100% |
| **Multi-Stream Sharding** | gRPC stream 按 ConnectionId 哈希分片到 N 个 shard，消除单一 stream 瓶颈 |
| **Redis 服务发现** | 基于 Pub/Sub + Keyspace Notification 的四重保障服务注册/发现/掉线检测 |
| **快速掉线响应** | 主动注销(毫秒级) + Key过期事件(~10s) + 定期扫描(兜底) + gRPC连接检测(秒级) |
| **推送组** | 基于 serverId 的自动分组推送，支持全服推送、组推送、定向推送 |
| **单一登录** | 同一用户重复登录自动踢掉旧连接并通知客户端 |
| **多协议支持** | TCP、UDP、WebSocket |
| **Logic SDK** | 引入 `logic` 包即可快速接入，3 行代码启动一个逻辑服务 |
| **多维度限流** | IP / 用户 / 路由 / 全局限流 |
| **熔断器** | 原子操作实现零锁竞争 |
| **链路追踪** | 基于采样的分布式追踪 |
| **panic recovery** | 关键路径保护 |

## 架构

```
                          ┌──────────────────────────────────────────────────┐
                          │                  SGate Gateway                   │
                          │                                                  │
  Clients ──TCP/UDP/WS──► │  gnet ──► Fast Path ──► Direct Response         │
                          │         │                                        │
                          │         └──► Normal Path ──► Worker Pool         │
                          │                                │                 │
                          │                   ┌────────────▼──────────┐      │
                          │                   │   LogicClientPool     │      │
                          │                   │  ┌────────────────┐   │      │
                          │                   │  │ logic_1 (N shards)│  │      │
                          │                   │  │ logic_2 (N shards)│  │      │
                          │                   │  │ logic_N (N shards)│  │      │
                          │                   │  └────────────────┘   │      │
                          │                   │   RoundRobin 路由     │      │
                          │                   └───────────┬───────────┘      │
                          │                               │                  │
                          │  ServiceDiscovery             │                  │
                          │  ┌──────────────────┐         │                  │
                          │  │  Redis Pub/Sub   │         │                  │
                          │  │  Keyspace Notify │         │                  │
                          │  │  Periodic Scan   │         │                  │
                          │  └──────────────────┘         │                  │
                          └───────────────────────────────┼──────────────────┘
                                                          │ gRPC
                   ┌──────────────────────────────────────┼──────────────────┐
                   │              Logic Services           │                  │
                   │  ┌──────────┐  ┌──────────┐  ┌──────▼──────┐          │
                   │  │ logic_1  │  │ logic_2  │  │  logic_3   │  ...     │
                   │  │ :50052   │  │ :50053   │  │  :50054    │          │
                   │  └──────────┘  └──────────┘  └─────────────┘          │
                   └─────────────────────────────────────────────────────────┘
```

### 超级快速路径

对于 `test` 路由，网关使用超级快速路径绕过 Protobuf 解析和前置检查，直接通过字节模式匹配处理请求：

```
Client Request ──► gnet OnTraffic ──► Peek(零拷贝) ──► 字节模式匹配
                                                       │
                                          ┌────────────┴────────────┐
                                          │ 匹配成功                  │ 匹配失败
                                          ▼                          ▼
                                    预计算批量帧响应            Normal Path
                                    (零内存分配)            (Protobuf解析)
```

核心技术：
- **零拷贝**：使用 gnet Peek+Discard 替代数据拷贝
- **字节模式匹配**：直接匹配 protobuf 编码后的固定字节模式，绕过 Protobuf 解析
- **预计算批量帧**：覆盖 1-128 的常见批量大小，消除内存分配
- **批量 I/O**：合并多个响应帧减少系统调用

### Multi-Stream Sharding

网关与每个 Logic 服务之间建立 N 个 gRPC stream（默认 N = CPU 核心数），客户端连接按 ConnectionId 的 FNV-1a 哈希分配到固定 shard：

```
ConnectionId ──► FNV-1a Hash ──► shard[ hash % N ] ──► gRPC Stream
```

### 服务发现与快速掉线响应

```
┌──────────┐  register/deregister/heartbeat   ┌──────────┐
│  Logic   │ ──────── Redis Pub/Sub ────────► │ Gateway  │  ① 主动注销（毫秒级）
│  服务    │                                    │          │
└──────────┘                                    │          │
      │ 心跳续约(3s)                             │          │
      │ Key TTL(10s)                            │          │
      └──► Redis Key TTL过期 ───────────────► │          │  ② Key过期事件（~10秒）
                                              │          │
                                              │ 定期扫描  │  ③ 定期扫描（兜底，10-20秒）
                                              │          │
                                              │ gRPC断开 │  ④ 连接检测（秒级）
                                              └──────────┘
```

| 保障层级 | 触发条件 | 响应时间 | 机制 |
|---------|---------|---------|------|
| **主动注销** | Logic 优雅退出 | 毫秒级 | `registry.Stop()` → 发布 deregister 事件 → Gateway 立即断开 |
| **Key 过期事件** | Logic 异常崩溃 | ~10秒 | 心跳停止 → Key TTL 过期 → Keyspace Notification → 即时感知 |
| **定期扫描** | 兜底保障 | 10-20秒 | Gateway 定期扫描 Redis key，发现消失则触发掉线 |
| **gRPC 连接检测** | 网络断开 | 秒级 | gRPC stream 断开 → `receiveMessages` 返回错误 → 触发掉线处理 |

## 快速开始

### 前置条件

- Go 1.21+
- Redis 6.0+（服务发现依赖）

### 1. 启动 Redis

```bash
redis-cli ping  # 应返回 PONG
```

### 2. 启动 Logic 服务

```bash
go run ./examples/logic_server
```

Logic 服务默认监听 gRPC 端口 `:50052`，自动向 Redis 注册。

启动多个 Logic 实例：

```bash
# 实例2
$env:LOGIC_PORT='50053'; $env:LOGIC_SERVICE_ID='logic_2'; $env:LOGIC_ADVERTISE_ADDR='localhost:50053'; go run ./examples/logic_server

# 实例3
$env:LOGIC_PORT='50054'; $env:LOGIC_SERVICE_ID='logic_3'; $env:LOGIC_ADVERTISE_ADDR='localhost:50054'; go run ./examples/logic_server
```

### 3. 启动 Gateway

```bash
cd examples/high_concurrency_gateway
go run .
```

Gateway 默认监听：
- TCP `:48080`
- UDP `:48081`
- WebSocket `:48082`

Gateway 启动后自动从 Redis 发现已注册的 Logic 服务并建立连接。

### 4. 运行客户端

```bash
cd examples/client
go run main.go localhost:48080
```

## 快速接入 Logic 服务

`logic/` 包是一个 SDK 库，任何 Go 项目只需引入 `github.com/streasure/sgate/logic` 即可快速接入 SGate 网关。

### 最简接入（3 行代码）

```go
package main

import (
    "github.com/streasure/sgate/logic"
    "github.com/streasure/sgate/protobuf"
)

func main() {
    svc := logic.NewService()

    svc.RegisterRoute("ping", func(msg *protobuf.Message) *protobuf.Message {
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "ping",
            Payload:      map[string]string{"message": "pong"},
        }
    })

    svc.Run()
}
```

### 完整游戏逻辑接入

`examples/quickstart_logic` 展示了包含玩家登录、心跳、移动、聊天、服务器推送的完整示例：

```go
svc := logic.NewService(
    logic.WithServiceID("game_logic_1"),
    logic.WithServiceName("logic"),
    logic.WithListenPort("50052"),
    logic.WithRedisAddr("127.0.0.1:6379"),
    logic.WithHeartbeat(3*time.Second, 10*time.Second),
)

pm := NewPlayerManager(svc.Server())

// 玩家登录
svc.RegisterRoute("player.login", func(msg *protobuf.Message) *protobuf.Message {
    userID := msg.GetPayload()["userID"]
    name   := msg.GetPayload()["name"]
    serverID := msg.GetPayload()["serverID"]

    player := pm.Login(msg.ConnectionId, userID, name, serverID)

    return &protobuf.Message{
        ConnectionId: msg.ConnectionId,
        Route:        "player.login",
        Payload: map[string]string{
            "code": "200", "level": fmt.Sprintf("%d", player.Level),
            "hp": fmt.Sprintf("%d", player.HP), "serverID": player.ServerID,
        },
        Timestamp: time.Now().UnixMilli(),
    }
})

// 玩家心跳
svc.RegisterRoute("player.heartbeat", func(msg *protobuf.Message) *protobuf.Message {
    ok := pm.UpdateHeartbeat(msg.ConnectionId)
    if !ok {
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId, Route: "player.heartbeat",
            Payload: map[string]string{"code": "401", "message": "not logged in"},
            Timestamp: time.Now().UnixMilli(),
        }
    }
    return &protobuf.Message{
        ConnectionId: msg.ConnectionId, Route: "player.heartbeat",
        Payload: map[string]string{
            "code": "200",
            "serverTime": fmt.Sprintf("%d", time.Now().UnixMilli()),
            "onlineCount": fmt.Sprintf("%d", pm.GetOnlineCount()),
        },
        Timestamp: time.Now().UnixMilli(),
    }
})

// 服务器推送（向指定 serverID 的所有玩家推送消息）
svc.RegisterRoute("server.push", func(msg *protobuf.Message) *protobuf.Message {
    serverID := msg.GetPayload()["serverID"]
    message  := msg.GetPayload()["message"]

    sent := svc.Server().PushToGroup("server:"+serverID, &protobuf.Message{
        Route: "server.announcement",
        Payload: map[string]string{"message": message, "from": "system"},
    })

    return &protobuf.Message{
        ConnectionId: msg.ConnectionId, Route: "server.push",
        Payload: map[string]string{"code": "200", "sent": fmt.Sprintf("%d", sent)},
        Timestamp: time.Now().UnixMilli(),
    }
})

svc.Run()
```

运行：

```bash
go run ./examples/quickstart_logic
```

### Logic SDK API

#### Service 生命周期

| 方法 | 说明 |
|------|------|
| `NewService(opts...)` | 创建逻辑服务实例 |
| `svc.RegisterRoute(route, handler)` | 注册路由处理器 |
| `svc.Server()` | 获取底层 Server 实例 |
| `svc.Start()` | 启动服务（非阻塞） |
| `svc.Stop()` | 停止服务 |
| `svc.Run()` | 启动并阻塞等待信号（推荐） |

#### Server 推送能力

| 方法 | 说明 |
|------|------|
| `server.PushToConnection(connID, msg)` | 向指定连接推送消息 |
| `server.PushToGroup(groupID, msg, exclude...)` | 向推送组内所有连接推送消息 |
| `server.PushToServer(msg, exclude...)` | 向当前服务器组推送消息 |
| `server.Broadcast(msg, exclude...)` | 全服广播 |
| `server.JoinGroup(groupID, connID)` | 将连接加入推送组 |
| `server.LeaveGroup(groupID, connID)` | 将连接移出推送组 |
| `server.OnDisconnect(callback)` | 注册连接断开回调 |

#### Option 列表

| Option | 默认值 | 说明 |
|--------|-------|------|
| `WithListenPort(port)` | `50052` | gRPC 监听端口 |
| `WithAdvertiseAddr(addr)` | `localhost:{port}` | 对外广播地址 |
| `WithServiceID(id)` | 空 | 服务唯一标识（为空则禁用服务发现） |
| `WithServiceName(name)` | `logic` | 服务名称（需与 Gateway discovery.serviceName 一致） |
| `WithRedisAddr(addr)` | `127.0.0.1:6379` | Redis 地址 |
| `WithRedisPassword(pwd)` | 空 | Redis 密码 |
| `WithRedisDB(db)` | `10` | Redis 数据库编号 |
| `WithHeartbeat(interval, ttl)` | `3s, 10s` | 心跳间隔与 Key TTL |

## 性能测试结果

测试环境：Windows, 12 CPU cores, gnet poll mode, treasure-slog v1.0.7, 1 Logic 实例

### 超级快速路径（test 路由）

| 测试场景 | 连接数 | 总请求数 | QPS | 成功率 |
|---------|--------|---------|-----|--------|
| 单进程 | 500 | 5,000,000 | **1,910,355** | 100% |
| 4 进程 | 1,600 | 3,200,000 | **6,003,089** | 100% |
| 8 进程 | 1,600 | 8,000,000 | **10,903,091** | 100% |

### 正常路径（ping 路由，经 Protobuf 解析 + Logic 转发，pipeline=1）

| 连接数 | 每连接请求数 | 总请求数 | QPS | 成功率 |
|--------|-------------|---------|-----|--------|
| 100 | 100 | 10,000 | 157,002 | 100% |
| 500 | 200 | 100,000 | 146,490 | 100% |
| 1000 | 100 | 100,000 | 119,513 | 100% |
| 2000 | 50 | 100,000 | 121,680 | 100% |

> Windows 下 gnet 使用 poll 模式，Linux 下使用 epoll 模式性能会显著提升。

### 压测工具

```bash
# 单进程压测（超级快速路径）
go run fastloadtest.go [连接数] [每连接请求数] [pipeline] [地址] [serverID] [路由]
go run fastloadtest.go 500 10000 128 localhost:8083 S1

# 正常路径压测（ping 路由，经 Logic 转发）
go run fastloadtest.go 500 200 1 localhost:8083 S1 ping

# 多进程压测
go run multi_fastloadtest.go [进程数]
go run multi_fastloadtest.go 4
```

## 配置说明

### Gateway 配置

配置文件：`examples/high_concurrency_gateway/config/config.yaml`

```yaml
port: 8080
logLevel: info

redis:
  addr: "127.0.0.1:6379"
  password: ""
  db: 10
  poolSize: 10
  minIdleConns: 5

discovery:
  enabled: true
  serviceName: "logic"
  heartbeatInterval: 3s
  heartbeatTTL: 10s
  deregisterDelay: 5s
  scanInterval: 10s

transports:
  - protocol: tcp
    port: 48080
  - protocol: udp
    port: 48081
  - protocol: tcp
    port: 48082
    type: websocket

workerPool:
  minWorkers: 128
  maxWorkers: 1000
  queueSize: 500000

rateLimiter:
  rate: 500000
  burst: 1000000
  window: 1s
  userRateLimit:
    enabled: true
    rate: 20
    burst: 30
    action: close

security:
  authSecret: "default_secret"
  authRoutes:
    - "getConnections"
    - "broadcast"
```

### Logic 服务环境变量

| 环境变量 | 默认值 | 说明 |
|---------|-------|------|
| `LOGIC_PORT` | `50052` | gRPC 监听端口 |
| `LOGIC_ADVERTISE_ADDR` | `localhost:50052` | 对外广播地址 |
| `LOGIC_SERVICE_ID` | 空 | 服务唯一标识（为空则禁用服务发现） |
| `LOGIC_SERVICE_NAME` | `logic` | 服务名称 |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `REDIS_PASSWORD` | 空 | Redis 密码 |

## 客户端协议

### 长度前缀帧协议

```
┌──────────────────┬─────────────────────┐
│  4 字节大端序长度  │   Protobuf 数据      │
└──────────────────┴─────────────────────┘
```

### 消息结构

```protobuf
message Message {
  string connection_id = 1;
  string user_uuid = 2;
  string route = 3;
  map<string, string> payload = 4;
  int64 timestamp = 5;
  // ... 更多字段见 protobuf/message.proto
}
```

### 握手流程

客户端连接后需先发送 `handshake` 消息进行协议版本协商：

```go
msg := &protobuf.Message{
    Route: "handshake",
    Payload: map[string]string{
        "version":   "2.0.0",
        "timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
        "serverId":  "S1",  // 非空时自动加入 server:S1 推送组
    },
    ProtocolVersion: "2.0.0",
}
```

### 推送组

- 连接建立时，若 `handshake` 中 `serverId` 非空，自动加入 `server:{serverId}` 推送组
- Logic 服务可通过 `PushToGroup` 向指定组推送消息
- Logic 服务可通过 `Broadcast` 向所有连接广播
- 连接断开时自动解绑所有推送组

### Go 客户端示例

```go
conn, _ := net.DialTimeout("tcp", "localhost:48080", 10*time.Second)

msg := &protobuf.Message{
    Route: "test",
    Payload: map[string]string{"data": "hello"},
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
```

## 目录结构

```
sgate/
├── discovery/                      # 共享服务发现包
│   ├── types.go                   # ServiceInfo, ServiceEvent, 常量定义
│   └── registry.go                # ServiceRegistry（服务注册/心跳/注销）
├── gateway/                        # 核心网关代码
│   ├── gateway.go                 # 网关核心 + 超级快速路径 + Worker Pool
│   ├── gateway_gnet.go            # GatewayGnet（旧实现，保留参考）
│   ├── grpc.go                    # gRPC StreamManager + Multi-Shard + LogicClientPool
│   ├── discovery.go               # Redis 服务发现（订阅/Key过期检测/定期扫描）
│   ├── connection.go              # 连接管理 + 推送组 + 单一登录
│   ├── route.go                   # 路由管理
│   ├── auth.go                    # JWT 认证
│   ├── rate_limiter.go            # 速率限制
│   ├── load_balancer.go           # 负载均衡
│   ├── circuit_breaker.go         # 熔断器
│   ├── message_queue.go           # 消息队列
│   ├── metrics.go                 # 指标收集
│   ├── health.go                  # 健康检查
│   ├── heartbeat.go               # 心跳
│   ├── websocket.go               # WebSocket 支持
│   ├── cache.go                   # 缓存管理
│   ├── database.go                # 数据库
│   ├── redis.go                   # Redis 客户端
│   ├── tls.go                     # TLS 支持
│   ├── tracing.go                 # 链路追踪
│   ├── compression.go             # 压缩
│   ├── message.go                 # 消息处理
│   ├── message_ack.go             # 消息确认
│   ├── message_integrity.go       # 消息完整性
│   ├── version_negotiation.go     # 版本协商
│   └── whitelist_blacklist.go     # 白名单/黑名单
├── logic/                          # Logic SDK 库包
│   ├── server.go                  # gRPC Server + 路由分发 + 推送组
│   ├── service.go                 # Service 生命周期管理 + 服务注册
│   └── config.go                  # ServiceConfig + Option 模式
├── protobuf/                       # 共享 Protocol Buffers 定义
│   ├── gateway.proto              # gRPC 服务定义
│   ├── gateway.pb.go
│   ├── gateway_grpc.pb.go
│   ├── message.proto              # 消息定义
│   └── message.pb.go
├── internal/
│   └── config/                    # 配置管理
├── examples/
│   ├── high_concurrency_gateway/  # 高并发网关示例
│   │   ├── main.go
│   │   └── config/config.yaml
│   ├── quickstart_logic/          # 快速接入示例（推荐）
│   │   └── main.go
│   ├── game_logic/                # 完整游戏逻辑示例
│   │   └── main.go
│   ├── logic_server/              # 基础 Logic 服务示例
│   │   └── main.go
│   └── client/                    # 客户端示例
│       └── main.go
├── fastloadtest.go                 # 高性能压测工具
├── multi_fastloadtest.go           # 多进程压测工具
└── k8s/                           # Kubernetes 部署配置
```

## 服务发现详解

### Redis Key 结构

```
sgate:service:logic:{serviceID} → {
  "service_id": "logic_1",
  "service_name": "logic",
  "address": "localhost:50052",
  "weight": 1,
  "metadata": {"port": "50052", "version": "1.0.0"},
  "start_time": 1778295169731
}
```

TTL = `heartbeatTTL`（默认 10s），每次心跳续约。

### Redis Pub/Sub 频道

| 频道 | 用途 |
|------|------|
| `sgate:service:events` | 服务事件（register/deregister/heartbeat） |
| `__keyevent@10__:expired` | Key 过期事件（由 Gateway 自动开启 `notify-keyspace-events Ex`） |

### 向后兼容

当 `discovery.enabled: false` 或 Redis 不可用时，Gateway 自动回退到静态连接模式（`localhost:50052`），确保基本功能可用。

## 部署

### Kubernetes

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

### 多实例部署建议

- 每个 Logic 实例使用不同的 `LOGIC_PORT` 和 `LOGIC_SERVICE_ID`
- `LOGIC_ADVERTISE_ADDR` 设置为其他节点可达的地址（不要用 localhost）
- Redis 建议使用独立 DB（默认 db=10），避免 Key 冲突
- 生产环境建议 Redis 开启持久化，避免重启后服务信息丢失

## 监控指标

- **连接指标**：总连接数、活跃连接数、连接超时数、连接错误数
- **消息指标**：接收消息数、处理消息数、失败消息数
- **QPS 计数器**：服务端内部原子计数器，每 5 秒输出到 `qps_counter.log`
