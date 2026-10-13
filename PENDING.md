# 待处理事项

尚未实施、已知现状与候选方案。已完成项不在此维护。

## 非 Windows 路径验证（mono_other）

**现状**：`obs.MonoNowNs` 在非 Windows 回退 `time.Now`，行为正确但精度受平台限制（无裸 QPC）。开发机仅 Windows，linux 侧未实跑单测。

**候选方案**：
- `GOOS=linux go build ./internal/obs/` 做编译检查
- CI 加 linux runner 跑 `TestMonoNowNs` / `TestMonoNowNsNoAlloc`
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

## 封禁多网关共享

**现状**：封禁为本地 JSON 快照（`security.banPersistPath`），单实例重启可恢复，多网关实例之间不共享。

**候选方案**：接 MySQL 或 etcd KV，启动加载 + 变更同步；过期清理与现有 `BanStore.Expired` 逻辑对齐。

**优先级**：中（多实例部署前需要）
