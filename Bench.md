# 压测说明

## 总则

- WebSocket 与 TCP 分开压测，WS 优先。
- 客户端 ↔ sgate 消息体为 `protocol.MessageFrame`；sgate ↔ logic 为 `protocol.StreamData`。
- sgate 与 logic 之间为 gRPC 双向流（flip：logic 拨入 sgate，`shardCount` 条流按 session 哈希分片）；同一 session 恒走同一条流（保证有序，压测只需保证 stream 通信性能）。
- bench/logic 严禁做无用回包校验；bench 中除 LoginGate 外的协议 recv 直接丢弃，logic 收到的消息全部丢弃。
- 压测配置禁止登录校验（`loginValidation.enabled: false`）；authfail 出现即为 sgate 登录校验或 bench/logic 误处理，不是预期行为。

## bench1：客户端 → sgate → 逻辑服（上行转发）

单向链路：bench 登录 LoginGate 成功后持续发心跳/数据帧，sgate 转发至 logic 后丢弃。衡量 sgate 纯转发吞吐。

```
Client: Marshal(MessageFrame) + write
sgate:  read → Unmarshal → pipeline → StreamShard → Marshal(StreamData) → gRPC Send
Logic:  gRPC Recv → Unmarshal → 丢弃
```

实现：`bench/bench1_tcp` + `bench/logic1_tcp`，`bench/bench1_ws` + `bench/logic1_ws`。

## bench2：逻辑服 → sgate → 客户端（下行推送）

客户端先登录并加入 logic 分组，logic2 按组持续推送，bench2 对推送 recv 丢弃。衡量 sgate 推送转发吞吐与准确性。

```
Logic:  Recv loginreq → JoinGroup → 按组 PushToConnection → gRPC Send
sgate:  gRPC Recv → Unmarshal → 按连接写出 TCP/WS
Client: read → Unmarshal → 丢弃
```

实现：`bench/bench2_tcp` + `bench/logic2_tcp`，`bench/bench2_ws` + `bench/logic2_ws`。

## 编译

```powershell
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic1_tcp\logic1_tcp.exe .\bench\logic1_tcp
go build -o bench\logic1_ws\logic1_ws.exe .\bench\logic1_ws
go build -o bench\bench1_tcp\bench1_tcp.exe .\bench\bench1_tcp
go build -o bench\bench1_ws\bench1_ws.exe .\bench\bench1_ws
go build -o bench\logic2_tcp\logic2_tcp.exe .\bench\logic2_tcp
go build -o bench\logic2_ws\logic2_ws.exe .\bench\logic2_ws
go build -o bench\bench2_tcp\bench2_tcp.exe .\bench\bench2_tcp
go build -o bench\bench2_ws\bench2_ws.exe .\bench\bench2_ws
go build -o bench\churn30\churn30.exe .\bench\churn30
```

`tools/client_logincheck` 为手动联调工具（会调 loginserver HTTP），**不属于压测程序**，吞吐压测时不要编译/运行。

## 测量条件

- 100 连接、单轮 10s、64B 载荷；登录仅 LoginGate（`loginValidation.enabled: false`）。
- 压测网关用 `config/bench/batch-off.yaml` / `config/bench/batch-on.yaml`：`security.rateLimit.enabled: false`（限流关）、`protection.benchMode: true`（跳过 WS 心跳/连接空闲等后台任务，过载保护 observe-only）。
- 网关：`shardCount=96`、`connGroupCount=4`、`sendChannelSize=65536`、`disableTracer`、`disableMetricsLog`。
- 逻辑服 96 流（`NumCPU*8`）；logic2 `push-size=64`、`push-workers=12`、`expected-members=0`（登录即开推）。
- bench2 用 `config/bench/batch-on.yaml` / `config/bench/batch-off.yaml` 切换 `batchPush: true|false`。
- 绝对数值随后台负载波动，同配置多轮约 ±10%；校验标准：`connections failed=0`、`droppedAuth=0`，日志无 401 / authfail / login-key 拒绝。

## 参考吞吐（本机实测）

原始数据：`bench/latest_results.json`；bench2 逐秒明细见 `logs/bench.log`。

### bench1 上行转发

| 协议 | 总转发量(10s) | 平均速率 | 登录失败 | droppedAuth |
| --- | ---: | ---: | ---: | ---: |
| TCP | 4,565,055 | **456,501/s** | 0 | 0 |
| WebSocket | 6,562,583 | **647,095/s** | 0 | 0 |

### bench2 下行推送

| 协议 | batchPush | 平均接收速率 | 连接失败 | droppedAuth |
| --- | --- | ---: | ---: | ---: |
| TCP | true | **1,277,758/s** | 0 | 0 |
| TCP | false | 428,333 ~ 485,659/s | 0 | 0 |
| WebSocket | true | **1,174,324/s** | 0 | 0 |
| WebSocket | false | 381,512/s | 0 | 0 |

`batchPush=false` 为非合帧逐条写出路径，数值低于合帧路径属预期；主路径以 batch-on 为准。

### 30 分钟混合 churn（bench/churn30）

常驻 150 用户随机突发 + 每分钟 50 新用户波次登录后大突发、5 分钟生存期 churn 关闭；JSONL 逐 10s 结果。

- 配置：`config/bench/rate-limit.yaml`（限流开）+ `config/log/bench.yaml`
- 典型结果：1800s、约 4700 万条、**~26,000 msg/s** 均值；登录全成功、churn 零错误；`dropped*` 全 0；RSS 平台期 ~500MB；goroutines 恒定无泄漏；latency P50≈1.3µs / P99≈6µs

### 4 小时混合 churn 稳定性（bench/churn30 -duration 4h）

常驻 150 + 每分钟 50 波次、5min 生存期；配置 `config/bench/batch-off.yaml` + `config/log/bench.yaml`；结果 `logs/churn4h.jsonl`（10s 采样 ×1441）。

| 指标 | 值 |
| --- | ---: |
| 时长 | 4h（14,410s） |
| 总发送 / 转发 | 399,193,835 条 |
| 平均速率 | **27,721 msg/s**（min 9,997 / max 33,062，波次登录期波动） |
| 登录成功 / 失败 | 8,832,150 / **0** |
| churn 连接 | 8,248,592，write_err=0 / read_closed=0 |
| `/stats` failed / droppedRateLimit | **0 / 0** |
| latency | P50=1.2µs / P99=7.2µs |
| sgate RSS | 平台期 ~520–526MB，结束后 GC 回落 159MB |
| 进程 | 全程存活，无 OOM / 无 goroutine 泄漏迹象 |

结论：4 小时混合负载下吞吐、内存、错误计数均平稳，可作为长稳参考基线。

## 限流 maxTokens 与吞吐关系

per-IP 限额即 `maxTokens`（压测全走 127.0.0.1 时整机共享该额度）；全局硬顶 = `maxTokens×10`，路由上限 = `routeMaxTokens`（0 = `maxTokens×4`）。

bench1_tcp 100 连接下扫（以 `/stats.forwarded` 为准，客户端发送速率含被丢弃消息不可用）：

| maxTokens | fwd/s | droppedRateLimit |
| ---: | ---: | ---: |
| 100 | 120 | ~3.0M |
| 1,000 | 1,180 | ~2.8M |
| 10,000 | 11,980 | ~2.9M |
| 50,000 | 59,980 | ~2.5M |
| 100,000 | 123,231 | ~2.6M |
| 250,000 | 254,911 | ~1.1M |
| 400,000 | 430,481 | ~0.5M |
| 600,000 | 478,586 | **0** |
| ≥ 800,000 | ~440K–480K | **0** |

**读法**：fwd/s 随 maxTokens 线性扩展至膝点 ~600K（单 IP 独占场景的机器容量上限），之后满吞吐零丢弃。生产默认 `maxTokens=10000` 覆盖 NAT 后多客户端共享 IP 的合理突发（~100 连接 × 100 msg/s），同时硬顶异常单 IP；高吞吐部署按需上调。

## 性能相关实现说明

### 延迟采样与计时

- 消息管道延迟用裸 QPC 单调钟（`obs.MonoNowNs`，Windows；非 Windows 回退 `time.Now`）。Go `time.Now` 粒度 ~1.2µs，会把亚微秒管道延迟截成 0。
- 每 16 条消息采样 1 条计时（QPC 为 syscall ~1µs，全量计时在 500K msg/s 下烧 ~1 核；31K 样本/s 对百分位足够）。
- `/stats` 的 latency 环形窗 32768 样本（采样 16 下约 1s 窗口）。

### CPU / 过载指标

- Windows 进程 CPU 时间量化 ~15.6ms，单点采样频繁 delta=0；`cpuPercent` 用 EMA 平滑（~1s 时间常数）。空闲时为 0 是正确行为。
- `protection.benchMode: true` 时过载保护为 observe-only：采样 cpu/mem 供 `/stats` 展示，不置过载标志、不丢消息。
- 转发失败计入 `/stats.failed`，不计入 `droppedTotal`（后者专指 overload 队列满等网关主动丢弃）。

### 读缓冲复用

- TCP：`ConnContext.FrameBuf` + `frameOff` 偏移，消费完 `[:0]` 保留容量，残留下次读事件前移；append 热路径零重分配。
- WebSocket：`WebSocketConnection.Buffer` + `bufOff` 同理；分片重组 `fragBuf` 同样保留容量。
- 超限（`maxFrameBufSize` / `maxWSBufferSize`）断连，防无界增长。

### 内存与 GC

- `perf.gcPercent: 300`、`perf.memoryLimitPercent: 80`（GOMEMLIMIT 为总内存百分比）。
- 短时冷启动下 GOGC 300–600 吞吐差异落在噪声内；长稳场景若 GC 成瓶颈再调。
- 内存分配归因：`SGATE_MEM_PROFILE_RATE=4096`（默认 512KB 采样率会把每消息小分配全采样掉，profile 只剩启动期）。

### 限流热更

- `POST /admin/ratelimit`（Bearer 鉴权，token 来自 `admin.token`）：body `{"maxTokens":N,"tokenRefresh":"1s","routeMaxTokens":N,"globalMaxTokens":N}`，缺省 0/空表示恢复默认派生值。
- 也可通过配置中心热更，走同一 `handleConfigUpdate` 路径。

### 封禁（已迁移 loginserver）

- sgate 不再存封禁（BanStore/banRedis 已删）。封禁权威库在 loginserver（MySQL ACL），
  落库后 best-effort 下发 KickAccounts/KickIP 至各 sgate；sgate 按本地索引踢人。
- P2 BatchValidate 周期安全网（`loginValidation.batchInterval`）批量重校验在线会话，
  兜底 mid-login 竞态与会话缓存 TTL 过期。压测配置必须 `loginValidation.enabled: false`。

## etcd key 约定

`/services/{belong}/{ServerTypeName}:{zone}/{instanceId}`（如 `SERVER_TYPE_SGATE` / `SERVER_TYPE_LOGINSERVER` / `SERVER_TYPE_LOGICSERVER`）。

## 停止所有压测进程

```powershell
Get-Process -Name sgate,logic1_tcp,logic1_ws,logic2_tcp,logic2_ws,bench1_tcp,bench1_ws,bench2_tcp,bench2_ws,churn30 -ErrorAction SilentlyContinue | Stop-Process -Force
```
