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
- [ ] **Linux CI 与压测基线**：构建 + 吞吐/连接数回归阈值固化。
      Windows 连接天花板 16,384（临时端口池），百万连接与 epoll 差异无法本机复现。
- [ ] **网关纯上限钉死**：多机部署（网关独占 CPU + 真实网卡）。
      本机四轮 + 六项复验一致证明瓶颈在压测工具两端（CPU ≤3.8/12 核）。

## 2. 需要决策 / 排期（本仓可做，尚未实施）

- [ ] **下行写路径背压上限**：`Connection.AppendCoalesced` 无界 append + AsyncWrite 队列无上限。
      场景 C OOM 事故根因（12K 无读连接即打爆系统提交量）；生产中慢消费者目前只靠
      gRPC 流控窗口 + 内核 socket 缓冲自然限界。修复建议见 benchmark-report
      「满载复验 → 遗留修复建议 1」，**待决定是否排期**。
- [ ] **场景 D 关闭风暴期 `/stats` 采样超时**（基线既有现象，本轮复验未复现采样超时，
      但关闭期写失败计数 `asyncWriteFailed` 仍存在）：与上一条同源，随背压方案一并处理。
- [ ] **logicserver 实施（阶段 1~4）**：骨架方案已定稿
      [`docs/logicserver-design.md`](docs/logicserver-design.md)（独立服务、实现在 `E:\logic`
      仓、SDK+服务同仓、replace 不打 tag）；待排期开工。
- [ ] **logic SDK 版本治理**：tag + changelog、`replace => ../logic` 切 `require` 的时机
      （方案阶段 5 前置）；当前决策是不打 tag 走 replace，改 SDK 必须同轮跑 sgate 五门禁。
- [ ] **跨仓集成冒烟基线**：`sgate + loginserver + logicserver + bench` 四进程对账冒烟，
      checkout 各兄弟仓钉同一批 commit；与下方 Linux CI（P2-4）打包推进。
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
