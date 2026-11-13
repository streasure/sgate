# sgate 逻辑服接入手册

面向游戏逻辑服（或任意后端业务进程）开发者：如何嵌入 `github.com/streasure/logic` SDK、接入 sgate 网关、收发消息与实现可靠推送。
客户端侧对接见 [client-guide.md](client-guide.md)；架构原理见 [architecture.md](architecture.md)。

参考实现（本仓可运行的最完整逻辑服）：`bench/logic_reliable`（登录处理 + 分组 + `PushReliable` + 指标对账，推荐首选参考）、`bench/logic2_ws` / `logic2_tcp`（下行推送）、`bench/logic1_ws` / `logic1_tcp`（上行接收）。

---

## 1. 接入模型（先看懂这张图）

```text
                 ┌────────────────────────── sgate 网关 ──────────────────────────┐
  客户端 ◄─TCP/WS─►  pipeline（鉴权/限流/过滤）  ◄──96 流×4 连接──►  你的逻辑服
                 └───────────────────────────────────────────────────────────────┘
                                    ▲
                     etcd：网关地址发现 + 你的服务注册
```

三个关键事实：

1. **flip 模式**：不是网关连你，而是**你的进程主动拨入网关**（gRPC 双向流 `GatewayStream.onData`）。网关地址来自 etcd 发现（网关 `discovery.registerSelf: true` 时注册）或 `gateways` 静态配置。
2. **连接责任在你侧**：拨入失败/断线由 SDK 自动重拨（`dialer`），网关重启后会自动恢复，无需网关感知你的存在。
3. **SDK 不参与网关分层依赖**：`github.com/streasure/logic` 是独立仓嵌入库（sgate 的 `internal/gateway`、`internal/backend` 均不 import 它）——你只需 import 它，与网关之间只有 gRPC + etcd 两个契约。

---

## 2. 快速开始

```go
package main

import (
    "context"

    protocol "github.com/streasure/protocol/gateway"
    logic "github.com/streasure/logic"
    "github.com/streasure/util/tlog"
    "google.golang.org/protobuf/proto"
)

func main() {
    // 1. 创建服务（全部可选参数，零值走默认）
    svc := logic.NewService(
        logic.WithListenPort("50061"),          // 本进程 gRPC 监听端口
        logic.WithServiceID("logic1"),           // 实例 ID（etcd instanceID，须唯一）
        logic.WithZone("default"),
        logic.WithEtcd("http://127.0.0.1:2379"),
        logic.WithShardCount(0),                 // 0 = NumCPU*8（12C 机器 = 96，与网关对齐）
        logic.WithGateways("127.0.0.1:50051"),   // 可选：静态网关地址兜底（与 etcd 发现互补）
        logic.WithReliability(logic.ReliabilityConfig{WindowSize: 64}),
    )

    // 2. 注册上行处理器（cmd → 请求 protobuf 类型 → 处理函数）
    svc.RegisterProto(protocol.CmdLoginGate, &protocol.LoginGateReq{}, 0,
        func(ctx *logic.Context, req proto.Message) proto.Message {
            _ = req.(*protocol.LoginGateReq)
            // ctx.ConnectionID = 会话 ID；ctx.UserUUID = {serverId}:{userId}
            svc.Server().RegisterUser(ctx.UserUUID, ctx.ConnectionID)
            return nil // 返回非 nil 且 respCmd != 0 时自动回包
        })

    // 3. 启动：gRPC 监听 → etcd 注册 → 拨入网关（失败自动重试）
    if err := svc.Start(); err != nil {
        tlog.Error(context.TODO(), "start failed error=%v", err)
        return
    }
    defer svc.Stop() // 优雅关闭：停拨入 → 清流 → 限时 GracefulStop

    // 4. 业务循环……
}
```

`svc.Run()` = `Start()` + 等 SIGINT/SIGTERM + `Stop()`，简单进程直接用它。

---

## 3. 配置项（ServiceOption）

| Option | 对应配置 | 默认 | 说明 |
| --- | --- | --- | --- |
| `WithListenPort(port)` | listenPort | `50052` | 本进程 gRPC 监听端口（网关不连它，供观测/扩展） |
| `WithServiceID(id)` | serverId | `""` | 实例 ID，etcd instanceID；**集群内必须唯一** |
| `WithServerType(t)` | serverType | `Logic` | 服务类型 |
| `WithServiceName(n)` | serviceName | `logic` | 服务名 |
| `WithZone(z)` | zone | `default` | 可用区（空值不生效） |
| `WithEtcd(endpoint)` | etcdEndpoint | `http://127.0.0.1:2379` | 单节点 etcd（或用 `etcdEndpoints` 列表字段） |
| `WithGateways(addrs...)` | gateways | 空 | 静态网关 gRPC 地址（`ip:port`），etcd 不可用时的兜底 |
| `WithShardCount(n)` | shardCount | `0` = `NumCPU×8` | 拨入每个网关的流分片数；与网关 `stream.shardCount`（96）对齐 |
| `WithReliability(cfg)` | reliability.* | 见 §7 | 可靠推送参数，零值字段回退默认 |

其他 `ServiceConfig` 字段（`connGroupCount` 默认 4、`dispatchWorkers`、`streamSendChSize`、`advertiseAddr`、etcd 账号密码等）见 logic 仓 `config.go` 注释。

**etcd 注册**：键 `/services/{belong}/SERVER_TYPE_LOGICSERVER:{zone}/{instanceId}`，地址为 `AdvertiseAddr`（默认取出口 IPv4 + listenPort），lease TTL 10s 自动续期。`ServiceID` 为空则不注册（仅静态 `gateways` 模式可用）。

**与网关对齐的参数**：`shardCount`（网关 `stream.shardCount=96` ↔ logic `NumCPU×8`）、`belong`/`zone` 必须与网关配置一致，否则发现不到彼此。

---

## 4. 接收上行消息

### 4.1 RegisterProto

```go
svc.RegisterProto(cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler)

type ProtoHandler func(ctx *logic.Context, req proto.Message) proto.Message
```

- `reqProto`：该 cmd 的请求 protobuf **实例**（SDK 按类型反射解码 `StreamData.Data`，带对象池）
- `respCmd`：handler 返回非 nil 时，SDK 自动 marshal 并以 `respCmd` 回包给客户端；**返回 nil 或 `respCmd=0` 表示不回包**
- 未注册的 cmd：先走内置命令（§4.2），否则告警 `received unregistered cmd` 丢弃

### 4.2 上下文 `logic.Context`

| 字段 | 含义 |
| --- | --- |
| `ConnectionID` | 会话 ID（session ID，格式见 client-guide §7） |
| `UserUUID` | 用户标识 `{gatewayServerId}:{userId}`，登录后由网关注入 |
| `Server` | `*Server` 引用（推送/分组 API 都在它上面） |
| `Msg` | 原始 `StreamData`（含 `Cmd`/`SeqId`/`ClientIp`） |

### 4.3 内置命令（SDK 自动消费，无需注册）

| 命令 | SDK 行为 |
| --- | --- |
| `CmdHeartbeatReq(1100010)` | 静默消费（网关健康检查心跳，无需应答） |
| `CmdUserOffline(1100012)` | 调 `Offline(sessionID, userUUID)`：清理会话/分组/用户映射，防泄漏 |
| `CmdMsgAck(1100013)` | 可靠推送 ACK，批量移出未确认窗口（§7） |
| `CmdLoginGate(1000001)` | **不内置**——原始 `LoginGateReq`（带 SessionId/UserKey）由网关转发给你，需自行 `RegisterProto` 处理（登录即建会话/分组的时机） |

> 登录消息是原始 `LoginGateReq` 直通，没有 LoginReq/LoginAck 转换——你直接读 `server_id`/`user_id`，需要回包时用 `respCmd`。

---

## 5. 推送 API

全部在 `svc.Server()` 上，返回值语义：`error` 仅 `PushToConnection/PushReliable` 有；`int` 为**成功投递的会话数**。

| API | 语义 | 典型用途 |
| --- | --- | --- |
| `PushToConnection(sessionID, cmd, data) error` | 尽力而为投递（**本地入队即成功**，可能丢） | 位置同步、可自愈消息 |
| `PushReliable(sessionID, cmd, data) error` | **可靠推送**：会话内单调 SeqId + 未确认窗口 + 超时重传（§7） | 结算、掉落、交易 |
| `SendToUser(userUUID, cmd, data) int` | 按用户 UUID 查会话后投递，返回 0/1 | 已知 userUUID 的定向推送 |
| `SendToGroup(groupID, cmd, data) int` | 快照组员后逐个投递，返回成功数 | 小队/房间 |
| `SendToGroupWithRetry(groupID, cmd, data, maxRetries) (int, []string)` | 组播 + 失败名单 | 需要感知失败方的场景 |
| `Broadcast(cmd, data) int` | 向所有活跃会话广播（分片并发，先快照再投递） | 全服公告 |
| `Kick(sessionID, args...) int` | **只投递到该会话**，绝不广播；缺省 cmd=1100012，可传 `(int32 cmd, []byte body)` 自定义（如 `CmdKickNotify` + `logic.KickNotify`） | 业务踢人 |

注意事项：

- **批量是网关的事**：`batchPush`/`PushBatch`/写合并在网关侧完成，你只管单条 API，无需感知
- `PushToConnection` 返回 `nil` ≠ 客户端收到——丢不起的用 `PushReliable`
- 会话不存在（已断线）返回非 nil 错误，属正常竞态，按业务忽略或计数

---

## 6. 会话 / 用户 / 分组管理

```go
svc.Server().RegisterUser(userUUID, sessionID)    // 登录处理时建立映射（SendToUser 依赖）
svc.Server().UnregisterUser(userUUID)
svc.Server().GetConnectionIDByUser(userUUID) (string, bool)

svc.Server().JoinGroup(groupID, sessionID) int    // 返回成员数
svc.Server().LeaveGroup(groupID, sessionID) int
svc.Server().JoinGroupForUser(userUUID, groupID) error
svc.Server().LeaveGroupForUser(userUUID, groupID) error
svc.Server().GetGroupMembers(groupID) []string    // 快照
svc.Server().GetGroupCount(groupID) int

svc.Server().Offline(sessionID, userUUID)         // 主动清理（CmdUserOffline 内部也调它）
svc.Server().GetConnectionCount() int             // 当前活跃会话数
```

**防泄漏要点**：`Offline` 由网关的 `CmdUserOffline` 自动触发（断线/心跳超时/被踢），会清掉该会话的分组与用户映射——**不要只依赖自己的登出处理**，异常断连路径全靠它。

---

## 7. 可靠推送（PushReliable）

### 7.1 原理（为什么网关零改动）

ACK 建在**端到端（你的进程 ↔ 客户端）**：你按会话单调分配 `SeqId` 写进 `StreamData.SeqId`（字段既有，wire 零改动），消息进该会话的未确认窗口；客户端回 `CmdMsgAck(1100013)` 由 SDK 批量清窗。gRPC 流断、网关崩溃、异步写失败、客户端没执行——一切丢包最终都表现为"没收到 ACK"，被同一机制兜住。**网关保持纯管道，不存任何可靠状态。**

### 7.2 配置 `WithReliability`

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `WindowSize` | `64` | 每会话未确认窗口条数上限 |
| `ScanInterval` | `200ms` | 重传扫描周期（janitor 扫描式，非每消息一个 timer） |
| `InitialBackoff` | `300ms` | 首次重传退避 |
| `MaxBackoff` | `5s` | 指数退避上限 |
| `MaxRetries` | `5` | 单条最大重传次数（不含首发），达上限放弃 + `giveUps` 计数告警 |

### 7.3 错误与语义

```go
err := svc.Server().PushReliable(sessionID, cmd, data)
switch {
case err == nil:                      // 已入窗口并首发成功
case errors.Is(err, logic.ErrReliableWindowFull):
    // 背压：客户端 ACK 慢导致窗口满——绝不静默丢，由业务决定
    // 等待重试 / 降级为 PushToConnection / 放弃
default:
    // 会话已断等
}
```

- **会话内 at-least-once**：会话 Offline/断流即清窗（未确认条目计入 `droppedOffline`），**不跨会话续传**——断线补齐靠客户端重连后的登录全量/增量同步（你的业务流程）
- **去重是客户端的职责**：重传会导致重复投递，客户端按 seq 滑动窗口去重（窗口 1024，见 client-guide §7）
- **顺序**：ACK 层只保证到达 + 去重，补发可能晚于新消息，顺序由消息自带版本号兜底
- **按需 opt-in**：只有丢不起的消息走 `PushReliable`——全量 ACK 等于在应用层重造 TCP

### 7.4 观测

```go
snap := svc.Server().PushMetricsSnapshot()
// snap["acked"]           客户端已确认
// snap["retransmits"]     重传次数
// snap["giveUps"]         达重传上限放弃（配合告警）
// snap["droppedOffline"]  断线清窗条数
// snap["totalFailed"]     其他失败（会话不存在等）
```

对账等式（联调验收用）：`pushed = acked + droppedOffline + giveUps`（正常场景 giveUps=0）。
网关侧互补指标：`/stats` 的 `asyncWriteFailed`（异步写客户端失败）。

---

## 8. 运行与运维

- **日志**：统一 `github.com/streasure/util/tlog`（`tlog.Info(ctx, "fmt %v", ...)`），禁止 `fmt.Print*` / `log.*`（AGENTS.md 规范）
- **启动顺序**：etcd → 网关 → 你的进程（或反之——拨入自动重试，顺序不敏感）
- **优雅停止**：`svc.Stop()`（停拨入 → 清流 → 5s 限时 GracefulStop，超时强杀），务必在 SIGINT/SIGTERM 处理里调用
- **重连语义**：网关重启 → 你的 dialer 自动重拨、流重建；期间发出的 `PushToConnection` 会失败（断线缓存/丢弃由网关侧策略决定），关键消息用 `PushReliable` 覆盖窗口内、断流后清窗由业务重连拉取补齐
- **pprof**（可选）：`obs.StartPProfServer("127.0.0.1:6061")`（bench logic 均支持 `-pprof` flag）

---

## 9. 联调检查清单

- [ ] `belong`/`zone` 与网关一致，`ServiceID` 全局唯一
- [ ] 网关 `discovery.registerSelf: true` 或你配了 `WithGateways` 静态地址
- [ ] 启动日志出现 `logic service started ... shards=... connGroups=...`，网关侧出现 `logic stream attached serviceID=... shard=N/96`（96 流全部挂载）
- [ ] 注册了 `CmdLoginGate` 处理器并 `RegisterUser`（否则 `SendToUser` 找不到会话）
- [ ] 收到上行后能按 `ctx.ConnectionID` 回包（`PushToConnection` 验证下行通路）
- [ ] 可靠推送：`pushed = acked + droppedOffline` 严格对账（`bench/client_ack` + `bench/logic_reliable` 可直接复用）
- [ ] `CmdUserOffline` 清理路径验证（杀客户端进程 → 分组/用户映射归零）

---

## 10. 常见问题

**Q: 网关日志一直 `logic stream attached shard=...` 只挂了几条？**
`shardCount` 不一致或拨入中断。logic 默认 `NumCPU×8`，网关默认 96，12 核以下机器会少于 96 条——用 `WithShardCount(96)` 对齐。

**Q: 上行一直 `received unregistered cmd`？**
该命令没有 `RegisterProto`（内置命令只覆盖 heartbeat/offline/msgack/login）。检查 cmd 是否注册、reqProto 类型是否与客户端发的一致。

**Q: `PushToConnection` 返回 nil 但客户端说没收到？**
正常——尽力而为语义。查网关 `/stats`：`pushDroppedNoConn`（会话已断）、`asyncWriteFailed`（写客户端失败）。关键消息改 `PushReliable`。

**Q: `PushReliable` 一直 `ErrReliableWindowFull`？**
客户端没回 ACK 或回得太慢（去重窗口未实现/ACK 丢弃）。检查客户端 §7 实现，或调大 `WindowSize`（只是缓冲，不解决根因）。

**Q: 重连后收到重复消息？**
预期行为（at-least-once + 断线清窗不续传的组合）。客户端去重窗口必须覆盖重连场景。

**Q: `Kick` 把全服都踢了？**
`Kick(sessionID)` 绝不广播（sessionID 为空直接返回 0）。检查是否误用了 `Broadcast(1100012, ...)`。

**Q: 进程收到 SIGINT 挂死？**
直接用 `svc.Run()` 或确保 SIGINT 路径调用 `svc.Stop()`——SDK 的 `Stop` 先关服务端流再 GracefulStop，顺序反了长活双向流永远不会自行结束。
