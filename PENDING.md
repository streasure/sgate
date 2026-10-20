# 待处理事项

尚未实施、已知现状与候选方案。已完成项不在此维护。

## 非 Windows 路径验证（mono_other）

**现状**：`obs.MonoNowNs` 在非 Windows 回退 `time.Now`，行为正确但精度受平台限制（无裸 QPC）。开发机仅 Windows，linux 侧未实跑单测。

**候选方案**：
- `GOOS=linux go build ./internal/obs/` 做编译检查
- 本仓 CI 已删除；如重建，加 linux runner 跑 `TestMonoNowNs` / `TestMonoNowNsNoAlloc`
- 或接受现状：非 Windows 精度低但不影响正确性，代码注释已说明

**优先级**：低

## Windows 调度器 osyield 开销

**现状**：CPU profile 中 `runqgrab.osyield` + `lock2.osyield` 合计约 28%，对应多连接每连接读写唤醒的 goroutine 调度成本，属 Windows 平台固有开销。当前吞吐已达标。

**候选方案**：
- 实验分支试 `gnet` 锁 OS 线程 / 减少 goroutine 迁移（`GOMAXPROCS` 调优）
- A/B bench1 对比吞吐与 CPU profile
- 风险高、收益不确定；建议仅在 CPU 成为瓶颈时再做

**优先级**：低

## etcd 依赖与单机模式

**现状**：即使单机 bench 也需 etcd 常驻（服务注册/发现）。

**候选方案**（独立"单机模式"需求）：
- `etcd.endpoints` 为空时跳过注册/发现，logic 地址改静态配置直连
- 改动面大（cluster/discovery 多处），需单独立项

**优先级**：低（部署形态需求，非性能）

## 封禁收敛（已迁移 loginserver）

**已实现（P1+P2，2026-10）**：sgate 不再存封禁（BanStore/banRedis/admin 端点已删，commit `eb4b21c`）。
- P1 单向 Kick 模型：loginserver 权威库（MySQL ACL）落库后 best-effort 下发 KickAccounts/KickIP；
  sgate KickService 纯执行器（x-internal-token 鉴权）按本地 accountID/RemoteHost 索引踢人。
- P2 BatchValidate 周期安全网：sgate 登录后留存 loginKey，每 `loginValidation.batchInterval`
  批量重校验在线会话，踢 mid-login 竞态与会话缓存 TTL 过期的失效会话。

**待做**：反代场景 `trustedProxies`（IP 踢人口径）；多实例一致性压测；NAT/CIDR 封禁。
新方案见 `E:\loginserver\docs\ban-convergence-design.md`。
