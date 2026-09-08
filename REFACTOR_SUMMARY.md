# sgate 重构总结

## 架构

```
Client (TCP) ──MessageFrame{cmd,body}──▸ sgate (gnet) ──StreamData{cmd,data}──▸ Logic (gRPC)
Logic ──SendToUser(userUUID)/Gateway.{Broadcast,JoinGroup,...}()──▸ sgate ──TCP write──▸ Client
```

## 协议层 (E:\protocol)

### gateway.proto — 两个 service
- **GatewayStream**: 客户端↔网关双向流 (`onData(StreamData)`)
- **Gateway**: Logic→网关 gRPC (8 个 unary RPC):
  - CloseSession, KickSession, SendToClient
  - Broadcast (repeated group_id), BroadcastAll
  - JoinGroup (repeated group_id), LeaveGroup (repeated group_id)
  - GetGroupInfo

### cmd.proto — 仅客户端 CMD
- 网关控制: 1,000,000-1,099,999 (LoginGate 1000001/1000002)
- 业务逻辑: 1,100,000-1,199,999 (Login/Logout/Heartbeat/Chat/Kick 等)

### push.proto — 仅客户端推送数据
- PushNotify, Announcement, ChatMsg, KickNotify

logic 层的单用户推送以 `userUUID` 为业务目标。logic 自动维护
`userUUID -> sessionID` 映射，调用 `Server.SendToUser(userUUID, ...)` 后通过
GatewayStream 投递；`sessionID` 仅是 sgate 内部连接路由标识。

## sgate 核心 (E:\sgate\internal/)

| 文件 | 职责 |
|------|------|
| gateway.go | Gateway struct + gnet.EventHandler + wire encode/decode (4字节大端) |
| connection.go | Connection/ConnectionManager (连接、userUUID、组管理) |
| backend.go | GRPCServer、LogicClientPool、GatewayClientPool |
| frontend.go | Gateway 主逻辑、TCP/WebSocket 流程 |
| cluster_component.go | etcd 注册、Logic/Gateway 服务发现 |

## 关键设计决策

1. **组生命周期隐式管理**: 无 CreateGroup/DeleteGroup, Join 自动建组, Leave 最后成员离开自动删组
2. **连接断开**: sgate 内部 ConnectionManager 清理组，通知 logic 仅做业务清理
3. **Wire format**: 4字节大端长度前缀 + protobuf MessageFrame
4. **单用户推送**: Logic `Server.SendToUser(userUUID, ...)`→logic 内部 `userUUID→sessionID` 映射→GatewayStream→目标客户端
5. **组广播**: Client→ChatMsg→Logic→Gateway.Broadcast(group_id=[...])→组内全员
6. **全服广播**: Client→ChatMsg(no target)→Logic→Gateway.BroadcastAll()→全员

## 2026-09-08 实测结果

本次在 Windows、12 logical CPUs、Go 1.22.5、本机 loopback 环境执行。TCP 和 WebSocket 均为 10 连接、10 秒、batchSize=16；TCP 另测试 inflight=8192 和 256。

| 场景 | 客户端发送 | 客户端接收 | 平均接收 QPS | 认证失败 |
|---|---:|---:|---:|---:|
| TCP，inflight=8192 | 51,008 | 9,995 | 997 | 5 |
| TCP，inflight=256 | 12,688 | 9,990 | 997 | 0 |
| WebSocket | 91,920 | 10,000 | 998 | 0 |
| push personal stream echo | 12,656 | 9,990 | 997 | 未统计 |
| push group stream echo | 12,704 | 9,990 | 997 | 未统计 |
| push broadcast stream echo | 12,656 | 9,990 | 997 | 未统计 |

真实主动推送已由 `examples/push_driver` 覆盖：10 个客户端、10 秒、1,000 个事件/s，`SendToUser` 收到 6,530 条、约 653 QPS；10 人组推送收到 66,125 条、约 6,609 QPS；10 人全服广播收到 65,731 条、约 6,569 QPS。`examples/logic_noop` + `examples/forward_bench` 的 no-op 纯转发测试在本次环境约 6.6K msg/s 无丢弃稳定运行，实际 offered 13.3K msg/s 时转发约 10.3K 并开始丢弃。`ghz v0.120.0` 对 Gateway `GetGroupInfo` 的结果为 45,040 req/s、P99 2.02ms。生产配置启动验证成功，但未启动 logic 时 `/health`、`/ready` 返回 503；未执行双 gateway GatewayClientPool 压测。

## 后续压测缺口

1. 增加多机网络和更多连接数的压测矩阵。
2. 增加 P95/P99/P999、CPU、内存、GC 和长稳运行统计。
3. 增加双 gateway GatewayClientPool 的跨网关推送压测。
