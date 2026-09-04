# sgate - 高性能游戏网关

sgate 是一个基于 Go 语言编写的高性能游戏网关，支持 TCP/UDP/WebSocket 多协议接入，通过 gRPC 双向流与逻辑服通信，具备**百万级 QPS** 双向转发能力。

## 架构概览

```
客户端 (TCP/UDP/WS)
    │
    │  [4字节长度][MessageFrame{cmd, seq_id, body=StreamData}]
    ▼
┌─────────────────────────────────────────────────┐
│                  sgate Gateway                   │
│                                                  │
│  OnTraffic ──► ExtractMessageFrame ──► Forward ──►│──► gRPC StreamData ──► Logic Server
│  OnTraffic ◄── EncodeMessageFrame ◄── push    ◄──│◄── gRPC StreamData ◄── Logic Server
│                                                  │
│  SessionManager: session映射/状态机               │
│  GroupManager: 组管理/组广播/全服广播             │
└─────────────────────────────────────────────────┘
         │
    Logic Server (gRPC 双向流)
```

### 简化架构 vs 企业级架构

当前代码库包含两套架构：

- **简化架构**（当前活跃代码）：核心转发逻辑，代码精简，适合快速迭代和性能调优
- **企业级架构**（`//go:build legacy` 标签）：包含安全防护、过载保护、过滤器链、可观测性等完整企业特性

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

- **高性能**: 基于 gnet v2 事件驱动网络框架，零拷贝帧解析
- **多协议**: TCP、UDP、WebSocket
- **服务发现**: etcd 租约注册与 watch 发现
- **集群部署**: 多节点水平扩展，Leader 选举与自动容灾
- **安全防护**: IP 白名单/黑名单、多维限流、熔断器、WAF、TLS（企业级架构）
- **监控**: `/stats` JSON + `/metrics` Prometheus + `/health` K8s 探针（企业级架构）
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

    // Gateway control (1,000,000 - 1,099,999)
    CMD_LOGIN_GATE_REQ    = 1000001;
    CMD_LOGIN_GATE_ACK    = 1000002;
    // Logic business (1,100,000 - 1,199,999)
    CMD_LOGIN_REQ         = 1100001;
    CMD_LOGIN_ACK         = 1100002;
    CMD_HEARTBEAT_REQ     = 1100010;
    CMD_HEARTBEAT_ACK     = 1100011;
}
```

### 路由常量（gateway/routes.go）

| 常量 | 值 | 说明 |
|------|-----|------|
| `RouteLoginGate` | `"login_gate"` | 选服并将 session 绑定到指定 logic server，客户端不感知 zone |
| `RouteLogin` | `"login"` | Logic 业务登录 |
| `RouteHeartbeat` | `"heartbeat"` | Logic 心跳 |
| `RouteUserOffline` | `"user_offline"` | Gateway 通知 Logic 客户端异常断开 |
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

# Gateway (简化架构)
go build -o gw.exe ./cmd/gateway/

# Gateway (企业级架构，含安全/监控/过滤器链)
go build -tags legacy -o gw_legacy.exe ./cmd/gateway/

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

### 简化架构（当前活跃代码）

**环境**: Intel Core i5-10400F (6 核 12 线程, 2.90GHz), Windows, Go 1.22.5

| 测试 | 连接数 | QPS |
|------|--------|-----|
| 双向 Heartbeat | 500 | **~350K** |
| Personal Push | 500 | **~365K** |

### 企业级架构（legacy 构建）

**环境**: Intel Core i5-10400F (6 核 12 线程, 2.90GHz), Windows, Go 1.22.5

#### 双向压测（100 连接, 10s, batchSize=16）

| 指标 | 数值 |
|------|------|
| 正向 QPS (client→gateway→logic) | **2.25M** |
| 推送 QPS (logic→gateway→client) | **2.17M** |
| 正向丢弃 | **0** |
| 推送丢弃 | **0** |
| 结果 | **BIDIRECTIONAL SUCCESS** |

#### 推送压测（100 连接, 10s, batchSize=16）

| 模式 | 发送 QPS | 接收 QPS | 有效吞吐 | 说明 |
|------|----------|----------|----------|------|
| Personal push | 1.17M | 1.08M | 1.08M/sec | 每次请求推 1 个客户端 |
| Group push (100人) | 280K | 198K | **19.8M/sec** | 每次请求推整个组 |
| Broadcast (100人) | 386K | 366K | **36.6M/sec** | 每次请求推所有客户端 |

### 性能对比分析

| 对比维度 | 简化架构 | 企业级架构 |
|----------|----------|------------|
| 正向 QPS | ~350K (500连接) | 2.25M (100连接) |
| 推送 QPS | ~365K (500连接) | 2.17M (100连接) |
| 批量转发 | 无 | RouteBatch 多帧合并 |
| 写合并 | AsyncWrite | Writev 多缓冲区合并 |
| gRPC 流 | 单流 per logic | 多分片 StreamManager |
| 代码复杂度 | 低（~1000行核心） | 高（含安全/监控/过滤器链） |

**结论**: 简化架构牺牲了批量转发和写合并等优化，换取代码可维护性。适合中小规模场景（百万连接以下），大规模场景建议使用企业级架构。

## 实现原理

### 1. 网络层：gnet 事件驱动

Gateway 使用 gnet v2 作为网络框架，所有 TCP 连接运行在事件循环中：

```
OnTraffic(conn, inBuf)
  ├── 循环解析 [4字节长度][MessageFrame] 帧
  ├── 首帧拦截: LoginGateReq 选服并绑定 session
  ├── 认证守卫: 未绑定连接拒绝转发
  └── 转发: SendToLogic(sess, cmd, body)
```

**关键优化**：
- 零拷贝帧解析：`ExtractMessageFrame` 使用 protowire 直接扫描字节，不反序列化整个 MessageFrame
- AsyncWrite 异步写入：gnet 事件循环内批量处理，减少系统调用次数
- 写缓冲区调优：Socket 缓冲区 4MB，读写缓冲区 256KB

### 2. 协议层：MessageFrame + StreamData

```
客户端发送 (wire bytes):
  [00 00 00 1B] [MessageFrame{cmd=1000001, seq_id=1, body=LoginReq{...}}]
   ↑ 4字节大端长度     ↑ protobuf 序列化

Gateway 解码:
  ExtractMessageFrame(data) → (cmd=1000001, seqID=1, body=LoginReq_bytes)
  → StreamData{SessionId, UserKey, Cmd: cmd, SeqId: seqID, Data: body}

Gateway 转发 (gRPC stream):
  stream.Send(&StreamData{
    SessionId: "conn_abc123",
    UserKey:   "uuid_12345",      // 登录后填充
    Cmd:       1000001,
    Data:      LoginReq_bytes,
    ClientIp:  "192.168.1.100",
  })

Logic 处理:
  按 cmd 查找 cmdRoutes → 调用注册的 handler → 返回 StreamData

Gateway 编码回包:
  SendFrameToConn(conn, cmd, seqID, body)
  → EncodeMessageFrame → [4字节长度][MessageFrame{cmd, seq_id, body}]
```

### 3. 登录流程

```
客户端                    Gateway                     Logic
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=1000001,         │── 绑定 session→logic-1    │
  │    LoginGateReq}        │                           │
  │◄── LoginGateAck ───────│                            │
  │   {sessionId, serverId} │                           │
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=1100001,         │── StreamData ─────────────►│
  │    LoginReq}            │                           │── 注册 user→session 映射
  │                         │                           │── 返回 LoginAck
  │                         │◄── StreamData ────────────│
  │◄── MessageFrame ───────│  {UserKey:"uuid_xxx"}     │
  │   {cmd=1100002}         │                           │
```

### 4. 心跳流程

```
客户端                    Gateway                     Logic
  │                         │                           │
  │── MessageFrame ────────►│                           │
  │   {cmd=1100010,         │── StreamData ─────────────►│
  │    HeartbeatReq}        │                           │── 返回 HeartbeatAck
  │◄── MessageFrame ───────│                           │
  │   {cmd=1100011}         │◄── StreamData ────────────│
```

### 5. 推送模式

#### 个人推送
```
Logic 调用 SendToClient:
  Stream.Send(&StreamData{SessionId: "xxx", Cmd: cmd, Data: data})
  → Gateway receiveLoop 收到 → SendToClient(sessionID, cmd, data)
  → 按 sessionID 查找连接 → EncodeMessageFrame → AsyncWrite
```

#### 组推送
```
Logic 调用 Broadcast (带 group_id):
  Gateway.SendToGroup(groupID, cmd, data)
  → GroupManager.RangeSessions(groupID, fn)
  → 遍历组内所有连接 → 逐条 SendFrameToConn
```

#### 全服广播
```
Logic 调用 BroadcastAll:
  Gateway.Broadcast(cmd, data)
  → SessionManager.Range(fn)
  → 遍历所有活跃连接 → 逐条 SendFrameToConn
```

### 6. Session 状态机

```
StateConnected (TCP 已连接)
    │
    │  LoginGateReq + LoginGateAck
    ▼
StateBound (已绑定 logic server)
    │
    │  Logic 返回 UserKey
    ▼
StateAuthenticated (已认证)
```

## 项目结构

```
sgate/
├── cmd/gateway/              # Gateway 主入口
│   ├── main.go               # CLI 入口、配置加载、信号处理
│   ├── gc_tune.go            # GC 调优
│   └── priority_*.go         # 进程优先级设置
│
├── internal/                 # 核心实现
│   ├── gateway.go            # Gateway 核心 + gnet EventHandler + 帧编解码
│   ├── session.go            # Session/SessionManager (FSM: Connected→Bound→Authenticated)
│   ├── grpc_server.go        # GRPCServer: GatewayStream + Gateway 双 service 实现
│   ├── groups.go             # GroupManager: 隐式组生命周期管理
│   ├── transport_component.go# gnet 传输层组件
│   ├── config/               # 配置解析 (config.go, defaults.go)
│   ├── security/             # 安全组件 (IP/限流/熔断/WAF/JWT)
│   ├── traffic/              # 流量组件 (灰度/镜像/降级/WASM)
│   ├── cluster/              # 集群组件 (etcd/告警/负载均衡)
│   ├── obs/                  # 可观测性 (pprof/prometheus/tracing)
│   └── codec/                # 协议编解码
│
├── gateway/                  # 公共路由定义 + 协议辅助函数
│   └── routes.go             # 路由常量、ExtractMessageFrame、类型别名
│
├── logic/                    # Logic Server SDK (legacy)
│   ├── server.go             # 推送/组管理/广播
│   ├── handler.go            # RouteHandler/Dispatcher
│   └── service.go            # gRPC 服务 + etcd 注册
│
├── types/                    # 公共类型 (FilterContext, FilterChain)
├── api/codes/                # 错误码定义
│
├── examples/
│   ├── bench/                # 双向压测客户端
│   ├── push_bench/           # 推送压测 (personal/group/broadcast)
│   ├── logic_server/         # 逻辑服示例 (完整路由注册)
│   └── integration/          # 集成测试示例
│
├── config/                   # 配置文件
│   ├── config.yaml           # 主配置
│   ├── prometheus.yml        # Prometheus 抓取配置
│   └── grafana-dashboard.json# Grafana 仪表盘
│
├── go.mod                    # Go 模块定义
├── go.sum                    # 依赖校验
├── .golangci.yml             # Linter 配置
├── README.md                 # 项目文档
└── DESIGN.md                 # 设计文档
```

## 配置

```yaml
# config/config.yaml
port: 8081                    # HTTP 管理端口
logLevel: info
serverId: "gateway-1"
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

logicServers:
  - serverId: "logic-1"
    zone: "default"
    address: "localhost:50052"

monitoring:
  pprofAddr: ":6060"          # pprof 地址
  prometheus:
    enabled: true
    addr: ":9100"
    path: "/metrics"
    prefix: "app"

configCenter:
  enabled: true
  type: "etcd"
  endpoint: "http://127.0.0.1:2379"

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
