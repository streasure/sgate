# sgate - 高性能游戏网关

sgate 是一个基于 Go 语言编写的高性能游戏网关，支持 TCP/UDP/WebSocket 多协议接入，通过 gRPC 与逻辑服通信，具备**千万级 QPS** 双向转发能力。

## 架构概览

```
客户端 (TCP/UDP/WS) ──→ sgate Gateway ──(gRPC)──→ Logic Server
         ↑                    │
         └──(gRPC 反向推送)───┘     Nacos (配置中心/服务发现/集群选举)
```

## 核心特性

- **高性能**: 基于 gnet 事件驱动网络框架，写合并（write coalescing）+ 批量刷新，单节点 2.8M+ 双向 QPS
- **多协议支持**: TCP、UDP、WebSocket
- **服务发现**: 基于 Nacos 的自动服务发现与注册，支持 zone 隔离（委托 `util/nacos`）
- **集群部署**: 多节点水平扩展，Leader 选举与自动容灾
- **安全防护**: IP 白名单/黑名单、多维限流、熔断器、WAF、TLS、消息完整性校验
- **监控面板**: `/stats` JSON + `/metrics` Prometheus + `/health` `/ready` `/live` K8s 探针
- **配置热更新**: 限流阈值/白名单/黑名单/过载保护参数无需重启
- **推送模式**: 个人推送、组推送（100 人组 = 127M msg/sec）、全服广播

## 依赖

```
github.com/streasure/util v1.0.1
```

## 快速开始

### 1. 编译

```bash
$env:CGO_ENABLED=0

# Gateway
go build -o gw.exe ./examples/high_concurrency_gateway/

# Logic Server
go build -o logic.exe ./examples/logic_server/

# 压测工具
go build -o bench.exe ./examples/bench/
go build -o push_bench.exe ./examples/push_bench/
```

### 2. 启动

```bash
# 终端 1: 启动 Logic Server
./logic.exe

# 终端 2: 启动 Gateway
./gw.exe
```

### 3. 压测

```bash
# 双向压测（duplex）：100 连接，10 秒，batch=16
./bench.exe 127.0.0.1:48080 100 10 16

# 推送压测
./push_bench.exe 127.0.0.1:48080 100 10 personal 16 5000
./push_bench.exe 127.0.0.1:48080 100 10 group 16 5000
./push_bench.exe 127.0.0.1:48080 100 10 broadcast 16 5000
```

## 压测结果

**环境**: Intel Core i5-10400F (6 核 12 线程, 2.90GHz), Windows, Go 1.22.5

### 双向压测（100 连接, 10s, batchSize=16）

| 指标 | 数值 |
|------|------|
| 正向 QPS (client→sgate→logic) | **2.92M** |
| 推送 QPS (logic→sgate→client) | **2.82M** |
| 正向丢弃 | **0** |
| 结果 | **BIDIRECTIONAL SUCCESS** |

### 推送压测（100 连接, 10s, batchSize=16）

| 模式 | 发送 QPS | 接收 QPS | 有效吞吐 | 说明 |
|------|----------|----------|----------|------|
| Personal push | 1.43M | 1.38M | 1.38M/sec | 每次请求推 1 个客户端 |
| Group push (100人) | 1.32M | 1.27M | **127M/sec** | 每次请求推整个组 |
| Broadcast (100人) | 1.41M | 1.36M | **136M/sec** | 每次请求推所有客户端 |

## 项目接入指南

### 1. 项目接入（go.mod）

```go
module your-project

go 1.22.5

require (
    github.com/streasure/sgate v1.0.0
    github.com/streasure/util v1.0.1
)
```

### 2. Logic Server 接入

```go
package main

import (
    "os"
    "time"
    "github.com/streasure/sgate/logic"
    "github.com/streasure/sgate/protobuf"
)

func main() {
    svc := logic.NewService(
        logic.WithServiceID("logic-1"),
        logic.WithAdvertiseAddr("localhost:50052"),
        logic.WithListenPort("50052"),
        logic.WithNacosEndpoint("http://127.0.0.1:8080"),
        logic.WithNacosNamingEndpoint("http://127.0.0.1:56000"),
        logic.WithNacosNamespace("public"),
        logic.WithNacosGroup("DEFAULT_GROUP"),
        logic.WithNacosAuth("nacos", "nacos"),
        logic.WithNacosAPIVersion("v3"),
        logic.WithServiceName("logic"),
    )

    // 登录注册（必须）：注册 userUUID → connectionID 映射
    svc.RegisterRoute(protobuf.RouteLogin, func(msg *protobuf.Message) *protobuf.Message {
        userUUID := "uuid_" + msg.GetPayload()["userId"]
        svc.RegisterUser(userUUID, msg.ConnectionId)
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        protobuf.RouteLogin,
            Payload:      map[string]string{"code": "200", "userUUID": userUUID},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    // 双向请求-响应（duplex）
    svc.RegisterBurstRoute(protobuf.RouteTest, func(msg *protobuf.Message, push func(*protobuf.Message)) {
        push(&protobuf.Message{
            Route:     protobuf.RouteTestResult,
            Timestamp: time.Now().UnixMilli(),
        })
    })

    // 个人推送（push to self）
    svc.RegisterBurstRoute("push_me", func(msg *protobuf.Message, push func(*protobuf.Message)) {
        push(&protobuf.Message{
            Route: "personal_notification",
            Payload: map[string]string{
                "message": "Hello from server!",
            },
            Timestamp: time.Now().UnixMilli(),
        })
    })

    // 个人推送（push to another user）
    svc.RegisterRoute("send_msg", func(msg *protobuf.Message) *protobuf.Message {
        targetUUID := msg.GetPayload()["targetUUID"]
        svc.Server().PushToServer(&protobuf.Message{
            Route: protobuf.RouteServerSendToUser,
            Payload: map[string]string{
                "userUUID": targetUUID,
                "route":    "direct_message",
                "message":  msg.GetPayload()["message"],
                "from":     msg.UserUuid,
            },
            Timestamp: time.Now().UnixMilli(),
        })
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "send_msg_ack",
            Payload:      map[string]string{"code": "200"},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    // 组管理：加入组
    svc.RegisterRoute("join_group", func(msg *protobuf.Message) *protobuf.Message {
        groupID := msg.GetPayload()["groupID"]
        svc.Server().PushToServer(&protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        protobuf.RouteServerJoinGroup,
            Payload:      map[string]string{"groupID": groupID},
            Timestamp:    time.Now().UnixMilli(),
        })
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "join_group_ack",
            Payload:      map[string]string{"code": "200", "groupID": groupID},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    // 组管理：离开组
    svc.RegisterRoute("leave_group", func(msg *protobuf.Message) *protobuf.Message {
        groupID := msg.GetPayload()["groupID"]
        svc.Server().PushToServer(&protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        protobuf.RouteServerLeaveGroup,
            Payload:      map[string]string{"groupID": groupID},
            Timestamp:    time.Now().UnixMilli(),
        })
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "leave_group_ack",
            Payload:      map[string]string{"code": "200"},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    // 组推送
    svc.RegisterRoute("group_msg", func(msg *protobuf.Message) *protobuf.Message {
        groupID := msg.GetPayload()["groupID"]
        svc.Server().SendToGroup(groupID, &protobuf.Message{
            Route: "group_broadcast",
            Payload: map[string]string{
                "message": msg.GetPayload()["message"],
                "from":    msg.UserUuid,
            },
            Timestamp: time.Now().UnixMilli(),
        })
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "group_msg_ack",
            Payload:      map[string]string{"code": "200"},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    // 全服广播
    svc.RegisterRoute("broadcast_msg", func(msg *protobuf.Message) *protobuf.Message {
        svc.Server().Broadcast(&protobuf.Message{
            Route: "global_broadcast",
            Payload: map[string]string{
                "message": msg.GetPayload()["message"],
                "from":    msg.UserUuid,
            },
            Timestamp: time.Now().UnixMilli(),
        })
        return &protobuf.Message{
            ConnectionId: msg.ConnectionId,
            Route:        "broadcast_msg_ack",
            Payload:      map[string]string{"code": "200"},
            Timestamp:    time.Now().UnixMilli(),
        }
    })

    svc.Run()
}
```

### 3. Logic Server API 参考

```go
// ===== 用户注册（推送前提） =====
svc.RegisterUser(userUUID, connectionID)     // 登录时注册
svc.UnregisterUser(userUUID)                 // 登出时注销

// ===== 个人推送 =====
// 方式1: burst route push 回调（最高效，推荐）
svc.RegisterBurstRoute("route", func(msg *protobuf.Message, push func(*protobuf.Message)) {
    push(&protobuf.Message{Route: "notify", Payload: map[string]string{...}})
})

// 方式2: PushToServer + server.send_to_user
svc.Server().PushToServer(&protobuf.Message{
    Route:   protobuf.RouteServerSendToUser,
    Payload: map[string]string{"userUUID": target, "route": "msg", "data": "..."},
})

// ===== 组管理 =====
svc.Server().PushToServer(&protobuf.Message{
    ConnectionId: connID,
    Route:        protobuf.RouteServerJoinGroup,
    Payload:      map[string]string{"groupID": "room_1"},
})
svc.Server().PushToServer(&protobuf.Message{
    ConnectionId: connID,
    Route:        protobuf.RouteServerLeaveGroup,
    Payload:      map[string]string{"groupID": "room_1"},
})

// ===== 组推送 =====
svc.Server().SendToGroup("room_1", &protobuf.Message{
    Route:   "group_event",
    Payload: map[string]string{"data": "..."},
})

// ===== 全服广播 =====
svc.Server().Broadcast(&protobuf.Message{
    Route:   "announcement",
    Payload: map[string]string{"text": "hello"},
})
```

### 4. Gateway 启动示例

```go
package main

import (
    "os"
    "os/signal"
    "runtime"
    "syscall"

    "github.com/streasure/sgate/gateway"
    "github.com/streasure/sgate/types"
    "github.com/streasure/sgate/internal/config"
    "github.com/streasure/util/component"
    tlog "github.com/streasure/treasure-slog"
)

func main() {
    runtime.GOMAXPROCS(runtime.NumCPU())

    cfg, _ := config.LoadConfig()

    fc := types.NewFilterChain()
    secComp := gateway.NewSecurityComponent(cfg.Security, cfg.WAF, cfg.JWTAuth, fc)
    obsComp := gateway.NewObservabilityComponent(cfg.OTelTracer, ":6060", fc)
    traComp := gateway.NewTrafficComponent(cfg.Canary, cfg.TrafficMirror, cfg.Degradation, fc)
    clsComp := gateway.NewClusterComponent(*cfg, cfg.GRPC.Port, nil)
    trnComp := gateway.NewTransportComponent(nil, cfg.Transports)

    for _, c := range []component.Component{secComp, obsComp, traComp, clsComp} {
        c.Init()
        c.Start()
    }

    gw := gateway.NewGatewayWithDeps(gateway.GatewayDeps{
        Config:             *cfg,
        FilterChain:        fc,
        LogSanitizer:       obsComp.LogSanitizer,
        WhitelistBlacklist: secComp.WhitelistBlacklist,
        WAF:                secComp.WAF,
        RateLimiter:        secComp.RateLimiter,
        JWTAuth:            secComp.JWTAuth,
        CircuitBreakerMgr:  secComp.CircuitBreakerMgr,
        Tracer:             obsComp.Tracer,
        OTelTracer:         obsComp.OTelTracer,
        LatencyTracker:     obsComp.LatencyTracker,
        CanaryFilter:       traComp.CanaryFilter,
        TrafficMirror:      traComp.TrafficMirror,
        Degradation:        traComp.Degradation,
        Discovery:          clsComp.Discovery,
        Balancer:           clsComp.Balancer,
        ConfigCenter:       clsComp.ConfigCenter,
        ClusterNode:        clsComp.Cluster,
        AlertWebhook:       clsComp.AlertWebhook,
    })

    trnComp.SetGateway(gw)
    gw.StartServices()
    trnComp.StartTransports()

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    <-sigCh

    trnComp.Destroy()
    gw.Close()
    for i := len([]component.Component{clsComp, traComp, obsComp, secComp}) - 1; i >= 0; i-- {
        comps := []component.Component{secComp, obsComp, traComp, clsComp}
        comps[i].Destroy()
    }
}
```

### 5. 关键注意事项

| 要点 | 说明 |
|------|------|
| **用户注册** | 登录 handler 中必须调用 `svc.RegisterUser(userUUID, connID)`，否则推送无法路由 |
| **组操作** | `JoinGroup`/`LeaveGroup` 通过 `PushToServer` 发送到 Gateway，异步生效 |
| **个人推送** | 推荐 burst route 的 `push` 回调（最高效路径）；跨用户用 `RouteServerSendToUser` |
| **组推送** | `SendToGroup` 路由到 Gateway 的 `ConnectionManager.SendToGroup`，组成员需先通过 `RouteServerJoinGroup` 加入 |
| **广播** | `Broadcast` 路由到 Gateway 的 `ConnectionManager.Broadcast`，推送所有连接 |
| **PushToServer** | 直接写入 gRPC stream，所有 Gateway 连接收到后按 Route 分发 |

## 项目结构

```
sgate/
├── examples/
│   ├── bench/                         # 压测客户端（duplex 模式）
│   ├── push_bench/                    # 推送压测（personal/group/broadcast）
│   ├── integration/                   # 完整接入示例（所有推送模式）
│   ├── logic_server/                  # 逻辑服示例（完整路由）
│   └── high_concurrency_gateway/      # Gateway 启动入口
│       ├── main.go                    # 主入口 + 信号处理
│       ├── gc_tune.go                 # GC 调优
│       ├── priority_windows.go        # Windows 进程优先级
│       └── config/config.yaml         # 网关配置
├── gateway/                           # Gateway 核心
│   ├── core_gateway.go                # 主逻辑、OnTraffic、转发路径
│   ├── core_grpc.go                   # gRPC 客户端/服务端、消息分发、写合并
│   ├── core_connection.go             # 连接管理、Broadcast、推送组
│   ├── core_stats.go                  # Stats()
│   └── *_component.go                 # 生命周期组件
├── cluster/                           # 集群管理
├── logic/                             # 逻辑服 SDK
│   ├── server.go                      # Server: 推送/组管理/广播
│   └── service.go                     # Service: gRPC + Nacos 注册
├── protobuf/                          # Proto 定义 & 路由常量
└── internal/config/                   # 配置解析
```

## 运行模式

### 单节点模式（压测/开发）

```yaml
# config.yaml
discovery:
  enabled: false          # 关闭 Nacos 发现，直连 localhost:50052
grpc:
  logicAddr: "localhost:50052"
cluster:
  enabled: false
configCenter:
  enabled: false
```

### 生产模式（Nacos 服务发现）

```yaml
# config.yaml
discovery:
  enabled: true
  serviceName: "logic"
grpc:
  logicAddr: ""           # 由 Nacos 发现自动填充
cluster:
  enabled: true
configCenter:
  enabled: true
  endpoint: "http://nacos-server:8080"
  namespace: "production"
```

## 环境变量

Logic Server 支持以下环境变量（优先于默认值）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LOGIC_SERVICE_ID` | `logic-1` | 服务实例 ID |
| `LOGIC_ADVERTISE_ADDR` | `localhost:50052` | 对外暴露地址 |
| `LOGIC_PORT` | `50052` | gRPC 监听端口 |
| `NACOS_ENDPOINT` | `""` | Nacos 控制台地址 |
| `NACOS_NAMESPACE` | `public` | Nacos 命名空间 |
| `GRPC_WINDOW_SIZE` | `67108864` | gRPC 窗口大小 |
| `GRPC_MAX_MSG_SIZE` | `4194304` | gRPC 最大消息大小 |
| `LOGIC_STREAM_CH_SIZE` | `1048576` | 流发送通道大小 |
| `LOGIC_DISPATCH_WORKERS` | `24` | 消费者协程数 |
| `LOGIC_PASSTHROUGH` | `false` | 透传模式 |
