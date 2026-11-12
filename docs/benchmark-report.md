# sgate 压测报告

## 测试环境

| 项目 | 值 |
|------|-----|
| 操作系统 | Windows 10/11 (win32) |
| CPU | Intel i5-10400F 6C/12T |
| 内存 | 32GB |
| 网卡 | 1Gbps |
| Go 版本 | go1.26 |
| gnet 版本 | v2.9.7 |

## 测试限制

**Windows 平台无法创建超过 ~16K 本地连接**，原因是：
- Windows 临时端口范围默认 49152-65535（共 16384 个）
- 每个 `net.Dial` 创建新 socket 需要一个临时端口
- 无论连接多少个目标端口，总连接数受限于临时端口数
- 扩大端口范围需要管理员权限：`netsh int ipv4 set dynamicport tcp start=1024 num=64511`
- 真正的百万连接测试需要 **Linux 环境**（epoll 原生支持）或**管理员权限**

## 工程要点（现状）

- **热路径无调试 I/O**：WebSocket 帧调试（`wsDebug`）已从网络事件热路径移除，调试信息统一走 `tlog` 结构化日志；文件 I/O + `fmt.Fprintln` 阻塞与 goroutine 栈累积问题不复存在（内存稳定在数十 MB 量级，见「内存占用分析」）。
- **gnet Windows 事件循环模型**：Windows 上每连接一个 goroutine（gnet `eventloop_windows.go` 模拟 epoll 所致，非网关代码问题）；Linux（原生 epoll）无此开销，Windows 上仅影响工作集指标，不影响功能。
- **连接级流控**：`connection.Connection` 上的 `msgCount`/`msgWindowStart` + `CheckAndIncrementMsgRate()` 实现每秒窗口计数，pipeline 处理阶段 2.5 检查；配置 `protection.maxMessagesPerConn`（0=不限制）。
- **连接生命周期指标**：复用 `obs.LatencyTracker`（滑动窗口 10000 样本），`OnClose` 时记录连接存活时长，`/stats` 返回 `connectionDurationP50Ms`/`P95Ms`/`P99Ms`。
- **热配置更新**：`configWatcher` 监听文件变化，`handleConfigUpdate` 动态更新以下参数：

| 参数 | 热更新支持 |
|------|-----------|
| 限流阈值 (`rateLimiter.UpdateRate`) | ✅ |
| 黑白名单 | ✅ |
| 过载保护阈值 | ✅ |
| JWT 密钥 | ✅ |
| 灰度规则 | ✅ |
| 流量镜像比例 | ✅ |
| 降级规则 | ✅ |
| 连接限制 (`maxConnections`/`maxConnectionsPerIP`) | ✅ |
| 连接级流控 (`maxMessagesPerConn`) | ✅ |

## 压测结果

### 测量条件

100 连接、10 秒、64B 推送载荷、12 推送协程、`shardCount=96`、`connGroupCount=4`、`sendChannelSize=65536`、`expected-members=0`（登录即开推）、`loginValidation.enabled: false`。登录仅走 LoginGate（cmd 1000001），无 HTTP login、无 token 校验。bench2 通过 `config/bench/batch-on.yaml` / `config/bench/batch-off.yaml` 切换 `batchPush`。

### 当前实测

| 轮次 | 方向 | 协议 | 平均速率 | 失败 | droppedAuth |
|------|------|------|----------|------|-------------|
| bench1_tcp | client→sgate→logic | TCP | **456,501 msg/s** | 0 | 0 |
| bench1_ws | client→sgate→logic | WebSocket | **647,095 msg/s** | 0 | 0 |
| bench2_tcp（batch on） | logic→sgate→client | TCP | **1,277,758 msg/s** | 0 | 0 |
| bench2_ws（batch on） | logic→sgate→client | WebSocket | **1,174,324 msg/s** | 0 | 0 |
| bench2_tcp（batch off） | logic→sgate→client | TCP | 428,333 ~ 485,659 msg/s | 0 | 0 |
| bench2_ws（batch off） | logic→sgate→client | WebSocket | 381,512 msg/s | 0 | 0 |

**校验**：各轮 `connections failed=0` / `droppedAuth=0`，日志无 401 / authfail / login-key 拒绝。

**负载说明**：绝对数值受本机后台应用影响，同配置多轮约 ±10%。`batchPush=false` 为非合帧路径，数值低于 batch-on 属预期。对比吞吐时应以同批测量内部对比为准。限流下扫表与 30min churn 结果见 [`Bench.md`](../Bench.md)。

### 16K 极限压测（2026-11，`config/bench/batch-on.yaml`）

三场景分别逼近 Windows 单机可测的连接数与消息速率上限（bench 规则：`loginValidation.enabled: false`、仅 LoginGate、无 HTTP login）。

#### 场景 A：连接极限（bench_1m，TCP，目标 16384）

| 指标 | 结果 |
|------|------|
| 建连成功 | 16,095 / 16,384 与 16,071 / 16,384（两轮） |
| 失败原因 | 289~313，全部为尾批 `bind: ...lacked sufficient buffer space or queue was full`——Windows 临时端口池（49152 起共 16384 个）耗尽，**非网关拒绝** |
| 建连+登录速率 | **6,583 ~ 6,605 conn/s**（2.44s 建完 1.6 万连接并全部登录） |
| 60s 保持期 | active=16,071 **零掉线**、droppedTotal=0 |
| 保持期内存 | RSS 1,341 → 1,342MB 稳定（每连接约 75KB，进程基线 124MB）、alloc 1,162 → 1,164MB |
| 保持期 goroutine | 16,364（≈ 每连接 1 个 + 300 基线，Windows gnet 每连接一 goroutine） |
| 保持期 CPU | ≈ 0.3%（心跳/batch 校验被 benchMode 跳过） |

**结论**：Windows 下 16K 即单机连接天花板（端口池硬限制）；网关侧 16K 保持零资源漂移。更大规模需 Linux（epoll）。

#### 场景 B：15K 连接 WS 上行（bench1_ws）

| 指标 | 结果 |
|------|------|
| 登录 | 15,000 / 15,000 零失败（批量并发登录，`-login-batch 100`） |
| 10s 窗口吞吐 | **724,469 msg/s** 与 **759,359 msg/s**（client-side 发送口径，两轮） |
| 网关侧对账 | `forwarded` 与客户端 `sent` 逐条对上；`failed=0`、`dropped*=0`、`asyncWriteFailed=0` |
| 管道延迟（洪峰期 ring 采样） | P50 **0.5µs** / P95 0.8µs / P99 1.2µs / max 9.5ms |
| 洪峰期网关 CPU | ≈ 3 核（10s 窗口累计 CPU 32s） |
| 连接时长 | P50 17.4s / P95 18.0s |

#### 场景 C：15K 连接 WS 下行（bench2_ws + logic2-ws，96 流，batch on）

三方对账（消息条数口径，`pushedToClient` 按逻辑条数计）：

| 侧 | 计数 | 说明 |
|----|------|------|
| logic 发出 | **22,754,772** | 推送窗口 10:51:50.5 → 10:52:26.5（36.1s），≈ **630K msg/s** |
| 网关分发 | **22,635,259** | `pushedToClient`，占发出的 99.48% |
| 差值 | 119,513 | `pushDroppedNoConn` 113,029 + Send/marshal 失败 6,484，**精确闭合、全部如实计数** |
| 客户端读走 | **17,024,058** | 30s 读取窗口；差值 5,611,201 为客户端到点主动 `Close` 后的在途/队列积压（非网关丢失） |

其他指标：

- **零业务丢弃**：`droppedTotal=0`、`failed=0`（113,029 条 `pushDroppedNoConn` 全部产生于客户端主动关闭之后，属正常拒绝而非过载丢弃）
- **连接稳定性**：推送期间 logic 侧 members 恒为 15,000，直到客户端 30s 到点关闭才开始递减（中途采样 `active=11971` 为关闭进行中的中间态）
- **洪峰 CPU**：C 轮网关累计 129.9s，扣除登录与空闲 ≈ **2.5~3 核**（与 B 轮上行洪峰一致）
- **内存画像**：洪峰期 RSS 峰值 **6,582MB**（Go heap `sys=6,605MB` 向 OS 已提交未归还 + 下行背压在途积压），真实活跃堆 `alloc=809MB`；推送结束、积压排空后 RSS 回落 **467MB**、alloc 207MB
- **下行速率瓶颈**：logic 侧 630K/s 为网关分发能力；客户端实际读取 ≈ 471K msg/s（17.0M / 36.1s），差距来自下行背压下管道积压（关闭时刻在途约 3.7M 条）

**结论**：单机 15K 连接下行洪峰下网关分发 **≈630K msg/s**、上行 **≈750K msg/s**，业务零丢弃、对账分毫不差；下行内存峰值受客户端读取速度制约（背压积压），生产中客户端慢读会以队列积压而非丢消息体现。

### 多 TCP 连接并行（connGroupCount）

gateway 与 logic 之间的 gRPC stream 若共享单条 TCP 连接，HTTP/2 协议要求同一连接上的 stream 共享写锁，96 个 shard 会串行写入（实际并行度 1）。`stream.connGroupCount` 配置 gateway 对同一 logic 建立 N 条独立 TCP 连接（默认 4），每个 connGroup 承载 `shardCount/N` 个 stream、各自拥有独立 HTTP/2 写锁。当前配置默认 `connGroupCount: 4`。

### 连接稳定性（5 轮循环测试）

| 轮次 | 连接前内存 | 峰值内存 | 销毁后内存 | 堆 Inuse | 活跃连接 |
|------|-----------|---------|-----------|----------|---------|
| 1 | 39MB | 886MB | 883MB | 42.7MB | 7,829 |
| 2 | 883MB | 1,583MB | 1,580MB | 65.7MB | 8,268 |
| 3 | 1,580MB | 1,657MB | 1,564MB | 71.2MB | 8,334 |
| 4 | 1,654MB | 1,778MB | 1,777MB | 71.2MB | 7,913 |
| 5 | 1,777MB | 1,679MB | 1,216MB | 31.2MB | 7,907 |

**关键结论**：
- 堆内存稳定 31-71MB，无泄漏 ✅
- 工作集增长是 Go 虚拟内存保留行为（GC 后从 1.3GB 降到 671MB）⚠️
- 连接建立/销毁正常，无泄漏 ✅
- 每轮稳定 ~8K 活跃连接 ✅

### 内存占用分析

| 组件 | 内存占用 | 说明 |
|------|---------|------|
| 堆 inuse | 50MB | 实际使用 |
| gnet accept 缓冲区 | 31MB | 481 个对象 |
| StreamManager | 15MB | 26 个分片 |
| logger 缓冲区 | 2MB | treasure-slog |
| goroutine 栈 | ~500MB | 770 个 goroutine（Windows gnet） |
| Go 虚拟内存保留 | ~800MB | 不归还 OS（正常行为） |

## 监控数据说明

sgate 所有监控数据通过以下方式输出（无标准输出）：

1. **结构化日志**：`tlog.Info(ctx, "gateway metrics ...", ...)` 每秒输出一次
2. **HTTP API**：`/stats` 端点返回 JSON 格式指标
3. **pprof**：`/debug/pprof/` 提供 Go 运行时分析

### /stats 端点新增字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `connectionDurationP50Ms` | float64 | 连接存活时长 P50（毫秒） |
| `connectionDurationP95Ms` | float64 | 连接存活时长 P95（毫秒） |
| `connectionDurationP99Ms` | float64 | 连接存活时长 P99（毫秒） |

## 结论

| 项目 | 状态 |
|------|------|
| 堆内存泄漏 | ✅ 无 |
| 连接泄漏 | ✅ 无 |
| goroutine 泄漏 | ✅ 无（热路径无调试 I/O） |
| 消息吞吐量（TCP，100 连接） | ✅ bench1 **456,501 msg/s** / bench2(batch on) **1,277,758 msg/s** |
| 消息吞吐量（WS，100 连接） | ✅ bench1 **647,095 msg/s** / bench2(batch on) **1,174,324 msg/s** |
| 多 TCP 连接并行 | ✅ `connGroupCount` 配置，默认 4 连接 |
| 登录/鉴权 | ✅ 各轮 0 失败、droppedAuth=0、无 401/authfail |
| 连接级流控 | ✅ 已实现 |
| 连接时长分位数 | ✅ 已实现 |
| 热配置更新 | ✅ 已实现 |
| 0 连接失败 | ✅ |
| Windows 百万连接 | ❌ 受临时端口限制 |
| Linux 百万连接 | 待测（需要 epoll 环境） |

**下一步**：在 Linux 环境或管理员权限下进行真正的百万连接测试。

## etcd 注册地址格式

`discovery.registerSelf: true` 时网关向 etcd 注册网关连接信息，格式为 JSON：

```json
{
  "ip": "192.168.1.100",
  "grpc": 50051,
  "tcp": "192.168.1.100:48080",
  "websocket": "192.168.1.100:48081"
}
```

- etcd key: `/services/{belong}/SERVER_TYPE_SGATE:{zone}/{instanceId}`（protocol 枚举）
- lease: 自动续期，TTL 默认 10s
- loginserver 通过 etcd watch 前缀 `/services/{belong}/SERVER_TYPE_SGATE:` 获取网关连接地址
