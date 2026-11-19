# sgate 待办（受外部条件阻塞项）

本文件只记录**当前无法在本机/本仓处理**的事项；可在本仓直接推进的工程项见
[`docs/roadmap.md`](docs/roadmap.md) 待办清单，压测数据与结论见
[`docs/benchmark-report.md`](docs/benchmark-report.md)。

状态标记：`[ ]` 未开始 · `[~]` 工具/方案已具备，等条件 · `[x]` 已完成（保留作索引）

---

## 1. 需要 Linux 环境（与 roadmap P2-4 打包进行）

- [ ] **UDP 阶段 4 正式压测数据**：关联建立 QPS、上行 msg/s、下行 stateVer 时延分位数、
      与 WS 基线对比 + 弱网模拟（丢包/乱序/延迟抖动）。
      - 已具备：`bench/udputil` + `bench/udp_uplink` + `bench/udp_downlink` + `bench/logic_udp`
        + `config/bench/udp-on.yaml`（三方对账口径已验证，见 benchmark-report「UDP 阶段 4」）。
      - 阻塞原因：Windows 入口天花板 ≈280~300K datagrams/s（gnet Windows UDP 单 reader
        goroutine，`acceptor_windows.go`），sync/async A/B 已归因，Linux 多 reader 未验证。
- [ ] **Linux CI 与压测基线**：构建 + 吞吐/连接数回归阈值固化；依赖方向检查挂
      `scripts/check-deps.sh`（sgate 平面边界 + 跨仓零反向，已 ALL PASS）、
      跨仓冒烟挂 `scripts/smoke-logicserver.ps1`（需四兄弟仓 checkout + etcd/MySQL/Redis 服务）。
      Windows 连接天花板 16,384（临时端口池），百万连接与 epoll 差异无法本机复现。
- [ ] **网关纯上限钉死**：多机部署（网关独占 CPU + 真实网卡）。
      本机四轮 + 六项复验一致证明瓶颈在压测工具两端（CPU ≤3.8/12 核）。

## 2. 需要决策 / 排期（本仓可做，尚未实施）

- [x] **下行写路径背压上限**（已完成，本仓实现）：`Connection.AppendCoalesced` 无界 append +
      AsyncWrite 队列无上限是场景 C OOM 事故根因（12K 无读连接即打爆系统提交量）。
      已按裁定落地「**先延迟后丢弃 + 每连接/全局两级上限**」：单连接在途写 2MB、
      全局安全阀 1GB（按活跃连接数公平份额约束，慢连接不饿死健康连接）、
      合批缓冲硬顶 256KB（64KB 经实机标定偏小：flush 节奏 ~10ms 下停摆/超订
      会先于在途预算打到顶，健康连接实测丢弃 2~4%，故上调）；flush 超限只延迟（缓冲保留、重标脏下轮重试），直发超限
      返回 `ErrWriteBackpressure`，丢弃计数 `/stats pushDroppedBackpressure`、
      在途字节 gauge `pendingWriteBytes`。测试 `internal/dataplane/backpressure_test.go`。
      实机验证（2026-11-18，batch-off）：①读客户端 100 连接 10s——稳态 6.3M 消息
      零丢弃，仅登录窗口超订丢 46,434/9.77M=0.47%（64KB 时 3.5~4% 且稳态持续丢）；
      ②无读场景 200 连接 35s（OOM 主场景缩比）——`pendingWriteBytes` 封顶
      418,561,114≈200×2MB、缓冲触顶后丢弃持续计数、关闭后归零无泄漏、`/stats`
      全程响应、无 OOM；③heap 归因——峰值 WS 5GB 中 Go 活堆 ≤1.1GB（含预算内
      pending ≤419MB），其余为 ~1GB/s 分配 churn（累计 31GB：proto 编解码、
      WS reframe、`fmt.Sprintf`）的堆页保留/GC headroom（既有编码路径行为，
      测试结束活堆回落 117MB、WS 758MB），写路径预算已封顶。
- [~] **场景 D 关闭风暴期 `/stats` 采样超时**（基线既有现象，本轮复验未复现采样超时，
      但关闭期写失败计数 `asyncWriteFailed` 仍存在）：与上一条同源，背压已随上一条
      落地，**待复验**（重跑场景 D 确认关闭期不再积压）。
- [x] **logicserver 实施（阶段 1~5 全部完成）**：骨架方案见
      [`docs/logicserver-design.md`](docs/logicserver-design.md)（独立服务、实现在 `E:\logic`
      仓、SDK+服务同仓、`replace` 仍不切 `require`）。已完成：
      阶段 1（logic 仓 `8ea8750`）`cmd/logicserver` + `internal/config`（显式传参）+
      `internal/handler`（LoginGate 入组 + LoginReq/LoginAck 骨架应答）+ `config/*.yaml`；
      阶段 2（logic 仓 `68d6b59`）配置 4 件套压测侧 + `scripts/` + `docs/architecture.md`
      + README 运行说明；阶段 3（logic 仓 `7050dc1`）`internal/metrics`
      （/counters+/healthz+/readyz）+ `internal/probe` readiness + handler drain 门 +
      组合根 Start/信号/drainWait 优雅停机 + `ports.metricsAddr`/`drainGrace` 配置；
      阶段 4（sgate）`scripts/smoke-logicserver.ps1` 四进程冒烟（两轮 12/12 断言 PASS）+
      `scripts/check-deps.sh` 依赖方向检查（ALL PASS）+ §3.6 三处文档配套
      （architecture.md §2.7、logic-guide.md §8.1、Bench.md bench/logic_* 定位）；
      阶段 5（logic 仓 `d6ffe60` + tag `v0.1.0`）发版治理三件套（详见下条）。
      五阶段验收均过（两仓五门禁绿、etcd 注册/flip/bench1 端到端、常规与压测配置
      均可起、探针三态、batch-off 对账闭合、156ms 优雅停机 + etcd 注销、
      四进程冒烟一键过、apidiff 五路径实测）。对账口径：batch-on 配置合批缓冲未刷
      不计 pushedToClient，对账须用 batch-off。
- [~] **logic SDK 版本治理**（阶段 5 已落地，切换待触发）：logic 仓 `d6ffe60` +
      annotated tag `v0.1.0`（本地未推送）——`CHANGELOG.md` 基线条目、
      `scripts/apidiff.sh` 导出面兼容性门禁（go-apidiff，基准=最近 tag → HEAD，
      五路径实测：无指引/相等 PASS/仅 additive PASS/破坏 FAIL/默认 PASS）、
      logic README「版本治理」段（0.x bump 规则、tag 流程、`replace => require`
      切换时机）。`replace => ../logic` 暂不切 `require`，触发条件 = tag 推远端 +
      第二个仓外消费者；改 SDK 仍须同轮跑 sgate 五门禁。
- [~] **跨仓集成冒烟基线**（脚本已落地，待 Linux CI 接入）：`scripts/smoke-logicserver.ps1`
      四进程对账冒烟已实跑通过（loginserver → gateway → logicserver → bench1_ws，
      12 断言 PASS、版本钉扎 `logs/smoke_versions.txt`），本机手动一键可用；
      余下是接入 Linux CI（与下方 P2-4 打包）+ 各兄弟仓钉同一批 commit 的自动化。
- [ ] **loginserver 包级 `GetConfig` 改显式传参**：**仅当出现实际痛点**（如测试需注入不同
      配置）才动，不主动排期（童子军军规，见 logicserver-design §7.3）；新仓 logicserver
      一律显式传参。

## 3. 需要外部仓（客户端仓不在本机）

- [ ] **真实客户端仓对接 #4**：
      - 可靠消息端到端 ACK + 滑动窗口去重（`CmdMsgAck`，阶段 1~4 已完成于 bench 侧：
        `bench/logic_reliable` + `bench/client_ack`，见 roadmap P2-1）；
      - UDP 关联通道客户端实现（登录取 `udp_token` → ASSOCIATE → 数据报收发 + 保活）。
      - 客户端仓落地时按 `docs/client-guide.md` 对齐。

## 4. 需要外部环境（多 zone / 真实网络）

- [ ] **封禁收敛遗留**：多 zone 部署下的跨 zone 一致性验证；NAT 场景踢人口径
      （roadmap P2-2）。
- [ ] **P3 系列**（详见 roadmap P3）：多 Zone 支持、滚动升级连接迁移、
      跨服战消息路由、断线重连状态保持；P3-1 阶段 3（跨 zone 映射存储）延后随 P3-2。
