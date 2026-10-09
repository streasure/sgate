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
| 连接生命周期指标 | 平均时长 + P50/P95/P99（滑动窗口） |

### 鉴权与安全

| 能力 | 说明 |
| --- | --- |
| LogoutGate | `1000003/1000004`，断连无推送 + JWT Revoke |
| Admin 封禁 HTTP | `/admin/ban|unban|bans`，先推 `CmdBanNtf` 再断，Bearer `admin.token` |
| LoginGate 封禁拦截 | 403 |
| BanStore | 进程内实现（**TODO: 迁 MySQL**，多网关共享） |
| JWT | jti 持久化到 Connection，接入 logout/ban Revoke |
| 登录校验开关 | `loginValidation.enabled`（压测恒为 false） |

### 运维与配置

| 能力 | 说明 |
| --- | --- |
| 热配置更新 | 限流阈值、黑名单、过载保护、JWT、灰度、流量镜像、降级、连接限制、连接级流控 |
| 监控输出 | tlog 结构化日志、`/stats` HTTP API、`/debug/pprof/` |
| 速率采样 | `OnTick` 每秒采样 `msgRate`（Health 速率准确） |
| WS 实现要点 | close 正确关 TCP、握手半包累积、分片重组、强制客户端 mask |
| 多 TCP 连接并行 | `stream.connGroupCount`（默认 4），解除 HTTP/2 单连接写锁串行化 |

### 架构

- 包依赖方向单向无环：`gateway → backend → connection`；共享帧工具位于 `routes`
- 分层：`internal/gateway`（Gateway/handlers/pipeline）、`internal/backend`（LogicClient/Pool/Stream/GRPCServer）、`internal/connection`（Connection/Manager/Group/Coalescer）

---

## 待办

### P2：影响稳定性

1. **广播性能优化**
   - 问题：`BroadcastAll` 同步遍历所有连接，百万连接时广播一次可能导致秒级延迟
   - 文件：`internal/backend/grpc_server.go`、`internal/connection/manager.go`
   - 方案：广播改为异步分批投递，或按 zone 分片广播

2. **消息可靠投递（ACK + 重传）**
   - 问题：当前 TCP 写失败直接丢弃。SLG 游戏对关键消息（战斗结果、资源变更）要求 at-least-once
   - 方案：消息 ACK + 重传机制，或逻辑服侧重试 + 幂等

3. **封禁状态迁移 MySQL**
   - 问题：`BanStore` 仅进程内存，重启丢失、多网关不共享
   - 方案：MySQL 表 `user_bans(user_uuid PK, reason, jti, banned_at, expires_at)`；启动加载未过期记录；`/admin/ban` 双写内存+DB；定时清理过期行

### P3：架构增强

1. **跨网关路由（全局 Session 表）**
   - 问题：session ID 网关本地生成，逻辑服必须知道用户在哪个网关；网关重启时连接需客户端重连
   - 方案：全局 session ID + 网关路由表（etcd 维护 `session → gateway` 映射）

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
