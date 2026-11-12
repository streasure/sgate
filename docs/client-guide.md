# sgate 客户端接入手册

面向客户端（游戏客户端 / IM 客户端等）开发者的对接指南：照本文即可完成接入。
逻辑服侧对接见 [logic-guide.md](logic-guide.md)；协议字段的权威定义在 `github.com/streasure/protocol`（`gateway/gateway.proto`、`enums/cmd.proto`、`logic/push.proto`）。

参考实现（本仓可运行的最完整客户端）：`bench/client_ack`（登录 + 推送接收 + ACK + 去重 + 故障注入）、`bench/bench1_ws` / `bench1_tcp`（登录 + 上行）、`bench/bench2_ws` / `bench2_tcp`（登录 + 下行接收）。

---

## 1. 连接地址

| 传输 | 地址 | 说明 |
| --- | --- | --- |
| TCP | `host:48080` | 4 字节长度前缀帧 |
| WebSocket | `ws://host:48081/` | 二进制帧，路径 `/`（握手 `GET /`，与 bench 实现一致） |
| UDP（可选） | `host:48082` | 关联通道，需网关 `udp.enabled: true`；见 §9 |

端口以网关 `config/gateway.yaml` 的 `transports` 为准。生产环境可通过 etcd 注册信息（`/services/{belong}/SERVER_TYPE_SGATE:{zone}/{instanceId}`，JSON 含 `tcp`/`websocket` 字段）获取地址；反代场景取真实客户端 IP **仅 WS 支持**（`trustedProxies`），TCP 无 HTTP 头。

---

## 2. 帧格式

### 2.1 信封 MessageFrame

所有上行、下行消息（三个传输一致）的载荷都是 protobuf 编码的 `MessageFrame`：

```protobuf
message MessageFrame {
    int32 cmd    = 1;   // 命令号
    int64 seq_id = 2;   // 应用层序列号，网关原样透传（可靠推送的 ACK 用它对账）
    bytes body   = 99;  // 业务 protobuf 载荷
}
```

### 2.2 三种传输的打包方式

| 传输 | 打包 |
| --- | --- |
| **TCP** | `4 字节大端长度前缀（= MessageFrame 字节数）+ MessageFrame protobuf`。长度上限 `protection.maxFrameSize`（默认 4MiB），超限直接断连 |
| **WebSocket** | 一个 **binary 帧**装一个完整 MessageFrame（服务器强制客户端 mask，未 mask 会被拒） |
| **UDP** | 与 TCP 相同的 `4 字节长度前缀 + MessageFrame`，**单数据报 = 单帧**，载荷 ≤ 1400B（MTU 安全值） |

服务器 → 客户端的 TCP 回包同样是 `4 字节前缀 + MessageFrame`（`pipeline.go` 统一封装）。

### 2.3 错误帧

网关侧错误以 `cmd = 3`（`CmdError`）的 MessageFrame 返回，body 为 `commonstruct.ErrorResponse`：

```protobuf
message ErrorResponse {
    string route = ...;
    ErrorData error { message, code, details };
    int64 timestamp;   // UnixMilli
}
```

收到 `cmd=3` 表示该消息被网关拒绝（格式错误、未认证、限流等），按 `error.message` 处理。

---

## 3. 命令号总表

| 命令 | 数值 | 方向 | 说明 |
| --- | ---: | --- | --- |
| `CmdError` | 3 | S→C | 网关错误帧（§2.3） |
| `CmdLoginGate` | 1000001 | C→S | 登录网关（**唯一允许在登录前发送的命令**） |
| `CmdLoginGateAck` | 1000002 | S→C | 登录应答 |
| `CmdLogoutGate` | 1000003 | C→S | 登出（网关撤销 JWT、通知逻辑服、可靠投递应答后断连） |
| `CmdLogoutGateAck` | 1000004 | S→C | 登出应答（收到后连接即将关闭） |
| `CmdKickNotify` | 1100009 | S→C | 踢下线通知（**先推后断**），body 为 `logic.KickNotify{reason, code, message}` |
| `CmdHeartbeatReq` | 1100010 | C→S | 应用层心跳（见 §5：网关消费、不转发、无应答） |
| `CmdHeartbeatAck` | 1100011 | — | 预留；当前网关不产生该应答 |
| `CmdUserOffline` | 1100012 | S→logic | 网关→逻辑服的下线通知（客户端不可见） |
| `CmdMsgAck` | 1100013 | C→S | **可靠推送端到端 ACK**，body 为 `logic.MsgAck{repeated int64 seq_ids}`（§7） |
| `CmdPushBatch` | 9000002 | S→C | 批量推送，body 为 `gateway.PushBatch`（§6） |
| `1200000~1299999` | — | 双向 | **UDP 通道段**（§9），WS 上收到该段消息属异常 |
| 业务命令 | 其余 | 双向 | 一律走当前连接的传输（TCP/WS）；业务命令由逻辑服定义 |

**号段契约**：可靠消息（需 ACK 的）必须 `< 1200000`；可丢的状态同步消息才允许进 UDP 段。号段是三端共享 wire 契约，改号段 = 改协议。

---

## 4. 登录时序

```text
1. 建连（TCP / WebSocket）
2. C→S  CmdLoginGate(1000001)
        body = LoginGateReq{ server_id, user_id, login_key }
        · server_id：目标逻辑服实例 ID（网关按此路由登录消息）
        · login_key：登录凭证；是否校验由网关 loginValidation.enabled 决定
          （true 时必须过 loginserver，空 key 也不能绕过）
3. S→C  CmdLoginGateAck(1000002)
        body = LoginGateAck{ code, message, session_id, server_id, udp_token, udp_port }
4. code == 0 → 登录成功，记录 session_id（后续所有下行消息以它为路由键）
5. 之后才可发送业务命令（未登录时非 1000001 的命令会被丢弃，可能收到 cmd=3 错误帧）
```

**应答 code 语义**（`login.go` / `websocket.go` 实测路径）：

| code | 含义 | 客户端处理 |
| ---: | --- | --- |
| `0` | 成功 | 进入已登录态 |
| `401` | `invalid login key`（loginserver 校验失败） | 凭证错误，走业务重新登录 |
| `429` | `rate limited`（IP 限流）/ `too many concurrent logins`（登录并发槽满） | 退避重试 |
| `503` | `server overload`（过载丢弃）/ `server in maintenance`（维护模式） | 退避重试或提示维护 |

注意：

- **登录前只放行 `preAuthCommands`（默认 `[1000001]`）**，抢跑的其他命令被丢弃（不断连）
- 登录是异步处理（worker 协程），不要假设 ack 的到达顺序与其他消息严格有序
- 同一账号重复登录时**旧连接会被踢**（网关绑定新连接）
- `udp_token`/`udp_port`：网关 `udp.enabled: false` 时为空；开启时用于 §9 的 UDP 关联，30s 内一次性有效

---

## 5. 心跳与保活

连接的存活判据在网关侧，客户端必须制造"入站活动"：

| 传输 | 保活方式 | 超时行为 |
| --- | --- | --- |
| **WebSocket** | **标准 WS ping 帧**（网关回 pong），或**任意入站数据帧**——两者都刷新心跳时间 | 每 30s 检查一次，超过 `wsHeartbeatTimeout`（默认 60s）无活动 → 网关关闭连接并通知逻辑服下线 |
| **TCP** | 发送任意帧（含 `CmdHeartbeatReq`）刷新活跃时间 | 超过 `connIdleTimeout`（默认 5m，`config/gateway.yaml` 为 30s）无数据 → 断开 |
| UDP | 每 15s 发一次业务包或 `CmdUdpPing` | 关联空闲 `idleTimeout`（默认 60s，连丢 4 次保活才淘汰） |

**关于 `CmdHeartbeatReq(1100010)`**：它会通过管道但**网关不转发给逻辑服、也不产生应答**（`backend/stream.go` 消费、`logic` builtin 同样消费）——它只是"我还活着"的信号，**不要等待 ack**。推荐：WS 客户端直接用协议层 ping（浏览器自动），TCP 客户端周期发任意小帧或 `CmdHeartbeatReq`。

**建议节奏**：ping 间隔 ≤ 30s（timeout 的一半以内），确保偶发丢包不触发超时踢线。

---

## 6. 下行消息处理

### 6.1 单条推送

`cmd` 为业务命令号，`body` 为业务 protobuf，`seq_id` 按消息携带（可靠推送的消息必须保留，见 §7）。

### 6.2 批量推送 PushBatch（`cmd = 9000002`）

网关开启 `stream.batchPush: true` 时（压测与生产主路径），多条推送会打包：

```protobuf
message PushItem {
    string session_id = 1;
    int32  cmd        = 2;   // 真正的业务命令号
    bytes  data       = 3;   // 业务载荷
    int64  seq_id     = 4;   // 可靠推送序号（非可靠推送为 0）
}
message PushBatch { repeated PushItem items = 1; }
```

客户端处理：收到 `cmd=9000002` → 解析 `PushBatch` → 遍历 `items`，**把每条 item 当作独立消息处理**（`item.cmd` + `item.data`）。`PushBatch` 是传输层优化，业务无感知。

### 6.3 被踢与登出

```text
被踢：S→C  CmdKickNotify(1100009)  body=KickNotify{reason, code, message}
      → 展示原因 → 连接随后被服务端关闭（先推后断）
登出：C→S  CmdLogoutGate(1000003)  → 等 CmdLogoutGateAck(1000004) → 连接即将关闭
      登出期间服务端不再下发业务推送
```

---

## 7. 可靠推送接入（关键消息的 ACK）

`PushToConnection` 是尽力而为；逻辑服对**丢不起的消息**（结算/掉落/交易）使用 `PushReliable`——这些消息的 `seq_id > 0`，客户端负责确认与去重：

```text
1. 收到业务消息且 seq_id > 0
2. 【去重】维护滑动窗口（建议 1024，按 seq 位图/区间判断）：
   · seq 已在窗口内（重复）→ 只计数，不重复执行业务
   · seq 更大 → 执行业务，窗口前移
3. 【确认】回 CmdMsgAck(1100013)：
   body = MsgAck{ seq_ids: [1001, 1002, ...] }   // 一帧可批量确认多条
   ACK 是普通上行走现有管道，无需特殊处理
```

要点：

- **ACK 攒批**：每 50~200ms 或每 N 条合并一次，减少上行开销（`client_ack` 实现参考）
- **必须去重**：可靠推送是 at-least-once，超时重传会导致重复投递，去重窗口防止业务重复执行
- **断线清窗**：会话断开后 logic 清空未确认窗口（`droppedOffline`），**不跨会话续传**——重连后靠登录流程的全量/增量同步补齐（业务现有机制），客户端去重窗口在重连后可保留旧 seq 继续防重
- **顺序语义**：ACK 层只保证到达 + 去重，补发可能晚于新消息，顺序由业务消息自带版本号兜底

故障注入参考（联调时模拟丢包）：`bench/client_ack -drop-ack 10 -ack-interval 100ms`（丢 10% ACK）、`-no-ack`（完全不 ACK，观察 logic 背压与 giveUps）。

---

## 8. 完整接入清单

- [ ] 连接 TCP 48080 或 WS 48081（§1）
- [ ] 实现帧编解码：长度前缀 / WS binary（§2）
- [ ] 发送 `LoginGateReq`，处理 `LoginGateAck` 四种 code（§4）
- [ ] 登录前不发其他命令；只处理本端命令号（§3）
- [ ] WS 用 ping 保活（≤30s 间隔）；TCP 周期发帧（§5）
- [ ] 解析 `PushBatch`（§6.2）
- [ ] 处理 `KickNotify` / `LogoutGateAck`（§6.3）
- [ ] 可靠消息 ACK + 去重窗口（§7）
- [ ] （可选）UDP 关联通道（§9）
- [ ] 收到 `cmd=3` 错误帧的统一处理（§2.3）

---

## 9. UDP 关联通道接入（可选，`udp.enabled: true` 才启用）

一个会话两个通道：**WS 承载会话与指令面（丢不起的），UDP 承载状态同步面（位置/AOI 等高频可丢的）**。分工按命令号段静态分配：`1200000~1299999` 走 UDP，其余一律 WS。

```text
1. WS 登录成功后，从 LoginGateAck 取 udp_token、udp_port（30s 内有效，一次性）
2. 客户端自建 UDP socket（本地临时端口），向 gateway_host:udp_port 发：

   CmdUdpAssociate(1200000)  body = UdpAssociateReq{ udp_token, nonce, ver }
   ← CmdUdpAssociateAck(1200001)  body = UdpAssociateAck{ server_nonce, seq_base, state_ver }
   （NAT 映射只能由客户端先发包建立——服务器无法主动开 UDP，所以必须客户端先发）

3. 关联建立后：
   C→S  CmdUdpAction(1200002)  body = UdpAction{ seq, payload }   // 上行动作，seq 会话内单调递增
   S→C  CmdUdpState(1200003)   body = UdpState{ state_ver, snapshot }  // 下行状态，state_ver 只接受更大的
   C→S  CmdUdpPing(1200004) / S→C CmdUdpPong(1200005)             // UDP RTT 探测
4. 保活：每 15s 发一个 UDP 包（业务包或 UdpPing），否则 60s 空闲关联被淘汰
```

客户端必须实现的降级逻辑（**服务端会自动回落，客户端不需感知**，但要有兜底）：

- **UDP 不可靠**：`UdpState` 只接受 `state_ver` 更大的快照，旧包直接丢——状态同步天然幂等
- **关联失效 / 发送失败时服务端自动回落 WS**：可能收到同一状态的 WS 版本，按 state_ver 去重即可
- **丢不起的消息永远走 WS**（号段 < 1200000），不要把结算类消息放进 UDP 段
- **载荷 ≤ 1400B**（服务端 `maxDatagram`，超限被拒）
- WS 断开 → 关联随之拆除；重连后需重新 ASSOCIATE
- 存活判据只看 WS：**UDP 丢包不会导致被踢线**

反放大与安全（服务端已实现，客户端无感）：token 一次性防重放、未关联数据报静默丢弃（零回包）、seq 去重窗口 1024、per-IP 4 / 总量 100000 上限。完整设计见 [udp-ws-design.md](udp-ws-design.md)。

---

## 10. 常见问题

**Q: 登录后没有任何响应？**
确认逻辑服已启动并在 etcd 注册、且 `LoginGateReq.server_id` 与逻辑服 `-id` 一致。网关日志搜 `logic stream attached`。

**Q: 发送业务消息收到 `cmd=3` 错误帧？**
`unknown connection` = 未登录或已被踢；`Invalid message format` = 帧解码失败（检查长度前缀与 protobuf 编码）。

**Q: 连接频繁被断？**
检查保活节奏（§5）：WS 60s 无入站活动、TCP 空闲超时（30s/5m）即被清理。被踢时应先收到 `CmdKickNotify`。

**Q: 收到重复的可靠消息？**
正常——at-least-once 重传。用去重窗口挡住即可（§7）。

**Q: `PushBatch` 和单条推送混用？**
正常，`batchPush` 是传输优化，两条路径随时可能交替出现，按 §6 统一处理。

**Q: 可靠消息为什么 ACK 了还可能再收到？**
ACK 在途中丢失时 logic 会重传（300ms~5s 退避，最多 5 次）；去重窗口是唯一防线，务必实现。
