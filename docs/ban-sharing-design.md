# 封禁共享问题方案（已废弃 2026-10-16）

> **本文档描述的 BanStore/banRedis/Pub/Sub 封禁模型已在封禁收敛 P1 整体删除**
> （commit `eb4b21c`）。sgate 不再存封禁；封禁权威库在 loginserver（MySQL ACL 快照），
> sgate 仅作 KickService 纯执行器（loginserver 单向下发 KickAccounts/KickIP），
> 另有 BatchValidateLoginTokens 周期安全网（P2）收口 mid-login 竞态与会话缓存 TTL 过期。
>
> 新方案见 `E:\loginserver\docs\ban-convergence-design.md`（单向 Kick 模型 v2）。
>
> 以下内容仅作历史设计参考，不再反映代码现状。

针对单/多实例、大/中/小规模给出建议路径。默认**不改现有行为**，按阶段启用。

## 规模与形态假设

| 形态 | 实例数 | 封禁量级 | 在线连接 | 说明 |
| --- | ---: | --- | --- | --- |
| 单机小 | 1 | < 1k | < 50k | 本地内存即可，Redis 可选 |
| 单机中 | 1 | 1k–10w | 50k–500k | Redis 推荐（重启不丢） |
| 多实例小 | 2–4 | < 10k | 各实例 < 100k | Redis + 缩短同步 + Pub/Sub |
| 多实例中/大 | 5+ | 10k+ | 百万级 | Redis 哨兵/Cluster + 增量同步 + 网段封禁 |

---

## 问题 → 方案

### P1. 热路径延迟（登录同步路径打 Redis）

| 方案 | 适用 | 说明 |
| --- | --- | --- |
| A. 本地 LRU + TTL 缓存（现状弱化版） | 单/多实例通用 | 命中本地；未命中回源 Redis 并缓存 5–30s。单机多封禁查询几乎零 RTT |
| B. 登录路径仅查本地，依赖 sync | 多实例小规模 | 延迟最低；一致性窗口 = syncInterval（默认 30s，可调 5s） |
| C. 布隆/位图 IP 黑名单常驻内存 | 多实例大 IP 封禁 | 全量 IP 集合 < 10MB 时每连接 O(1) 位查，零 Redis 依赖 |

**建议**：默认走 A（本地缓存 + 回源）；多实例把 `syncInterval` 降到 5–10s 作为兜底；超大 IP 封禁量再上 C。

### P2. 多实例一致性窗口 / 跨实例踢人

| 方案 | 一致性 | 复杂度 | 说明 |
| --- | --- | --- | --- |
| A. Redis Pub/Sub 广播 | 秒级 | 中 | ban/unban 后 `PUBLISH sgate:ban:events`；各实例订阅 → 本地踢连接 + 刷表 |
| B. 共享 Redis Stream / List 事件 | 秒级+可回放 | 中高 | 多实例或重启后可补拉事件；比 Pub/Sub 可靠 |
| C. 周期同步 + 二次登录拦截（现状） | syncInterval + 下次登录 | 低 | 不踢已在线；实现成本最低 |
| D. 走 GatewayClientPool gRPC KickSession | 秒级、点对点 | 高 | 需全局 session→gateway 表（roadmap P3）；适合定向踢，不适合批量 |

**建议**：
- 多实例必须至少做 **A**：事件驱动踢连接 + 刷 BanStore，`syncInterval` 保留作全量校准（防丢消息）。
- 事件字段：`{op: ban|unban, kind: user|ip, key, reason, ttl, ts, originInstanceId}`。
- 订阅方处理：更新本地表 → `enforceBan`/`enforceBanIP`（只踢匹配连接）→ 指标 +1。
- 广播失败降级：依赖周期全量同步，打 warn 指标。

### P3. 全量 SCAN+MGET 成本（大封禁量）

| 方案 | 适用 | 说明 |
| --- | --- | --- |
| A. 增量同步（基于事件/版本号） | 中/大 | 正常路径只靠 Pub/Sub；全量仅冷启动 + 定时（如 5min）校准 |
| B. Redis 侧维护 SET/Hash 分页 | 大 | `HGETALL` 分批或 `SCAN` count 调大；避免一次 MGET 上万 key |
| C. 本地位图/CIDR 树 | 大 IP 封禁 | 见 P1-C；账号封禁仍走 map |

**建议**：默认 A（事件 + 低频全量）；封禁 > 1w 时全量间隔拉长到 5–15min。

### P4. IP 粒度（NAT / CIDR / 归一化）

| 方案 | 说明 |
| --- | --- |
| A. 规范化存储 | 写入前 `net.ParseIP().String()` 归一（IPv6 压缩形式统一）；查询同样归一 |
| B. CIDR 支持 | `ip` 字段允许 `10.0.0.0/8`；内存用前缀树/掩码匹配；Redis 存完整 CIDR 字符串 |
| C. 与 `security.blacklist` 合并热更 | IP 封禁事件同步写入 WhitelistBlacklist（pipeline 层也拦，不只登录） |
| D. NAT 后运维约定 | 文档注明：出口 IP 共享时禁用粗暴 IP 封禁，改账号封禁或源 IP 白名单 |

**建议**：多实例/生产至少做 **A+B**；C 作为纵深防御（已建立连接在 pipeline 也被丢弃）。

### P5. 查询语义（裸 IP 查不到）

| 方案 | 说明 |
| --- | --- |
| A. API 层统一 | `GET /admin/bans` 返回带 `kind`；文档写明 IP 键为 `ip:<addr>` |
| B. Get 自动加前缀 | `Get(ip)` 若含 `.` 或 `:` 且非账号格式，尝试 `ip:` 键（有误伤账号名风险，不推荐） |

**建议**：只做 A + 文档；代码不猜键类型。

### P6. Redis 故障与降级

| 方案 | 说明 |
| --- | --- |
| A. 明确降级状态机 | `redisUp/redisDown`；Down 时：读走本地、写仅本地 + 错误指标；恢复后全量 Sync |
| B. 启动 fail-fast 可选 | `banRedis.required: true` 时连不上拒绝启动（生产多实例建议 true） |
| C. 断线重连 | go-redis 自带；额外暴露 `ban_redis_up` gauge + 同步失败计数 |

**建议**：默认告警降级（现状）；多实例生产开 **B**；补 **C** 指标。

### P7. 历史 JSON 快照迁移

| 方案 | 说明 |
| --- | --- |
| A. 一次性导入工具 | `tools/ban_migrate`：读旧 `bans.json` → 批量写 Redis |
| B. 启动兼容读 | 配置 `banMigrateFrom: path`，首次启动导入后清空该配置（一次性语义） |

**建议**：若生产从未用过 JSON 快照则跳过；用过则做 A（离线工具，不进热路径）。

---

## 推荐落地顺序（不实施，仅排序）

| 阶段 | 内容 | 覆盖问题 |
| --- | --- | --- |
| 1 | IP 归一化 + CIDR + 文档 | P4, P5 |
| 2 | Redis 状态机/指标 + required | P6 |
| 3 | Pub/Sub 事件踢连接 + 增量同步 | P2, P3 |
| 4 | 大量 IP 位图/分层结构 | P1, P3（大规模才做） |
| 5 | 跨实例 KickSession（需全局 session 表） | P2 定向场景（roadmap P3） |

单实例/小规模做完阶段 1–2 即可；多实例中大型至少阶段 1–3。

---

## 与现有多 sgate 架构的衔接

- logic → gateway：按连接粘在 stream 上，**不需要**为封禁改路由。
- GatewayClientPool 已能连对端但无 Kick 语义；定向踢依赖全局 session 表（roadmap P3-1），**封禁广播优先走 Redis 事件，不依赖它**。
- Leader 选举目前是占位，封禁**不要**绑 Leader；任意实例收到 admin API 后写 Redis + Pub/Sub 即可。
- 维护模式仍是 per-instance；若要全网维护，复用同一事件通道（后续扩展 `op: maintenance`）。
