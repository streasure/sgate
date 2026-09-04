# sgate 重构总结

## 架构

```
Client (TCP) ──MessageFrame{cmd,body}──▸ sgate (gnet) ──StreamData{cmd,data}──▸ Logic (gRPC)
Logic ──Gateway.{SendToClient,Broadcast,JoinGroup,...}()──▸ sgate ──TCP write──▸ Client
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

## sgate 核心 (E:\sgate\internal/)

| 文件 | 职责 |
|------|------|
| gateway.go | Gateway struct + gnet.EventHandler + wire encode/decode (4字节大端) |
| session.go | Session/SessionManager (连接管理, 认证状态) |
| grpc_server.go | GRPCServer (GatewayStream + Gateway 双 service 实现) |
| groups.go | GroupManager (隐式生命周期: Join 自动建组, Leave 空组自动删) |
| transport_component.go | gnet 传输层组件 |

## 关键设计决策

1. **组生命周期隐式管理**: 无 CreateGroup/DeleteGroup, Join 自动建组, Leave 最后成员离开自动删组
2. **连接断开**: sgate 内部 RemoveSession 清理组, 通知 logic 仅做业务清理
3. **Wire format**: 4字节大端长度前缀 + protobuf MessageFrame
4. **组广播**: Client→ChatMsg→Logic→Gateway.Broadcast(group_id=[...])→组内全员
5. **全服广播**: Client→ChatMsg(no target)→Logic→Gateway.BroadcastAll()→全员

## 压测结果 (12核 i5-10400F, 500连接)

| 测试 | QPS |
|------|-----|
| 双向 Heartbeat | ~350K |
| Personal Push | ~365K |
| 组推送/全服推送 | 待组成员正确加入后验证 |

## 待完成

1. push_bench 需要在 group 模式发送 JoinGroup 消息将 session 加入组
2. cmd.proto 添加 CMD_JOIN_GROUP_REQ/ACK (1100013/1100014)
3. logic_server_min 处理 CMD_JOIN_GROUP_REQ 调用 Gateway.JoinGroup
4. 重跑组推送和全服推送压测
