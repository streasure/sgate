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

## 封禁共享存储（Redis）

**已实现**：`security.banRedis` 启用后封禁表写穿 Redis（多网关实例共享）。本地内存为热路径缓存；可选 Pub/Sub 事件增量同步（默认开，秒级踢跨实例在线连接）+ 周期全量校准（事件开时 10m，关时 30s）；未封禁负缓存默认 10s。IP 归一化（IPv4-mapped → v4）。默认关闭（仅内存）。IP 封禁键为 `ip:<addr>`。`required: true` 时 Redis 连不上拒绝启动。

**已知限制与方案**（不实施中）：
- NAT/CIDR 封禁、Redis 故障状态机指标导出：见 `docs/ban-sharing-design.md`
- 多实例测试用例与验收标准：见 `docs/multi-sgate-test.md`
