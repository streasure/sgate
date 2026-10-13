# 待处理事项

> 2026-10-13 从 14 条遗留问题筛出未做项；已完成项见提交记录与 Bench.md。

## 6. mono_other.go 非 Windows 路径验证

**现状**: `obs.MonoNowNs` 非 Windows 回退 `time.Now`，行为正确但精度受平台限制（无裸 QPC）。本机仅 Windows，未在 linux CI 实跑。

**方案**:
- `GOOS=linux go build ./internal/obs/` 编译检查（可立即做）
- CI 加 linux runner 跑 `TestMonoNowNs`/`TestMonoNowNsNoAlloc`
- 或接受现状：非 Windows 精度低但不影响正确性，注释已说明

**优先级**: 低

## 8. runqgrab.osyield ~25% Windows 调度器开销

**现状**: CPU profile 中 `runqgrab.osyield` + `lock2.osyield` 约占 28%，对应 100 连接 × 每连接读写唤醒的 goroutine 调度成本，属 Windows 平台固有。

**方案**:
- 实验分支试 `gnet` 锁 OS 线程 / 减少 goroutine 迁移（`debug.SetMaxProcs` 调优）
- A/B bench1 对比吞吐与 CPU profile
- 风险高、收益不确定；建议仅在 CPU 成为瓶颈时再做

**优先级**: 低（当前吞吐已达标）

## 14. etcd 硬依赖 / 单机模式

**现状**: 即使单机 bench 也需 etcd 常驻（服务注册/发现）。

**方案**: 独立"单机模式"需求：
- `etcd.endpoints` 为空时跳过注册/发现，logic 地址改静态配置直连
- 改动面大（cluster/discovery 多处），单独立项

**优先级**: 低（部署形态需求，非性能）

---

## 已完成（本轮关闭）

| # | 项 | 方式 |
|---|---|---|
| 1 | Bench.md WS 主表基线 | 更新为 647K |
| 2 | bench2 batch-off 复验 | 见 Bench.md 复验表 |
| 3 | logs 清理 | 删除 9 月历史 + 中间 churn；保留 final.jsonl/rl_sweep csv |
| 4 | WS 缓冲超限单测 | `TestWS_BufferLimitRejected` |
| 5 | WS 粘包/跨读单测 | `TestWS_CoalescedMultiFrames` / `TestWS_PartialFrameAcrossReads` |
| 7 | 采样步长 8→16 | `pipeline.go` `&15` |
| 9 | 延迟环窗 10000→32768 | obs_component.go |
| 10 | GOGC/GOMEMLIMIT 重调 | 数据验证后更新默认/注释 |
| 11 | 限流 admin API | `POST /admin/ratelimit` |
| 12 | ban 持久化 | `security.banPersistPath` JSON 快照 |
| 13 | 默认值变更传播 | CHANGELOG.md + 配置注释 |
