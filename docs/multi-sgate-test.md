# 多 Sgate 协作测试说明（已过时 2026-10-16）

> **本文档基于已删除的 BanStore/banRedis/Pub-Sub 模型编写**（封禁收敛 P1 `eb4b21c` 删除），
> `/admin/ban|unban|bans` 端点、`security.banRedis`、事件同步等均已不存在。
>
> 现行模型：loginserver 权威库 + 单向 KickService + BatchValidate 周期安全网，
> 见 `E:\loginserver\docs\ban-convergence-design.md`。多实例封禁一致性由 loginserver
> 的 SessionSgatesByAccount 路由 + sgate 本地 ForEach 兜底覆盖，无需 Redis 共享封禁。
>
> 以下用例仅作连接/登录基线（T1）与架构演进参考，封禁相关步骤（T2–T7）已不适用。

覆盖多网关实例下的封禁一致性、登录/踢线、基础连通。实现与验收标准见文末；本文档**只定义怎么测**，不改代码。

---

## 1. 拓扑与前置

### 1.1 最小拓扑（2 网关）

```
                ┌─────────┐
   clients ───► │ sgate-1 │ :48080 / :50051 / :8081
   clients ───► │ sgate-2 │ :48090 / :50052 / :8082
                └────┬────┘
                     │ gRPC（logic 主动拨号每台网关）
                ┌────▼────┐
                │ logic1  │ (可选 logic2 做推送)
                └────┬────┘
                     │
                 Redis + etcd
```

| 组件 | 配置 | 要点 |
| --- | --- | --- |
| etcd | 常驻 `127.0.0.1:2379` | 服务发现 |
| redis | `127.0.0.1:6379` | `security.banRedis.enabled: true` |
| sgate-1 | `config/cluster/gateway1.yaml`（可复制后改） | `serverId: gateway-1`，`cluster.mode: cluster`，`discovery.gatewayDiscovery: true` |
| sgate-2 | `config/cluster/gateway2.yaml` | `serverId: gateway-2`，端口错开 |
| logic1 | `bench/logic1_tcp` | 两台网关都要能发现并拨入 |

**两实例共享**：同一 etcd、同一 Redis、同一 `security.banRedis.keyPrefix`（默认 `sgate:ban:`）、同一 `admin.token`。事件默认开（`eventsEnabled` 零值=true）；`required: true` 时 Redis 连不上拒绝启动。

### 1.2 启动顺序

1. etcd、redis  
2. logic1（配置与单机 bench 相同）  
3. sgate-1 → 健康检查 `http://127.0.0.1:8081/health`  
4. sgate-2 → 健康检查 `http://127.0.0.1:8082/health`  
5. 客户端：一部分连 `:48080`（A），一部分连 `:48090`（B）

### 1.3 验证「两台都活着且互通」

| 检查 | 方法 | 理想结果 |
| --- | --- | --- |
| 各自注册 | etcd `get --prefix /services/default/SGATE:` | 两条 instanceId（gateway-1/2） |
| logic 拨入两台 | logic 日志 / 两侧 `/stats` 连接可登录 | 两台均能 LoginGate 成功 |
| admin 可用 | `POST /admin/bans`（Bearer admin.token） | 200，两台各自返回列表 |

---

## 2. 测试用例

### T1. 登录可达（基线）

**步骤**：N 个客户端均匀连 A/B，LoginGate 登录，发心跳/数据。  
**通过标准**：
- 两侧 `loginFail=0`、`/stats.failed=0`
- A/B 的 `forwarded` 都在涨
- logic 收到消息（丢弃即可，但无错误风暴）

### T2. 账号封禁：跨实例拒绝新登录

**步骤**：
1. 用户 `u-ban` 登录 **sgate-1**
2. 在 **sgate-2** 调 `POST /admin/ban` `{"userUuid":"u-ban","reason":"t2"}`
3. 等待事件传播（默认 `eventsEnabled: true` 时应为秒级；若关事件则 ≤ `syncInterval`，事件关时默认 10m 量级可手动调小测）
4. 从 **sgate-1** 再登录 `u-ban`（新连接）

**通过标准**：
| 项 | 理想（事件默认开） | 可接受（仅周期同步） |
| --- | --- | --- |
| 新登录结果 | 403 `banned: t2` | 403（≤ syncInterval 内） |
| 生效延迟 | < 1–2s | ≤ syncInterval |
| 已在线旧连接 | 秒级 403 断开（跨实例踢） | 未踢可接受，但须文档声明 |

### T3. 账号封禁：踢在线连接（Pub/Sub 默认开）

**步骤**：同 T2，但在 ban 后**不重连**，观察 `u-ban` 在 sgate-1 的旧连接。  
**通过标准**：
- 客户端收到 `CmdBanNtf`（cmd 1000005）或连接被关
- sgate-1 `/stats` 或日志显示 offline +1（或 Pub/Sub 处理计数 +1）
- **未实现广播时**：本用例标记 N/A，不失败

### T4. IP 封禁：跨实例踢 + 拒登录

**步骤**：
1. 客户端 `ipA` 从可识别 IP 登录 sgate-1（若本机全 127.0.0.1，需用多网卡/局域网双机，或对 loopback 分别用 `127.0.0.1`/`127.0.0.2`——Windows 需确认）
2. 在 sgate-2：`POST /admin/ban` `{"ip":"<ipA>","reason":"t4"}`
3. 观察 ipA 下所有连接是否被踢（跨实例依赖同步/广播）
4. 从 ipA 再连**任意**网关登录

**通过标准**：
- 新登录任意网关 403
- 已在线：广播开启 → 秒级断开；仅同步 → ≤ syncInterval 后**重登录**才拦（在线不踢为已知限制）

### T5. 解封（unban）

**步骤**：T2/T4 之后 `POST /admin/unban`（user 或 ip），等待同步，再登录。  
**通过标准**：登录恢复 200；若曾踢连接，需客户端重连（无自动恢复会话要求）。

### T6. Redis 短暂不可用

**步骤**：
1. 双实例 banRedis.enabled=true，已登录若干用户
2. 停 Redis 30–60s
3. 期间：A 封一个新账号、B 尝试登录该账号、旧用户继续打流量
4. 恢复 Redis

**通过标准**：
| 项 | 理想 |
| --- | --- |
| 网关进程 | 不退出；`ban redis init/down` 有 warn 日志 |
| 恢复模式 | 本地 ban 仍生效；跨实例新 ban **不保证**可见（声明为降级） |
| 恢复后 | 自动重连；`SyncFromRedis` 拉平；两侧 `/admin/bans` 一致 |
| 指标（若有） | `ban_redis_up=0→1`，同步失败计数有值 |

### T7. 大封禁量同步（中/大规模可选）

**步骤**：脚本写入 1e4–1e5 条账号/IP 封禁到 Redis，重启或触发全量同步。  
**通过标准**：
- 同步完成时间有日志；CPU/延迟无持续尖刺
- `GET /admin/bans` 条数正确
- 内存增量与条数线性、无泄漏

### T8. 压测叠加（回归）

**步骤**：两台网关 + churn30/分客户端，A/B 各半连接；中途 ban/unban 若干。  
**通过标准**：
- 吞吐相对单机 **两台之和 ≈ 单机 × 2**（允许 ±20%，视 CPU）
- ban 操作期间无全局 failed/dropped 飙升（限流关闭或 maxTokens 足够）
- 4h 短版可先跑 30min 混合 churn（见 Bench.md）

---

## 3. 测试环境与工具

| 项 | 建议 |
| --- | --- |
| 机器 | 至少可跑 2 sgate + 1 logic + redis + etcd；多机更佳（避免本机端口/带宽干扰） |
| 客户端 | 复用 `bench/bench1_tcp`、`churn30`（改 `-addr` 指向不同网关）；或小脚本拨号 |
| 观测 | 两侧 `/stats`、`/admin/bans`、sgate tlog、Redis `PUBSUB CHANNELS`、etcd `get --prefix` |
| 记录 | 每用例：ban 时刻、403 时刻、offline 计数、是否收到 BanNtf |

**Windows 注意**：本机多「源 IP」受限；T4 建议两台物理/虚拟客户端或 `netsh` 辅助，否则 IP 封禁用例在单机 127.0.0.1 上只能验证「踢当前 IP 全部连接」，无法区分实例。

---

## 4. 验收总表

| ID | 用例 | 单实例 | 多实例（仅周期同步） | 多实例（+Pub/Sub） |
| --- | --- | --- | --- | --- |
| T1 | 登录/转发 | 必测 | 必测 | 必测 |
| T2 | 跨实例拒新登录 | N/A | 必测 ≤ syncInterval | 必测 < 2s |
| T3 | 跨实例踢在线 | N/A | N/A 或声明限制 | 必测 |
| T4 | IP 封禁 | 本机简化 | 必测拒登录 | 必测踢+拒 |
| T5 | unban | 必测 | 必测 | 必测 |
| T6 | Redis 宕机降级 | 可选 | 必测 | 必测 |
| T7 | 大同步 | 可选 | 可选 | 可选 |
| T8 | 压测回归 | Bench.md | 30min/4h | 30min/4h |

**理想终态（多实例 + 事件同步）**：
- 任一实例 admin 封禁 → 全集群新登录 403 在 **2s 内**
- 在线连接 **2s 内**收到通知并断开（账号/IP 维度一致）
- Redis 故障可降级、可恢复，无进程崩溃
- 双网关吞吐近似线性叠加

**当前实现基线（无 Pub/Sub）**：
- 新登录：依赖本地缓存回源 Redis → **通常已秒级**（Get 回源）
- 已在线踢连接：**仅 admin 所在实例**；其他实例靠用户重连时 403
- 周期全量同步默认 30s，用于表对齐，不触发踢人

---

## 5. 与设计文档关系

问题清单与演进方案见 [`ban-sharing-design.md`](ban-sharing-design.md)。  
多实例测试应在启用对应阶段功能后更新本表「多实例（+Pub/Sub）」列的必测范围。

## 6. 清理

```powershell
Get-Process -Name sgate,logic1_tcp,logic1_ws,churn30 -ErrorAction SilentlyContinue | Stop-Process -Force
# 可选：清 Redis ban 键
# redis-cli --scan --pattern 'sgate:ban:*' | ForEach-Object { redis-cli del $_ }
```
