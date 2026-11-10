# WS + UDP 关联通道设计稿

> 状态：**已评审定稿**（§7 决策点全部定案），阶段 1 已合入（`d04d854`），按 §6 分阶段推进中。
> 背景：#3 已把 UDP 定位为「预留通道」（`8f01d71`），本稿是承载业务的正式方案。
> 关联：[architecture §4.4](architecture.md#44-udp-边界与-wsudp-关联方案)、[roadmap §3](roadmap.md)、README §9.1

---

## 1. 需求与分工

**需求**：WebSocket 承载长连接通信（登录、交易、结算等指令类消息），UDP 承载数据同步（位置、AOI 等高频状态）。

**一个会话，两个通道**：WS 是**会话与指令面**，UDP 是**状态同步面**。WS 是权威、是根；UDP 只是挂在 WS 会话上的附属数据面。

| | WS 通道 | UDP 通道 |
| --- | --- | --- |
| 语义 | 可靠、有序、有连接 | 可丢、乱序、无连接 |
| 承载 | 登录/登出、交易、结算、掉落、聊天、踢人、任务进度 | 实体位置与朝向、AOI 快照、移动输入、开火动作、RTT 探测 |
| 判据 | 丢了必须重发、必须有序 | 丢了下一帧覆盖即可 |
| 存活判据 | **是**（WS 断 = 会话结束） | **否**（UDP 丢包不断线，否则弱网玩家被误踢） |

**分工判据一句话**：问"这条消息丢了，客户端能不能靠下一帧自愈？"能 → UDP；不能 → WS。

**判据如何落地**：这个判断**编码在命令号段里**（§4.1）——可丢的状态类消息分配 `1200000~1299999` 号段，sgate 按号段分流到 UDP；其余号段一律 WS。**logic 层无感知**，只管按 cmd 推送；客户端本地也持同一份号段约定决定往哪个 socket 发。这样"该走哪条通道"是协议层的一次静态分配，而不是每条消息运行时做判断。

**反模式**：在 UDP 上自己实现重传 + 排序 + 确认。那等于重新发明一个更差的 TCP，白花几周还全是 bug。确实必须到的 UDP 消息（如命中判定）应挪到 WS。

**三档可靠性**（不要只分两档）：

| 档 | 通道 | 机制 | 例子 |
| --- | --- | --- | --- |
| A 可靠有序 | WS | 现有机制，不改 | 登录、交易、结算、掉落 |
| B 可丢但要"最新" | UDP | `stateVer` 单调递增，客户端丢弃旧版本，服务器只发最新快照 | 位置、朝向、血条、AOI |
| C 尽力而为 | UDP | 无 seq 要求，丢了靠 A 档补或客户端表现兜底 | 开火特效、非关键 AoE |

---

## 2. 网络原理约束（决定了协议形状）

### 2.1 NAT 三条铁律

客户端在私网（`192.168.x.x` / 运营商 CGNAT `100.64.x.x`），出站包被 NAT 路由器改写源地址并记账：

```
客户端内网 192.168.1.5:53211 ──UDP──▶ NAT 路由器(公网 203.0.113.5:62001)
                                        记账：(203.0.113.5:62001) ↔ (192.168.1.5:53211)
                                        超时 30s~5min 不发包即删除
                                     ──▶ 游戏服 203.0.113.9:48083
```

1. **服务器永远无法主动开 UDP** —— NAT 账本上没有记录，服务器发的包直接被丢。**必须客户端先发包**。
2. **客户端监听固定端口不可行** —— 需要在 NAT 上做端口映射（UPnP/端口转发），普通玩家与运营商 CGNAT 都做不到；且本地绑哪个端口，NAT 照样换公网端口。
3. **回包必须用客户端发往的那个端口** —— NAT 按 `(服务器IP, 服务器端口)` 记账，换端口回包被当陌生人丢掉。

**推论**：正确模型只能是"客户端自建 UDP socket 主动发关联包"。QUIC、WebRTC、所有网游都是这个套路。

### 2.2 丢包的两种后果

角色每 100ms 同步一次位置，第 7 个包丢了：

- **走 WS(TCP)**：第 8、9 个包必须排队等第 7 个（严格按序交付），TCP 同时开始重传（至少等一个来回）。客户端卡约 200ms 然后瞬移；若该 TCP 上恰好在传大文件，所有消息一起被拖住（队头阻塞）。
- **走 UDP**：第 7 个丢了，没人知道也没人管；第 8 个 100ms 后正常到达，直接告知当前位置。最多 100ms 不连续，客户端插值平滑即可。

**UDP 不是"更快"，而是"延迟不会因为重传而突然抖动"**。平时两者速度差不多，差异全在丢包那一刻。

### 2.3 MTU 与两种序号

- **MTU**：以太网单包约 1500 字节，超过即分片，**丢一片整个包作废**。游戏协议单包控制在 **1400 字节**内。
- **上行 `seq`**：客户端维护 per-session 递增序号，服务器滑动窗口**去重**（防重放、防同一动作执行两次）。
- **下行 `stateVersion`**：客户端只认版本号更大的包，旧包直接扔 —— 用**覆盖**代替**重传**。

---

## 3. 关联时序

```text
客户端                                                    网关
  │ ① WS/TCP 建连 ─────────────────────────────────────▶  OnOpen → 注册 connID
  │ ② CmdLoginGate LoginGateReq（现有流程，不改）──────▶  认证、绑定 serverID
  │ ◀─ CmdLoginGateAck{session_id, udp_token} ─────────  一次性 token（短有效期）
  │
  │ ③ 客户端自建 UDP socket（本地临时端口，NAT 分配公网端口）
  │    CmdUdpAssociate{udp_token, nonce, ver} ════════▶  网关固定 UDP 端口
  │                                                      验 token → 校验 → 建关联
  │                                                      connID ↔ (公网IP:port)
  │ ◀─ CmdUdpAssociateAck{server_nonce, seq_base} ═════  同一端口回（NAT 铁律 3）
  │
  │ ④ CmdUdpAction{seq, payload}      ════════════════▶  源地址反查 connID
  │                                                      → 复用现有 pipeline 全链路
  │    CmdUdpState{stateVer, snapshot} ◀══════════════  回程 sendUDPFrame（同端口）
  │
  │ ⑤ WS 心跳（会话存活判据）
  │    UDP 保活包（维持 NAT 账本，独立计时）
  │
  │ ⑥ WS 断开 / UDP 空闲超时 → 删关联 → 客户端下次登录回到 ②
```

**为什么必须先 WS 登录再 associate**：UDP 源地址可伪造（无握手），任何人可冒充他人 IP 发包。关联包必须携带 WS 登录后取得的一次性 token；之后每个 UDP 包带短会话标识 + 序号，服务器校验后才认。

---

## 4. 协议设计

### 4.1 命令号段与分流规则（`internal/routes/routes.go`）

复用 `MessageFrame`，**不为 UDP 引入第二套编解码**（`routes.DecodeClientMessage` 原样可用）。

**核心规则：按消息性质（命令号段）分流，而非按会话状态分流。** 号段内 = 状态同步类（可丢），号段外 = 指令类（必须可靠）。logic 层完全无感知，只管按 cmd 推送；分流只发生在 sgate。

```go
// routes.go —— 号段是三端（客户端/网关/logic）共享的协议契约，定义为常量，
// 不放进 yaml：改号段 = 改协议，必须走代码评审，避免实例间配置漂移。
// 数值沿用 protocol/enums/cmd.proto 既有分段惯例（1,000,000-1,099,999 gateway、
// 1,100,000-1,199,999 logic），取下一空段 1,200,000-1,299,999。
const (
    CmdUDPRangeStart int32 = 1200000 // UDP 通道业务命令下界
    CmdUDPRangeEnd   int32 = 1299999 // 上界（1,300,000 及以上保留给 WS）
)

// IsUDPCmd 判定命令是否走 UDP 通道。必须用区间而非单边界：
// CmdPushBatch = 9000002 > 1299999，单边界会把批量推送误判进 UDP。
func IsUDPCmd(cmd int32) bool {
    return cmd >= CmdUDPRangeStart && cmd <= CmdUDPRangeEnd
}
```

**传输控制命令也放入号段内**，使全系统只有一条规则：

| 命令 | 数值 | 方向 | 说明 |
| --- | ---: | --- | --- |
| `CmdLoginGateAck` | `1000002` | S→C | 现有（WS），扩展 `udp_token` 字段 |
| `CmdUdpAssociate` | `1200000` | C→S (UDP) | 关联请求 `{udp_token, nonce, ver}` |
| `CmdUdpAssociateAck` | `1200001` | S→C (UDP) | 关联应答 `{server_nonce, seq_base}` |
| `CmdUdpAction` | `1200002` | C→S (UDP) | 上行动作 `{seq, payload}` |
| `CmdUdpState` | `1200003` | S→C (UDP) | 下行状态 `{state_ver, snapshot}` |
| `CmdUdpPing` | `1200004` | C→S (UDP) | RTT 探测（WS 有队头阻塞，测出的 RTT 不准） |
| `CmdUdpPong` | `1200005` | S→C (UDP) | RTT 应答 |
| （业务状态同步） | `1200010+` | 双向 | 业务层自定义，按需从 `1200010` 起分配 |

> 现有全部命令（`3`、`1000001~1000004`、`1100001~1100012`、`9000002`）均在区间外，天然不受影响。
> `1,200,000~1,299,999` 段已核实无占用（sgate `routes.go` 与 `cmd.proto` 均无 12xxxxx 命令）。
> 分配新命令时：**可靠消息永远 < `CmdUDPRangeStart`，可丢状态消息落在区间内**——此约定写入 `cmd.proto` 分段注释，并加 lint 测试校验已知可靠命令不落入区间。

**三条分流规则（不可混淆）**：

```text
① 主动下行推送（logic → 客户端）：IsUDPCmd(cmd) && 有 UDP 关联 → UDP
                                   否则 → WS（回落，消息不丢）
② 响应/错误帧（对上行的应答）    ：永远跟随请求到达的通道，不参与号段路由
                                   （UDP 请求的错误帧必须回 UDP，否则客户端收不到）
③ 上行（客户端 → logic）         ：不校验号段，到达通道即处理通道
                                   （pipeline 传输无关；客户端本地也持同一份号段约定）
```

规则 ② 由 pipeline 现有 `isUDP` 机制承载；规则 ① 是本设计唯一新增的分流点（§5.3）。

### 4.2 协议消息（`github.com/streasure/protocol`，需扩字段）

```protobuf
// LoginGateAck 扩展（向后兼容，新增字段）
message LoginGateAck {
    int32  code       = 1;
    string message    = 2;
    string session_id = 3;
    string server_id  = 4;
    string udp_token  = 5;   // 新增：一次性关联 token，有效期 30s，用后即废
    int32  udp_port   = 6;   // 新增：UDP 关联端口（与 WS 端口同机不同端口）
}

message UdpAssociateReq  { string udp_token = 1; bytes nonce = 2; int32 ver = 3; }
message UdpAssociateAck  { bytes server_nonce = 1; int64 seq_base = 2; int64 state_ver = 3; }
message UdpAction        { int64 seq = 1; bytes payload = 2; }
message UdpState         { int64 state_ver = 1; bytes snapshot = 2; }
message UdpPing          { int64 ts = 1; }
message UdpPong          { int64 ts = 1; int64 server_ts = 2; }
```

### 4.3 线上帧格式

UDP 数据报 = **4 字节大端长度前缀 + `MessageFrame`**（与 TCP 完全一致，单数据报 = 单帧）。现有 `routes.DecodeClientMessage` / `MarshalClientMessage` 直接复用。

- 单包 ≤ 1400B（含前缀），避免 IP 分片
- 上行 `seq` 由客户端 per-session 递增，服务器滑动窗口去重（窗口 1024）
- 下行 `stateVer` 由服务器单调递增，客户端只认更大版本

---

## 5. sgate 落点（最小侵入）

**改动集中在 `internal/gateway`（关联表 + 收包路径）与 `internal/routes`（号段常量）；`backend`/`connection` 只做"换方法名"式的最小触碰**（`connection` +1 接口 +1 方法，`backend` 5 处 `Send` → `SendByCmd`），pipeline/ConnectionManager/coalescer/security 零改动，依赖方向 `gateway → backend → connection` 不变。

### 5.1 新增：关联表 `internal/gateway/udp_assoc.go`

```go
// udpAssociation 一个 WS 会话的 UDP 关联。生命周期严格挂在 WS 会话上。
type udpAssociation struct {
    connID    string        // 关联的 WS connectionID（会话根）
    publicAddr net.Addr     // 客户端公网地址（回程目标，来自 ASSOCIATE 包源地址）
    udpConn   gnet.Conn     // ASSOCIATE 包的 conn：fd/pc 属于监听 socket，长期有效
    token     string        // 已消费的一次性 token（防重放）
    seqWindow uint64        // 上行 seq 去重滑动窗口
    stateVer  atomic.Int64  // 下行状态版本（单调递增）
    lastSeen  atomic.Int64  // 最后收包时间（UnixMilli，空闲淘汰依据）
    createdAt int64
}

// udpAssocTable 关联表。两个索引，一个数据。
type udpAssocTable struct {
    byConnID sync.Map // connID      → *udpAssociation（回程/清理用）
    byAddr   sync.Map // addrKey(ip:port) → connID      （收包反查用）
    perIP    sync.Map // ip          → count          （per-IP 上限）
}
```

**为什么存 `gnet.Conn`**：gnet 对 UDP **每个数据报新建 conn 对象**（`newUDPConn`），`OnTraffic` 返回后 `release()`（`opened=false, ctx=nil, buffer=nil`），**但不关 fd**——`fd`/`pc` 属于监听 socket，属于 listener 生命周期。所以 ASSOCIATE 时捕获的那个 conn 引用可以长期持有，后续用 `conn.SendTo(frame, addr)` 向任意地址发包（datagram 专用 API，可指定目标地址）。

**gnet 源码核实结论（v2.9.7，写稿必需）**：

| 事实 | unix | windows |
| --- | --- | --- |
| 每包建 conn | `readUDP` → `newUDPConn(fd, el, ln.addr, sa, false)` | `acceptor` → `newUDPConn(el, pc, nil, ...)` |
| `OnTraffic` 后 | `c.release()`（`remote != nil` 时） | `c.release()` |
| `release()` 清什么 | `opened/ctx/buffer`，**不关 fd、不清 remote** | `ctx/localAddr/buffer`，**不清 `pc`** |
| `AsyncWrite` 对 datagram | **同步** `sendTo(buf, nil)`，注释明言 callback 不可依赖 | 投递 event loop 后 `Write` → `pc.WriteTo(p, remoteAddr)` |
| `AsyncWritev` 对 datagram | `ErrUnsupportedOp` | `ErrUnsupportedOp` |
| 主动发包到指定地址 | `SendTo(p, addr)` → `unix.Sendto(c.fd, ...)` | `SendTo(p, addr)` → `c.pc.WriteTo(p, addr)` |

**结论：UDP 下行主动推可行**，走 `SendTo(p, addr)`（两平台都支持、可指定任意目标地址）。回程**必须**用关联表里的 `publicAddr`，且从监听 socket 发出（NAT 铁律 3）。

> ⚠️ `SendTo` gnet 文档标注 "not concurrency-safe"。实际两平台实现都是**单次系统调用**（`sendto` / `PacketConn.WriteTo`），无 conn 内部状态读写，跨协程并发安全；但为稳妥与可读性，**所有 UDP 下行发送收敛到一个 `sendUDPFrame` helper**（现有 `udp_send.go` 改造），不散落调用点。

### 5.2 收包路径：`handleUDPDatagram` 改造

```text
handleUDPDatagram(c, data)
  ├─ 源 IP 限速（现有，保留）
  ├─ 帧校验 + 解码（现有，保留；垃圾包静默丢）
  ├─ cmd == CmdUdpAssociate？
  │    └─ 是 → handleAssociate(c, msg)   // 验 token → 建关联 → 回 Ack
  ├─ 反查 byAddr[源地址] → connID
  │    ├─ 查不到 → 丢（未关联的 UDP 包一律不处理；只放行 ASSOCIATE）
  │    └─ 命中 → seq 去重（窗口）→ 构造 pipelineTask{connectionID: 真实connID,
  │                                                isUDP: true, udpRemoteAddr}
  │              → Submit 到现有 worker pool
  └─ 更新 lastSeen
```

**复用现有 pipeline 全链路**：`pipeline.Process(conn, data, message, connectionID)` 传入**真实 WS connID**，阶段 2 `GetConnection(connID)` 命中已认证连接 → 鉴权、限流、过滤器、转发**一行不改**。UDP 包天然被"会话已认证"门槛挡住，不引入第二套安全路径。

**上行 seq 去重**在入 pipeline 前做（在 gateway 层，不在 pipeline 里加 UDP 分支）。

### 5.3 回程分流（**唯一**新增分支）

下行已知 cmd 的调用点共 5 处（`SendToClient`、`BroadcastAll`、`stream` 单条、`stream` 广播、`stream` 批量累积）。**不得在每处写 `if IsUDPCmd`**——判断逻辑收敛到一个方法，调用点只换方法名：

```go
// connection 包：全系统唯一分流点
func (c *Connection) SendByCmd(cmd int32, data []byte) error {
    if c.udp != nil && routes.IsUDPCmd(cmd) {   // 两级判断：号段内 && 有关联
        if err := c.udp.SendUDP(c.id, data); err == nil {
            return nil
        }
        // 无关联 / 发送失败 → 回落 WS，消息不丢（降级不丢是硬要求）
    }
    return c.Send(data)                          // 现有 WS/TCP 路径，零改动
}
```

**为什么发送失败要回落，而不是丢弃**——先分清两种"丢"：

| 情况 | 性质 | 处理 |
| --- | --- | --- |
| `send` 成功、包**在路上丢了** | 偶发单点，下一帧会覆盖 | 无所谓，这正是"可丢消息走 UDP"的前提 |
| `send` **返回错误** | **路断了**（关联被删/空闲过期/网关重启） | 必须回落 |

路断了不回落，后果不是丢一条，而是**这条通道后续每一条都丢**——"下一帧覆盖"的自愈前提是后续帧还能送出去，路断则自愈整体失效，客户端永远停在旧状态（与卡死无异）；且下行推送无回执，不回落就是静默黑洞。也不做"缓冲等关联恢复"：那需要服务端排队 + 重传 + 确认，等于在 UDP 上重建可靠传输（§1 反模式），且关联失效要 15s/60s 才检测得到，窗口内消息照样全丢。

回落 WS 是零成本兜底：关联表生命周期严格挂在 WS 会话上（§5.4），**能走到"UDP 发送失败"这一步时 WS 会话大概率还活着**——回落只是回到默认基线通道（类比 CDN 挂了回源站）。顺序也不乱：下行状态带 `stateVer` 单调递增，客户端只认更大版本、不看来源通道，从哪条到达都行。

**依赖方向**：`connection` 定义 `UDPDownlink` 接口（consumer-defined interface，同 `LogicClientProvider` 模式），`*gateway.Gateway` 实现并注入 —— **不产生反向依赖**，`routes.IsUDPCmd` 是纯函数无状态，热路径一次区间比较，开销可忽略。

> 依赖核查：`connection → routes` 是**新增依赖边**，已验证 `routes` 不 import sgate 内任何包（`go list -deps` 确认仅依赖 protocol/grpc），无环；`routes` 本就是 gateway/backend 共享的最底层协议包，connection 引入它符合分层方向 `gateway → backend → connection`，routes 位于其下。

```go
// connection 包定义
type UDPDownlink interface {
    SendUDP(sessionID string, frame []byte) error // 无关联返回 errNoUDPAssoc
}
func (c *Connection) SetUDPDownlink(d UDPDownlink) // ASSOCIATE 成功时注入
```

**调用点改造（只换方法名，不写条件）**：

| 调用点 | 改造 |
| --- | --- |
| `grpc_server.go` `SendToClient` | `conn.Send` → `conn.SendByCmd(req.GetCmd(), data)` |
| `grpc_server.go` `sendToConnections`（BroadcastAll） | 增加 cmd 参数，内部 `SendByCmd` |
| `stream.go` 单条推送 | `conn.Send` → `conn.SendByCmd(msg.Cmd, ...)` |
| `stream.go` 广播（空 SessionId） | 同上 |
| `stream.go` 批量累积 | **入 batch 前分流**，见下 |
| `manager.go` `SendToUser`（内部通知） | 保持 `Send`（拿不到 cmd，本就该可靠） |

**批量路径的坑**：`PushBatch` 混装不同 cmd 的 `PushItem`，外层帧是 `CmdPushBatch=9000002`。若先打包再判断，UDP 消息会被裹进 WS 帧。因此**必须在入 batch 之前分流**——`flushBatch` 累积循环里 `m.Cmd` 已可见：

```text
processOne / flushBatch 累积处：
  IsUDPCmd(m.Cmd) → 不进 connBatch，直接 SendByCmd（UDP 一包一消息，不合批）
  否则            → 现有 connBatch 聚合 → PushBatch → coalescer/Send（不变）
```

**UDP 不走 coalescer 合并写**：合并的目的是减少 syscall/帧开销，UDP 一包一消息本就无此开销；且合并会增大单包丢失面（一次丢多条状态）。由于 UDP 消息在**入批前**就被分流走（§5.3），它们天然不会进入 `AddMulti`/`connBatch`——coalescer 只服务 WS 路径，无需感知 UDP。

### 5.4 生命周期

| 事件 | 动作 |
| --- | --- |
| `OnClose`（WS 断开） | `byConnID.Delete` + `byAddr.Delete` + `perIP` 计数递减 —— **会话是根，WS 断则 UDP 关联一起销毁** |
| `RemoveConnection` | 同上（兜底，两条路径都删，幂等） |
| UDP 空闲超时（默认 60s 无收包） | 后台扫描淘汰（复用现有 idle-checker 节奏）；NAT 账本过期后关联无意义 |
| 关联建立 | per-IP 计数 +1，超上限（默认 4）拒绝新 ASSOCIATE |
| token 消费 | 一次性，用后即废；重复 ASSOCIATE 同 token → 拒绝（防重放） |
| 网关重启 | 关联表内存态，全部失效；客户端收到 ASSOCIATE 超时后重新走 ②③ |

### 5.5 配置（`config/gateway.yaml`）

```yaml
transports:
  - port: 48081
    type: websocket
  - port: 48083        # 新增：UDP 关联端口（与 WS 同机不同端口）
    type: udp

udp:
  enabled: false       # 默认关闭（预留通道现状不变，显式开启才生效）
  associateTimeout: 30s   # token 有效期
  idleTimeout: 60s        # 关联空闲淘汰
  maxPerIP: 4             # per-IP 关联数上限
  maxAssociations: 100000 // 关联表总上限（防表耗尽）
  seqWindow: 1024         # 上行 seq 去重窗口
  maxDatagram: 1400       # 单包上限（MTU 安全值）
```

`Validate()` 补：`udp.enabled=true` 时必须存在 `type: udp` 的 transport；`maxDatagram ≤ 1400`；`seqWindow` 幂等对齐。

### 5.6 安全要点（实现时逐条对照）

| 风险 | 对策 |
| --- | --- |
| 源地址伪造（UDP 无握手） | ASSOCIATE 必须带 WS 登录下发的一次性 `udp_token`，验后即废；后续包按源地址反查，反查不中即丢 |
| 关联表耗尽（内存攻击） | per-IP 上限 + 总上限 + 空闲淘汰 |
| 反放大（未认证回包 > 来包） | ASSOCIATE 回包 ≤ 来包大小；未关联包**静默丢**，不回错误帧（#3 已实现的规则延续） |
| 重放 | 上行 `seq` 滑动窗口去重；token 一次性 |
| IP 分片丢包率飙升 | 单包 ≤ 1400B，超限丢弃 |
| 时钟/版本回退 | `stateVer` 服务器单调递增，客户端拒绝旧版本 |
| NAT 映射过期 | 客户端周期保活包（建议 15s，小于 NAT 最小超时 30s）；过期后关联自然失效，重连重建 |

### 5.7 明确不做的事（防蔓延）

- ❌ 不在 UDP 上做重传/排序/确认（丢不起的走 WS）
- ❌ 不给 UDP 单独建会话（关联挂在 WS 会话上）
- ❌ 不引入第二套帧编解码（复用 `MessageFrame`）
- ❌ 不让 UDP 参与会话存活判据（不断线）
- ❌ 不动 `backend` 的业务逻辑、不改 `connection` 的 WS/TCP 主路径（只加可选注入点）

---

## 6. 分阶段落地与测试计划

### 阶段 1：号段规则与关联建立（可独立合入）

- `routes`：`CmdUDPRangeStart/End` 常量 + `IsUDPCmd` 纯函数 + lint 测试（校验已知可靠命令不在区间内）+ 6 个传输控制命令码
- `protocol` 仓：`enums/cmd.proto` 增补 UDP 号段分段注释（`1,200,000 - 1,299,999 UDP state sync`）+ 6 个 Udp 命令枚举 + `LoginGateAck.udp_token/udp_port` 字段；`routes/symmetry_test.go` 双表对称守卫同步登记
- `gateway/udp_assoc.go`：关联表 + per-IP 上限 + token 生成/消费 + 空闲淘汰
- `handleAssociate`：验 token → 建关联 → 回 Ack（回程 `SendTo` 同端口）
- `OnClose`/`RemoveConnection` 删关联
- config：`udp` 段 + Validate（**只有 `enabled`/超时/上限，不含号段**）

**测试**：`IsUDPCmd` 边界（`1199999/1200000/1299999/1300000/9000002` 五个点）、token 一次性（重放拒绝）、per-IP 上限、关联建立/销毁幂等、回程目标地址正确、垃圾 ASSOCIATE 静默丢。

### 阶段 2：上行走通（复用 pipeline）

- `handleUDPDatagram`：反查 → seq 去重 → `pipelineTask{真实 connID, isUDP}` → 现有 worker pool
- 错误回程：`sendUDPFrame`（保留 #3 的错误帧路径，改为关联地址；**错误帧跟随请求通道，不参与号段路由**）
- 埋点（§8.4）：`udpAssocSweptWSSurviving`（sweep 时 WS 会话仍存活计数）+ `udpAssocUnassociatedDrops`（未关联地址丢包计数），/stats 暴露

**测试**：UDP 上行经完整 pipeline 到达 logic（鉴权/限流/过滤器生效）、seq 去重窗口、未关联丢弃、超 MTU 丢弃、与 WS 会话同 connID 不串号、UDP 请求的错误帧回 UDP、**stale 淘汰埋点计数（会话存活时 sweep 该关联计数 +1，会话已死不计）**。

### 阶段 3：下行号段分流

- `connection`：`UDPDownlink` 接口 + `SetUDPDownlink` + **`SendByCmd` 唯一分流点**
- `SendByCmd` 内**新鲜度条件**（§8.5）：`now - lastSeen ≤ idleTimeout/2` 才走 UDP，否则直接 WS（stale 黑洞窗口砍半，零新配置）
- 5 个已知 cmd 调用点改 `SendByCmd`（只换方法名）；`SendToUser` 保持 `Send`
- `stream` 批量：**入 batch 前按号段分流**，UDP 消息不进 `PushBatch`、不走 coalescer
- `gateway`：实现 `SendUDP`（查 `byConnID` → 新鲜度检查 → `sendUDPFrame`），ASSOCIATE 成功时注入

**测试**：号段内消息走 `SendTo`、号段外走 WS、**同会话混合流量（结算走 WS + 状态走 UDP）互不串道**、无关联时回落 WS 不丢消息、**stale 关联（lastSeen 超过 idleTimeout/2）直接走 WS 不尝试 SendTo**、批量开启时 UDP 消息仍逐条走 UDP、广播逐连接判断、`-race` 下并发下行安全。

### 阶段 4：压测与文档

- bench：UDP 关联建立 QPS、上行 msg/s、下行 stateVer 推送延迟分位数、与 WS 基线对比
- 文档：README §9.1 升级为"已支持"、architecture §4.4 更新、roadmap 勾销、**协议文档写明号段分配约定**
- 安全复核：反放大、token 过期、表耗尽场景

**每阶段独立中文提交，过全仓门禁（gofmt/build -p 1/vet/test -p 1/staticcheck）。**

---

## 7. 决策点（已定稿）

1. **号段边界定义为常量而非配置**（§4.1）—— ✅ **常量**。原则"契约进代码，行为进配置"：号段是三端共享 wire 契约，改动必须走代码评审与版本发布；yaml 只放行为参数（超时/上限/开关）。配置管契约会造成实例间漂移（同消息在不同网关走不同通道），且启动校验查不出漂移
2. **号段区间 `1200000~1299999`**（§4.1）—— ✅ **区间判定 + 沿用既有分段惯例**。必须用区间：`CmdPushBatch=9000002` 单边界会误判；数值取 `cmd.proto` `1,000,000 / 1,100,000` 之后的下一空段，已核实无占用，整段 100k 留给 UDP 状态同步
3. **传输控制命令也入号段**（`1200000~1200005`）—— ✅ **入**。原则"一条规则覆盖全部，消灭特例"：例外列表是 bug 温床；这些命令本就走 UDP，不入号段反而制造规则裂缝
4. **`udp_token` 下发方式** —— ✅ **`LoginGateAck` 扩字段**（wire 兼容，老客户端忽略新字段；token 登录时原子下发，不多一次往返）。独立命令 = 新增状态机（时序/超时/与登录竞争），多一套失败模式无收益
5. **UDP 不走 coalescer 合并写**（§5.3）—— ✅ **逐条发**。原则"优化不跨传输层滥用"：coalescer 是治 TCP syscall 开销的药；UDP 一包一消息无此开销，合并反增大单包丢失面，且会把 UDP 逻辑渗进 WS 批量路径
6. **`udp.enabled` 默认值** —— ✅ **`false`**。Feature flag 默认关闭：合入不改变现有行为，staging 显式开启验证，灰度放量，关开关即回滚
7. **保活节奏** —— ✅ **客户端 15s / 服务端 60s 作为默认值**（config 可调）。依据：运营商 UDP NAT 映射普遍 30~120s 过期，15s 保活持续续命；60s = 允许连丢 4 次保活才淘汰，瞬时抖动不误踢。**上线后按真实弱网淘汰率数据校准**（业务场景：地铁/电梯/跨境需业务侧补充）
8. **阶段 1 是否包含 protocol 仓改动** —— ✅ **包含，protocol 仓先行**。依赖顺序构建：sgate 编译依赖生成代码。提交顺序 protocol（枚举+字段）→ sgate routes（号段+lint）→ gateway（关联）；每仓独立过门禁，配合 `enabled: false` 阶段 1 合入对线上零行为变化
9. **NAT 重绑/网络切换导致的 stale 关联缺口如何处理**（§8.3）—— ✅ **埋点 + 单点收紧，不改协议、客户端不重连，建不建重协商机制由上线数据决定**。定性依据：可靠消息全程走 WS 无损、状态消息冻结有界（≤60s 后 janitor 淘汰回落 WS 自愈），是 **QoS 问题而非正确性问题**。方案 A（客户端检测后断 WS 重连）撤回——药比病重，把"可能卡 60s"变成"必然闪断"；方案 B（WS 内重协商取新 token）不先建——为罕见事件加协议面是过早优化，且复杂化 token 生命周期会威胁一次性防重放不变量。三个落点：阶段 2 埋点（§8.4）、阶段 3 `SendByCmd` 新鲜度条件（§8.5）。**方案 B 触发条件：埋点显示受影响会话比例超阈值（暂定 1%）**，届时拿数据评审。

---

## 8. 弱网支持与已知缺口

### 8.1 弱网在测什么

弱网不是一种网络，是四种损伤叠加，每种考验不同能力：

| 损伤 | 表现 | 考验的功能 |
| --- | --- | --- |
| 丢包（5%~30%） | 消息凭空消失 | 可靠消息的到达率；可丢消息的自愈（下一帧覆盖） |
| 高延迟（100~1000ms） | 消息晚到 | 超时判定、保活节奏、操作手感 |
| 抖动 | 延迟忽高忽低、**乱序** | 旧版本状态不能覆盖新版本 |
| 带宽受限 | 排队堆积 | 队头阻塞——WS 一条大消息卡住后续所有消息 |
| 路径切换（切 WiFi/4G、NAT 重绑） | 地址变了，旧路径作废 | 通道重建能力 |

弱网测试本质是三个问题：**丢得起的丢没丢干净（不堆积）、丢不起的到没到（不丢失）、不该断的断没断（不误踢）**。

### 8.2 四种损伤 × 支持矩阵

| 损伤 | 支持 | 落点 |
| --- | --- | --- |
| 丢包 | 可丢消息才进 UDP 号段；`stateVer` 单调，客户端只认更大版本 → 丢一帧不重传不回退 | 号段规则（阶段 1） |
| 乱序/重复 | 上行 `seq` 滑动窗口去重 | 阶段 2 |
| 误踢 | UDP 不进存活判据（WS 断才算断）；60s 空闲淘汰 ≠ 踢人 | `udp.enabled` 语义 + janitor |
| NAT 过期 | 客户端 15s 保活（服务端只收包刷 `lastSeen`） | `idleTimeout` 配置 |
| 路径失效/黑洞 | 发送失败或关联不新鲜 → 回落 WS，消息不丢 | `SendByCmd` 两级判断 + 新鲜度条件（阶段 3） |
| 放大攻击 | 未关联包静默丢、per-IP 上限、垃圾包不回帧 | `handleAssociate` 全失败路径 |
| RTT 测不准 | `CmdUdpPing/Pong` 不经 WS 队头阻塞 → 测真实网络 RTT | 号段已分配（阶段 1） |

**客户端需支持的六项**（与客户端团队协调的清单）：

1. **双 socket**：WS 登录拿到 `udp_token/udp_port` 后，用本地 UDP socket 发 ASSOCIATE
2. **号段约定**：`1200000~1299999` 走 UDP，其余走 WS（同一份契约三端持有）
3. **`stateVer` 只认更大版本、不看来源通道**（回落乱序的自愈前提）
4. **15s 保活**（UdpPing 或业务包，维持 NAT 映射）
5. **丢包率/RTT 统计上报**（seq 缺口计数 + Ping/Pong），供服务端观测与业务调参
6. **静默降级**：UDP 关联失败/超时 → 全量走 WS，功能不报错不中断（服务端回落已保证，客户端本地也要有此行为）

### 8.3 已知缺口：NAT 重绑 / 网络切换后的 stale 关联

**场景**：客户端 UDP 源地址中途变化（NAT 端口重绑、短时挂起后恢复），而 **WS 还活着**。旧关联按 `addrKey` 反查不到；重新 ASSOCIATE 因 token 已消费被拒。

**后果推演**（阶段 3 下行路径）：

```text
服务端 SendByCmd → 表里"有关联" → SendTo(旧地址) → 系统调用成功
                                          ↓
                          UDP 无投递确认 → 不报错 → 不回落 → 静默黑洞
```

- 可靠消息：走 WS，全程无损 ✅
- 状态消息：冻结 ≤60s（janitor 空闲淘汰 → 关联消失 → 回落 WS，瞬间追平 `stateVer`）⏳
- 60s 后：会话以 WS-only 正确继续，只是失去 UDP 加速

**定性：QoS 问题，非正确性问题**——全程无消息错、无状态错，只有有界的降级。

**已否决的方案**：

| 方案 | 否决理由 |
| --- | --- |
| A：客户端检测 UDP 沉默后断 WS 重连 | 药比病重——为自愈的 QoS 降级强制闪断，把"可能卡 60s"变成"必然断线" |
| B：WS 内重协商取新 token | 为罕见事件先建协议面（15s 保活让 NAT 重绑本就罕见）；token 生命周期复杂化威胁一次性防重放不变量 |

**正确处理 = 埋点 + 单点收紧，数据驱动决定是否建机制**（决策点 9）。

### 8.4 阶段 2 落点：埋点

缺口的精确签名 = **"关联被空闲淘汰时，WS 会话还活着"**。新增两个计数器：

1. `udpAssocSweptWSSurviving`：janitor sweep 删除关联时，查 connectionManager 发现会话仍存活 → +1（缺口实际发生的次数）
2. `udpAssocUnassociatedDrops`：已开启 UDP 时未关联地址丢包计数（旁证：源地址漂移频率）

上线后计算 `SweptWSSurviving / 总会话数`：

- **< 1%**：缺口不值得建任何机制，关单
- **≥ 1%**：拿数据评审方案 B

### 8.5 阶段 3 落点：`SendByCmd` 新鲜度条件

"有关联"的语义应该是**"这条路能用"**，而非"表里有条目"。唯一分流点加一个条件：

```go
// 关联新鲜才走 UDP（now - lastSeen ≤ idleTimeout/2，默认 30s）
// 不新鲜 → 直接走 WS，不尝试 SendTo（系统调用会成功，错误回不了头）
```

- 黑洞窗口 60s → **30s**（2 次保活丢失即回落）
- 零新配置：阈值复用 `idleTimeout/2`，语义自洽（路由放弃先于表淘汰）
- 只在唯一分流点加一个条件 + 一个测试，不散落逻辑

### 8.6 弱网测试计划（阶段 4 + Linux 环境）

- 模拟参数：丢包 5/10/20/30%、RTT 100/300/800ms、抖动、乱序（clumsy / Linux `tc netem`）
- 验收指标：状态消息到达率与更新频率（WS 基线 vs UDP）、RTT 分位数、**误踢率必须为 0**、关联建立成功率
- 依赖 roadmap P2-4（Linux CI 基线）：Windows 本地连接天花板 ~16K，弱网/大规模压测需 Linux
