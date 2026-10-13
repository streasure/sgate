# Changelog

## 2026-10-13

### Changed

- **限流生产默认值 `DefaultRateLimitMaxTokens` 100 → 10000**
  - 下扫压测（bench1_tcp 100conn）：fwd/s 随 maxTokens 线性扩展，~600K 膝点后满吞吐零丢弃
  - 10000 覆盖 NAT 后多客户端共享 IP 的合理突发（~100 连接 × 100 msg/s），硬顶异常单 IP
  - 全局硬顶 = maxTokens×10，路由上限 = routeMaxTokens（0=maxTokens*4）
  - `config/config.yaml` 同步为 10000 并修复缺失的 `rateLimit:` 键
- **延迟采样步长 8 → 16**：QPC syscall 开销再降一半，62K→31K 样本/s 仍足够百分位
- **延迟环窗 10000 → 32768**：~1s 滑动窗口（采样 16 下），分位数更稳；内存 +~512KB

### Added

- `POST /admin/ratelimit`：热更限流参数（maxTokens/tokenRefresh/routeMaxTokens/globalMaxTokens），Bearer 鉴权，无需重启
- `security.banPersistPath`：封禁 JSON 快照持久化；配置后 ban/unban 写快照，启动自动加载
- WS 缓冲复用与超限拒绝单测（`TestWS_BufferReuseExactBoundary` / `TestWS_BufferLimitRejected` / 粘包/跨读重组）
- Bench.md：限流下扫数据表、bench1_ws/bench2 复验、最终 30min churn 结果

### Fixed

- `config/config.yaml` security.rateLimit 缩进损坏（`rateLimit:` 键缺失导致 YAML 无效）

### Performance

- WS 读缓冲 offset 复用（同 TCP FrameBuf）：bench1_ws 442K→647K（+46%），bench2_ws 759K→1.17M（+55%）
- TCP 帧缓冲 offset 复用 + QPC 零分配 + slog 邮箱唤醒：每消息分配 -37%+，bench1_tcp 暖轮 559-561K 无回退
- 30min churn 复跑 26,089/s 与基线持平，零丢弃零失败
