# sgate - 高性能游戏网关

sgate 是一个基于 Go 语言编写的高性能游戏网关，支持 TCP/UDP/WebSocket 多协议接入，通过 gRPC 双向流与逻辑服通信，具备**百万级 QPS** 双向转发能力。

## 架构概览

```
客户端 (TCP/UDP/WS)
    │
    │  MessageFrame{cmd, seq_id, body=StreamData}
    ▼
┌─────────────────────────────────────────────────┐
│                  sgate Gateway                   │
│                                                  │
│  OnTraffic ──► decodeClientMessage ──► filter ──►│──► gRPC StreamData ──► Logic Server
│  OnTraffic ◄── marshalClientMessage ◄── push  ◄──│◄── gRPC StreamData ◄── Logic Server
│                                                  │
│  ConnectionManager: session映射/组管理/广播       │
│  OverloadProtector: CPU/内存过载保护             │
│  Security: IP黑名单/限流/熔断/WAF/JWT            │
└─────────────────────────────────────────────────┘
         │
    Nacos (配置中心 / 服务发现 / 集群选举)
```

## 协议设计

### 客户端 ↔ Gateway：MessageFrame

客户端与 Gateway 之间使用 `MessageFrame` 作为线路协议：

```protobuf
message MessageFrame {
    int32 cmd    = 1;   // 指令号（由 enums/cmd.proto 定义或 CmdForRoute 生成）
    int64 seq_id = 2;   // 序列号（请求-响应配对）
    bytes body   = 99;  // 业务载荷（序列化的 StreamData）
}
```

- 固定3个字段， protowire 零拷贝解析
- `body` 内部是 `StreamData` 的序列化字节，Gateway 解析后转发给 Logic

### Gateway ↔ Logic：StreamData (onData 双向流)

Gateway 与 Logic 之间使用 `GatewayStream.onData` 双向 gRPC 流：

```protobuf
service GatewayStream {
    rpc onData(stream StreamData) returns (stream StreamData);
}

message StreamData {
    string session_id = 1;   // 连接会话 ID
    string user_key   = 2;   // 用户唯一标识（登录后填充）
    int32  cmd        = 3;   // 指令号
    int64  seq_id     = 4;   // 序列号
    bytes  data       = 5;   // 业务载荷
    string client_ip  = 6;   // 客户端 IP
    string route      = 7;   // 路由键（内部控制用）
    int64  timestamp  = 8;   // 时间戳
    map<string, string> payload = 9;   // 键值对载荷
    string checksum           = 10;  // 校验和
    string protocol_version   = 11;  // 协议版本
}
```

### 数据流

```
正向（客户端→逻辑服）:
  客户端发送: [4字节长度][MessageFrame{cmd, seq_id, body=StreamData{...}}]
  Gateway解码: ExtractMessageFrame → 提取 cmd/seq_id/body
  Gateway转发: StreamData{session_id, user_key, cmd, seq_id, data=body, client_ip}
  Logic处理: 按 cmd 路由到注册的 handler

反向（逻辑服→客户端）:
  Logic推送: StreamData{session_id, user_key, route, data, ...}
  Gateway编码: marshalClientMessage → [4字节长度][MessageFrame{cmd, seq_id, body}]
  Gateway发送: 按 session_id 查找连接，写入 gnet 事件循环
```

## 核心特性

- **高性能**: 基于 gnet v2 事件驱动网络框架，写合并（write coalescing）+ 批量刷新
- **多协议**: TCP、UDP、WebSocket
- **服务发现**: Nacos 自动注册/发现，支持 zone 隔离
- **集群部署**: 多节点水平扩展，Leader 选举与自动容灾
- **安全防护**: IP 白名单/黑名单、多维限流、熔断器、WAF、TLS、消息完整性校验
- **监控**: `/stats` JSON + `/metrics` Prometheus + `/health` `/ready` `/live` K8s 探针
- **推送模式**: 个人推送、组推送、全服广播

## 指令号定义

```protobuf
// enums/cmd.proto
enum Cmd {
    CMD_UNSPECIFIED       = 0;
    CMD_USER_INFO         = 1;
    CMD_MESSAGE           = 2;
    CMD_ERROR_RESPONSE    = 3;
    CMD_HANDSHAKE         = 11;
    CMD_STREAM_DATA       = 14;

    // 逻辑层 (1,000,000+)
    CMD_LOGIN_REQ         = 1000001;
    CMD_LOGIN_ACK         = 1000002;
    CMD_LOGOUT_REQ        = 1000003;
    CMD_LOGOUT_ACK        = 1000004;
    CMD_PUSH_NOTIFY       = 1000006;
    CMD_KICK_NOTIFY       = 1000009;
}
```

### 路由常量（gateway/routes.go）

| 常量 | 值 | 说明 |
|------|-----|------|
| `RouteHandshake` | `"handshake"` | 握手（Gateway 内部处理） |
| `RouteLogin` | `"login"` | 登录（Gateway 放行 + Logic 处理） |
| `RoutePing` | `"ping"` | 心跳请求 |
| `RoutePong` | `"pong"` | 心跳响应 |
| `RouteBatch` | `"_batch"` | 批量消息封包 |
| `RouteServerKick` | `"server.kick"` | 踢下线 |
| `RouteServerJoinGroup` | `"server.join_group"` | 加入组 |
| `RouteServerLeaveGroup` | `"server.leave_group"` | 离开组 |
| `RouteServerSendToGroup` | `"server.send_to_group"` | 组推送 |
| `RouteServerBroadcast` | `"server.broadcast"` | 全服广播 |
| `RouteServerSendToUser` | `"server.send_to_user"` | 跨用户推送 |

## 依赖

```
github.com/streasure/util v1.0.5
github.com/streasure/protocol (本地 replace)
google.golang.org/grpc v1.64.0
google.golang.org/protobuf v1.33.0
```

## 快速开始

### 1. 编译

```bash
$env:CGO_ENABLED=0

# Gateway
go build -o gw.exe ./cmd/gateway/

# Logic Server
go build -o logic.exe ./examples/logic_server/

# 压测工具
go build -o bench.exe ./examples/bench/
go build -o push_bench.exe ./examples/push_bench/
```

### 2. 启动

```bash
# 终端 1: 启动 Logic Server（监听 :50052）
./logic.exe

# 终端 2: 启动 Gateway（TCP :48080, gRPC :50051, metrics :9100）
./gw.exe
```

### 3. 压测

```bash
# 双向压测: 100 连接, 10 秒, batch=16
./bench.exe 127.0.0.1:48080 100 10 16

# 推送压测
./push_bench.exe 127.0.0.1:48080 100 10 personal 16 8192
./push_bench.exe 127.0.0.1:48080 100 10 group 16 8192
./push_bench.exe 127.0.0.1:48080 100 10 broadcast 16 8192
```

## 压测结果

**环境**: Intel Core i5-10400F (6 核 12 线程, 2.90GHz), Windows, Go 1.22.5

### 双向压测（100 连接, 10s, batchSize=16）

| 指标 | 数值 |
|------|------|
| 正向 QPS (client→gateway→logic) | **2.25M** |
| 推送 QPS (logic→gateway→client) | **2.17M** |
| 正向丢弃 | **0** |
| 推送丢弃 | **0** |
| 结果 | **BIDIRECTIONAL SUCCESS** |

### 推送压测（100 连接, 10s, batchSize=16）

| 模式 | 发送 QPS | 接收 QPS | 有效吞吐 | 说明 |
|------|----------|----------|----------|------|
| Personal push | 1.17M | 1.08M | 1.08M/sec | 每次请求推 1 个客户端 |
| Group push (100人) | 280K | 198K | **19.8M/sec** | 每次请求推整个组 |
| Broadcast (100人) | 386K | 366K | **36.6M/sec** | 每次请求推所有客户端 |

## 实现原理

### 1. 网络层：gnet 事件驱动

Gateway 使用 gnet v2 作为网络框架，所有 TCP 连接运行在单个事件循环中：

```
OnTraffic(conn, inBuf)
  ├── 解析 [4字节长度][MessageFrame] 帧
  ├── 首帧拦截: handshake/login 个别处理
  ├── 批量收集: 多帧打包为 RouteBatch
  ├── 认证守卫: 未认证连接拒绝转发
  ├── 过滤器链: IP黑名单 → 限流 → WAF → 熔断 → 完整性校验
  └── 转发: logicClient.SendMessage(batchMsg)
```

**关键优化**：
- 写合并（write coalescing）：多条推送消息合并为单次系统调用
- 零拷贝帧解析：`ExtractMessageFrame` 使用 protowire 直接扫描字节，不反序列化整个 MessageFrame
- 批量转发：多条客户端消息打包为单个 `RouteBatch`，减少 gRPC 调用次数

### 2. 协议层：MessageFrame + StreamData

```
客户端发送 (wire bytes):
  [00 00 00 1B] [MessageFrame{cmd=1000001, seq_id=1, body=LoginReq{...}}]
   ↑ 4字节长度     ↑ protobuf 序列化

Gateway 解码:
  ExtractMessageFrame(data) → (cmd=1000001, seqID=1, body=LoginReq_bytes)
  → StreamData{Route: RouteForCmd(cmd), Cmd: cmd, SeqId: seqID, Data: body}

Gateway 转发 (gRPC stream):
  StreamData{
    SessionId: "conn_abc123",
    UserKey:   "uuid_12345",      // 登录后填充
    Cmd:       1000001,
    SeqId:     1,
    Data:      LoginReq_bytes,
    ClientIp:  "192.168.1.100",
  }

Logic 处理:
  按 cmd 查找 cmdRoutes → 调用注册的 handler → 返回 StreamData

Gateway 编码回包:
  marshalClientMessage(response) → [4字节长度][MessageFrame{cmd, seq_id, body}]
```

### 3. 登录流程

```
客户端                    Gateway                     Logic
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=1, body=         │                           │
  │    Handshake{...}}      │                           │
  │                         │── 版本协商 ──►            │
  │◄── HandshakeResponse ──│                            │
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=2, body=         │                           │
  │    StreamData{          │                           │
  │      Route:"login",     │── StreamData ────────────►│
  │      Payload:{userId}}  │                           │── 注册 user→conn 映射
  │                         │                           │── 返回 LoginAck
  │                         │◄── StreamData ────────────│
  │◄── MessageFrame ───────│  {UserKey:"uuid_xxx"}     │
  │   {cmd=2, body=         │                           │
  │    LoginAck{code:200}}  │                           │
```

### 4. 心跳流程

```
客户端                    Gateway                     Logic
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=CmdForRoute(     │── StreamData ────────────►│
  │    "ping"), body=        │                           │── 返回 Pong
  │    StreamData{           │◄── StreamData ────────────│
  │      Route:"ping"}}     │                           │
  │◄── MessageFrame ───────│  {Route:"pong",            │
  │   {cmd=respCmd, body=   │   Timestamp:...}          │
  │    StreamData{           │                           │
  │      Route:"pong"}}     │                           │
```

### 5. 推送模式

#### 个人推送（Burst Route）
```
Logic 调用 push 回调:
  push(&StreamData{Route:"notify", Payload:{...}})
  → flushLoop 序列化 → stream.Send()
  → Gateway 收到 → 按 SessionId 查找连接 → Writev 合并写入
```

#### 组推送
```
Logic 调用 SendToGroup:
  PushToServer(&StreamData{Route:"server.send_to_group", Payload:{groupID:"xxx"}})
  → Gateway ConnectionManager.SendToGroup()
  → 遍历组内所有连接 → 逐条 Writev
```

#### 全服广播
```
Logic 调用 Broadcast:
  PushToServer(&StreamData{Route:"server.broadcast"})
  → Gateway ConnectionManager.Broadcast()
  → 遍历所有活跃连接 → 逐条 Writev
```

### 6. 性能优化

| 优化项 | 实现 |
|--------|------|
| **批量转发** | 多帧合并为 RouteBatch，单次 gRPC 发送 |
| **写合并** | gnet Writev 多缓冲区单次系统调用 |
| **零拷贝解析** | ExtractMessageFrame 直接扫描 protowire 字节 |
| **连接池复用** | StreamManager 多分片 gRPC 流，消除锁竞争 |
| **序列化优化** | appendMessageFast 手写 protowire 编码，跳过反射 |
| **对象池** | sync.Pool 复用 StreamData、FrameBuf、序列化缓冲区 |
| **内存池** | FrameBuf 按需分配，避免固定大缓冲区浪费 |

## 项目结构

```
sgate/
├── cmd/gateway/              # Gateway 主入口
├── examples/
│   ├── bench/                # 双向压测客户端
│   ├── push_bench/           # 推送压测（personal/group/broadcast）
│   ├── logic_server/         # 逻辑服示例（完整路由注册）
│   └── integration/          # 完整接入示例
├── internal/
│   ├── frontend.go           # TCP/WS 流量处理、帧解析、认证、过滤、转发
│   ├── backend.go            # LogicClient、StreamManager、gRPC 流管理
│   ├── session.go            # 连接 FSM、组管理、广播、推送
│   ├── frame.go              # MessageFrame 编解码
│   ├── filter.go             # 过滤器链
│   ├── integrity.go          # 消息完整性校验
│   ├── negotiation.go        # 版本协商
│   ├── overload.go           # 过载保护
│   ├── stats.go              # 统计数据
│   ├── config/               # 配置解析
│   ├── obs/                  # 可观测性（pprof/prometheus/tracing）
│   ├── security/             # 安全组件（IP/限流/熔断/WAF/JWT）
│   ├── traffic/              # 流量组件（灰度/镜像/降级）
│   └── cluster/              # 集群组件（Nacos/告警）
├── logic/                    # Logic Server SDK
│   ├── server.go             # 推送/组管理/广播
│   ├── handler.go            # RouteHandler/BurstRouteHandler/Dispatcher
│   └── service.go            # gRPC 服务 + Nacos 注册
├── api/                      # 导出路由常量
├── types/                    # FilterContext 等公共类型
└── config/                   # 配置文件 & Grafana/Prometheus

protocol/                     # 协议定义（独立仓库）
├── gateway/
│   ├── gateway.proto         # MessageFrame + Login/Heartbeat + StreamData + GatewayStream
│   └── routes.go             # 路由常量 + CmdForRoute + ExtractMessageFrame
├── commonstruct/             # Message + ErrorResponse + Handshake + Acknowledgement
├── enums/                    # CMD 枚举号 + PushType/CompressionType
└── logic/                    # 前后端交互协议（LoginReq/Ack, PushNotify 等）
```

## 配置

```yaml
# config/config.yaml
port: 8081                    # HTTP 管理端口
logLevel: info
zone: "default"

transports:
  - protocol: tcp
    port: 48080               # TCP 监听端口
  - protocol: websocket
    port: 48082               # WebSocket 监听端口

grpc:
  port: 50051                 # gRPC 服务端口
  logicAddr: "localhost:50052" # Logic Server 地址
  windowSize: 67108864        # gRPC 窗口大小

monitoring:
  pprofAddr: ":6060"          # pprof 地址
  prometheus:
    enabled: true
    addr: ":9100"
    path: "/metrics"
    prefix: "app"

configCenter:
  enabled: true
  type: "nacos"
  endpoint: "http://127.0.0.1:8080"
  namingEndpoint: "http://127.0.0.1:56000"

cluster:
  enabled: true
```

## Logic Server 接入示例

```go
package main

import (
    "time"
    "github.com/streasure/sgate/logic"
    "github.com/streasure/protocol/gateway"
)

func main() {
    svc := logic.NewService(
        logic.WithServiceID("logic-1"),
        logic.WithAdvertiseAddr("localhost:50052"),
        logic.WithListenPort("50052"),
    )

    // 登录（必须）：注册 userUUID → connectionID 映射
    svc.RegisterRoute(gateway.RouteLogin, func(msg *gateway.StreamData) *gateway.StreamData {
        userID := msg.GetPayload()["userId"]
        userUUID := "uuid_" + userID
        svc.RegisterUser(userUUID, msg.SessionId)
        return &gateway.StreamData{
            SessionId: msg.SessionId,
            UserKey:   userUUID,
            Route:     gateway.RouteLogin,
            Payload:   map[string]string{"code": "200", "userId": userID},
            Timestamp: time.Now().UnixMilli(),
        }
    })

    // 心跳
    svc.RegisterRoute(gateway.RoutePing, func(msg *gateway.StreamData) *gateway.StreamData {
        return &gateway.StreamData{
            SessionId: msg.SessionId,
            Route:     gateway.RoutePong,
            Payload:   map[string]string{"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)},
            Timestamp: time.Now().UnixMilli(),
        }
    })

    // 个人推送（burst route，最高效）
    svc.RegisterBurstRoute("push_me", func(msg *gateway.StreamData, push func(*gateway.StreamData)) {
        push(&gateway.StreamData{
            Route:     "personal_notification",
            Payload:   map[string]string{"message": "Hello!"},
            Timestamp: time.Now().UnixMilli(),
        })
    })

    // 组推送
    svc.RegisterRoute("group_msg", func(msg *gateway.StreamData) *gateway.StreamData {
        groupID := msg.GetPayload()["groupID"]
        svc.Server().SendToGroup(groupID, &gateway.StreamData{
            Route:     "group_broadcast",
            Payload:   map[string]string{"message": msg.GetPayload()["message"]},
            Timestamp: time.Now().UnixMilli(),
        })
        return &gateway.StreamData{
            SessionId: msg.SessionId,
            Route:     "group_msg_ack",
            Payload:   map[string]string{"code": "200"},
            Timestamp: time.Now().UnixMilli(),
        }
    })

    // 全服广播
    svc.RegisterRoute("broadcast_msg", func(msg *gateway.StreamData) *gateway.StreamData {
        svc.Server().Broadcast(&gateway.StreamData{
            Route:     "global_broadcast",
            Payload:   map[string]string{"message": msg.GetPayload()["message"]},
            Timestamp: time.Now().UnixMilli(),
        })
        return &gateway.StreamData{
            SessionId: msg.SessionId,
            Route:     "broadcast_msg_ack",
            Payload:   map[string]string{"code": "200"},
            Timestamp: time.Now().UnixMilli(),
        }
    })

    svc.Run()
}
```
