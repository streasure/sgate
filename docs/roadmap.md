# sgate 路线图

本文档为工程现状能力清单与待办事项，供后续迭代参考。架构细节见 [`architecture.md`](architecture.md)。

---

## 当前已具备能力

### 连接与资源

| 能力 | 说明 |
| --- | --- |
| 最大连接数限制 | `protection.maxConnections` |
| 单 IP 连接数限制 | `protection.maxConnectionsPerIP` |
| 启动 FD 检查 | 启动时校验文件描述符上限 |
| FrameBuf 默认值 | 零值回退 `4MiB`；百万连接建议显式 `64KiB`（见 `config_defaults_test`） |
| 连接级流控 | `protection.maxMessagesPerConn`（0=不限），pipeline 阶段 2.5 检查 |
| 重连处理 | 重连时主动关闭旧连接 |
| 分片化连接表 | ConnectionManager 分片 map；Group 成员无泄漏 |
| 分片并发广播 | `BroadcastAll`/`broadcastGroup` 先快照再 32 worker 分片并发投递（共享 `sendToConnections`），百万连接广播不再阻塞 gRPC handler；返回仅保证「已分发」 |
| 连接生命周期指标 | 平均时长 + P50/P95/P99（滑动窗口） |

### 鉴权与安全

| 能力 | 说明 |
| --- | --- |
| LogoutGate | `1000003/1000004`，断连无推送 + JWT Revoke |
| KickService 执行器 | loginserver 单向下发 KickAccounts/KickIP（x-internal-token 鉴权），sgate 无封禁存储 |
| LoginGate 封禁拦截 | 经 loginserver ValidateLoginToken（ACL 快照含 ban 表），失败 401 |
| BatchValidate 周期安全网 | `loginValidation.batchInterval`（默认 60s）批量重校验在线会话，踢 mid-login 竞态/TTL 过期失效会话 |
| JWT | jti 持久化到 Connection，接入 logout Revoke |
| 登录校验开关 | `loginValidation.enabled`（压测恒为 false） |

### 运维与配置

| 能力 | 说明 |
| --- | --- |
| 热配置更新 | 限流阈值、黑名单、过载保护、JWT、灰度、流量镜像、降级、连接限制、连接级流控 |
| 监控输出 | tlog 结构化日志、`/stats` HTTP API、`/debug/pprof/` |
| 流量镜像 | `trafficMirror` 按 percent 稳定采样上行，经 `GatewayStream.OnData` 真实转发到独立测试环境 logic；目标故障指数退避（1s→30s）不阻塞主流程，`/stats` 输出 `mirrorForwarded/mirrorDropped/mirrorFailed` |
| 速率采样 | `OnTick` 每秒采样 `msgRate`（Health 速率准确） |
| WS 实现要点 | close 正确关 TCP、握手半包累积、分片重组、强制客户端 mask |
| 多 TCP 连接并行 | `stream.connGroupCount`（默认 4），解除 HTTP/2 单连接写锁串行化 |

### 架构

- 包依赖方向单向无环：`gateway → backend → connection`；共享帧工具位于 `routes`
- 分层：`internal/gateway`（Gateway/handlers/pipeline）、`internal/backend`（LogicClient/Pool/Stream/GRPCServer）、`internal/connection`（Connection/Manager/Group/Coalescer）

---

## 待办

### P2：影响稳定性

1. **消息可靠投递（端到端 ACK + 重传）** — 方案已定稿（2026-11-10），待分阶段实现

   **问题**：下行推送现在是"本地入队即算成功"——每段各自的本地成功都不代表客户端真的用上了：

   | 丢包点 | 现状 |
   | --- | --- |
   | logic → 网关 gRPC 流中途断开 | sendLoop `SendMsg` 失败即 shutdown，整批静默丢失 |
   | 网关异步写客户端失败 | 已埋点：`/stats` 的 `asyncWriteFailed`（原 `NoopAsyncCallback` 吞掉写错误，无感知） |
   | 网关崩溃 / logic 断连期 | 内存队列清零 |
   | 客户端收到但没执行 | 完全无感知 |

   **原理（为什么必须端到端）**：投递好比寄挂号信——每段传送带（TCP、gRPC）只保证"把包裹交到下一段手里"，任何一段说"我送出去了"都不等于收件人签收。逐跳各建一层 ACK（网关停一层、gRPC 停一层）修不掉任何真实缺口，只是每层各造一套窗口/定时器/重传状态；唯一能证明送达的只有收件人回签收单。因此 ACK 建在**端到端（logic ↔ 客户端）**，网关保持纯管道、零新状态——gRPC 断、网关崩、写失败、客户端没执行，最终都表现为"没收到 ACK"，被同一机制统一兜住。**不做全量可靠**：每条消息都 ACK 等于在应用层重造 TCP（UDP 设计稿同一反模式）；只有丢不起的消息（战斗结果、资源变更）显式走可靠推送，其余靠"下一帧覆盖"自愈（与 WS/UDP 分工判据同源）。

   **方案**：
   1. protocol 仓新增上行命令 `CmdMsgAck`（1100xxx 系列，永远在 WS 段），body 为重复 seq 列表（一条 ACK 批量确认多条）
   2. logic 新增可靠推送 API：按会话单调分配 seq 写入 `StreamData.SeqId`（下行方向，客户端帧与 `PushBatch` item 已携带该字段，**wire 零改动**），消息进入该会话**未确认窗口**（有界，默认 64，满了背压返回错误给业务，绝不静默丢）
   3. 超时重传：窗口条目到期未收 ACK → 指数退避重发（300ms 起、上限 5s、默认最多 5 次，扫描式定时复用 janitor 模式，勿每消息一个 timer），仍失败 → 放弃 + 计数告警
   4. 客户端收到可靠消息 → **滑动窗口去重**（同 UDP seq 窗口模式，窗口 1024，重复 seq 不重复执行业务）→ 回 `CmdMsgAck`（可攒批）
   5. 网关**零改动**：ACK 是普通上行走现有 pipeline（鉴权/限流/过滤器全走），下行 seq 经 `MarshalClientMessage`/`PushBatch` 原样透传
   6. 断线语义：会话 Offline（`CmdUserOffline` / 断流清理）即清空窗口——**会话内 at-least-once，跨会话靠重连拉取**（登录后全量/增量同步是业务现有流程，本机制不跨会话续传）
   7. 观测：logic `PushMetrics` 增 acked/retransmits/giveUps/droppedOffline；网关补 async 写失败计数（Noop 回调改计数回调）

   **决策点（已定稿）**：
   1. ✅ **端到端 ACK，不逐跳** —— 只有端点能证明送达；网关"纯管道"架构不破，一切丢包归入"没收到 ACK"统一兜住
   2. ✅ **按需 opt-in（关键消息专用 API），非全量** —— 全量 ACK = 应用层重造 TCP
   3. ✅ **复用 `StreamData.SeqId` 承载下行序号** —— 字段与 PushBatch item 已存在，wire 零改动不加字段
   4. ✅ **窗口随会话，断线清窗不跨会话续传** —— 跨会话续传需持久化 + 会话迁移语义，复杂度暴涨；重连拉取已覆盖断线场景
   5. ✅ **ACK 层只保证"到达 + 去重"，不保证相对顺序** —— 丢失补发天然可能排在新消息之后，顺序由业务消息自带版本号/stateVer 兜底
   6. ✅ **窗口满 = 背压返回错误** —— 关键消息绝不静默丢，由业务决定等待/降级；与网关队列 drop 策略划清界限
   7. ✅ **重传参数进 yaml（reliability.*），号段与 seq 语义进代码** —— 沿用"契约进代码，行为进配置"

   **分阶段**：
   - 阶段 1：protocol 仓 `CmdMsgAck` 枚举 + body → sgate routes 常量（跨仓先行，同 UDP 阶段 1 模式）——✅ 已完成（protocol `4c4d72b` / sgate `ba887f1`）
   - 阶段 2：logic 可靠推送层（seq 分配 / 未确认窗口 / 超时重传 / ACK 处理 / Offline 清窗 / 指标 / 配置）——✅ 已完成（`be0b39e`，含 -race 测试）
   - 阶段 3：网关 async 写失败观测 + README/architecture 文档——✅ 已完成
   - 阶段 4：客户端实现（ACK + 滑动窗口去重）+ 测试（gRPC 断流补发、重复 ACK 幂等、窗口满背压、断线清窗、乱序到达）——待做（需客户端仓配合）

   **验收**：杀 logic↔网关 gRPC 流后客户端仍收齐全部关键消息；重复投递不重复执行；断线重连后拉取补齐；全仓门禁绿。

   - 待做：阶段 4（端到端验收需客户端实现后进行）

2. **封禁收敛遗留**
   - 已完成：P1 单向 Kick 模型（loginserver 权威库 + KickService）+ P2 BatchValidate 周期安全网
   - 待做：多 zone 部署下的跨 zone 一致性验证；NAT 场景踢人口径

3. **UDP：WS 会话 + UDP 关联通道**（阶段 1~3 已合入，默认关闭零行为变化）
   - 已完成：定位预留通道（`8f01d71`）→ 设计稿定稿（`6f97c84`）→ 阶段 1 关联表/token/ASSOCIATE（`d04d854`）→ 阶段 2 上行走通 + 弱网埋点（`430216e`）→ 阶段 3 下行号段分流唯一分流点 `SendByCmd`（`0c537ba`）；安全复核三项（反放大、token 过期、per-IP/总量表耗尽）测试齐备
   - 关联细节：号段 `1200000~1299999`、`udp.enabled` 默认 false、降级不丢、新鲜度条件、stale 埋点（详见 [docs/udp-ws-design.md](udp-ws-design.md) 与 architecture §4.4）
   - 待做：**阶段 4 压测数据**（UDP 关联建立 QPS、上行 msg/s、下行 stateVer 延迟分位数、与 WS 基线对比 + 弱网模拟）——需 Linux 环境，与 P2-4 打包进行

4. **Linux CI 与压测基线**
   - 问题：Windows 本地连接天花板约 16K，百万连接与吞吐压测无法在本机复现
   - 待做：Linux 环境的构建/压测 CI 基线，固化吞吐与连接数回归阈值

### P3：架构增强

1. **跨网关路由（全局 Session 表）→ 定稿：自描述 session ID（不建表）**

   **问题表**：
   | 现状 | 说明 |
   | --- | --- |
   | session ID `conn_{时间戳}_{序号}` 各网关本地生成 | 序号每台进程各自从 1 起，时间戳读数相同即发重号（虚拟化环境时钟粒度粗时并不罕见）；重号会让 logic 的 `sessions` 内存表把消息送到错误网关——静默数据错乱 |
   | 外部服务拿到 session ID 无法定位所在网关 | loginserver 已用 Redis 三键索引自解；跨 zone / 跨服战（P3-2、P3-4）无现成机制 |
   | "网关重启需客户端重连" | TCP 连接本性，属 P3-3 滚动升级，**本项不解决也不声称解决** |

   **消费方盘点（为什么原 etcd 建表方案不采纳）**：
   - logic 推送：flip 模式下 logic 拨入**全部**网关，会话挂在它来时的那条流上，内存 `sessions` 表即"会话→网关"映射——单 zone 内已解决
   - loginserver 踢人：登录时已写 Redis `sess:conn:{sgateId}:{connId}` + etcd 服务发现拨 Kick——已有
   - etcd 建百万级 session 表：etcd 定位是**低频服务注册**，百万会话随连接频繁增删 + 每条租约心跳续约是写放大，网关崩溃后残留还要等租约超时才清理；且建了也无真实消费方

   **方案：自描述 session ID** —— 把网关身份（config `serverId`，即 etcd 注册的 instanceID）编进 ID：`conn_{serverID}_{UnixNano}_{counter}`。定位 = 解析出 serverID → 走**现有** etcd 服务发现（`{belong}/SERVER_TYPE_SGATE:{zone}/{serverId}`）取地址。零新增状态、零新表，顺带根治跨网关重号。

   **原理**：路由信息不放外部登记表，而是"信封上直接写收件部门"——表会过期、要清理、要心跳续租；写在 ID 里随会话同生共死，天然一致、零维护。

   **决策点（已定稿）**：
   1. ✅ 自描述 ID，不建 etcd session 表 —— 表无真实消费方，且有写放大 / 残留清理代价
   2. ✅ 编入 config `serverId`（与 etcd 注册 instanceID 同一标识），解析结果可直接查 etcd 取地址
   3. ✅ 格式 `conn_{serverID}_{UnixNano}_{counter}`，解析**从右取两段**（末段序号、次末段时间戳均为纯数字），serverID 含下划线也能正确还原
   4. ✅ serverID 为空（测试/工具进程）保持旧格式 `conn_{ts}_{counter}`，解析返回空 —— 零破坏兼容
   5. ✅ 跨 zone 映射存储延后到 P3-2 有真实消费方再定（届时可能需要 zone 维度，本格式与解析仍可复用）

   **分阶段**：
   - 阶段 1：connection 包 ID 生成嵌入网关身份 + `ParseSessionGateway` 解析函数 + 网关构造时注入 serverID + 测试（跨网关不重号 / 含下划线 serverID / 旧格式兼容）
   - 阶段 2：README / architecture 文档约定（格式、解析、定位流程）
   - 阶段 3（延后，随 P3-2）：跨 zone 真实消费方出现后再定映射存储

   **验收**：两台不同 serverID 网关生成的 ID 必不相同；解析函数往返一致；旧格式解析返回空不 panic；全仓门禁绿。

   - 待做：阶段 1、2

2. **多 Zone 支持**
   - 问题：当前单 zone，百万在线需跨 zone 部署（华北/华东/华南）
   - 方案：zone 路由 + 跨 zone gRPC 转发

3. **滚动升级（连接迁移）**
   - 问题：关闭网关时客户端全部断开重连，百万连接同时重连冲击逻辑服
   - 方案：连接迁移机制（新网关接管旧连接）或分批重启策略

4. **SLG：跨服战消息路由**
   - 问题：跨 zone 战斗需要跨网关消息路由
   - 方案：全局路由表 + 跨 zone 转发（依赖 P3-1、P3-2）

5. **SLG：断线重连保持状态**
   - 问题：重连后需逻辑服重新下发状态，体验差
   - 方案：网关侧消息缓存（重连后重放最近 N 条消息）
