压测流程
websocket和tcp都需要压测。websocket优先级更高。这两个分开压测。
删除原有的bench和logic的设计和相关代码文件配置。
严禁在bench和logic做无用的什么回包校验。还有不可能存在authfail，存在就是sgate那边的登录校验代码错误。或者在bench和logic做了不该做的数据处理。
其中客户端和sgate的收发消息体都为E:protocol定义的MessageFrame
而sgate和logic之间的交互消息全是E:protocol定义的StreamData

sgate和logic之间是建立的stream流式通信。所有的client消息全走这个一个流，最后再通过userUuid去区分（这一步不用实现，只需要保证stream的通信性能即可）

压测顺序：先WebSocket，再TCP。

## bench1：客户端→sgate→逻辑服 转发压测

只测试websocket和tcp这一条链路的单向通信。
其中bench只负责logingatereq正常即可，其他的所有协议一律忽视回包只要logingatereq跑通，其他的协议recv直接全部丢弃不处理，logic更直接，只负责与sgate建立通信，收到的所有消息全部丢弃。
看sgate在logingatereq完成之后纯粹的转发性能。
重新设计符合这个要求的bench1和logic1。tcp和websocket分开写bench1_tcp+logic1_tcp和bench1_ws+logic1_ws。压测出sgate在这个链路的tcp和websocket的实际性能。

bench1具体的流程细节
Client: proto.Marshal(MessageFrame) + TCP/websocket write
sgate: TCP/websocket read → proto.Unmarshal(MessageFrame) → pipeline全量检查(已认证则跳过) → StreamShard channel → proto.Marshal(StreamData) → gRPC stream.Send
Logic: gRPC stream.Recv() → proto.Unmarshal(StreamData) → 丢弃

## bench2：逻辑服→sgate→客户端 推送压测

首先建立需要测试的连接。保证这一条链路是通的。这时候不开始不压测。
随后重新设计logic2.根据连接的useruuid去创建不同的组，针对这些组做推送。
其实相当于logic2变成了压测工具，测试的是logic2->sgate->bench2的性能。其中bench2。对这些推送recv直接丢弃。看的是sgate的推送转发性能和推送的准确性。
重新设计符合这个要求的bench2和logic2，tcp和websocket分开写bench2_tcp+logic2_tcp和bench2_ws+logic2_ws。压测出推送这块的实际性能。

bench2具体的流程细节
Client: proto.Marshal(MessageFrame) + TCP/websocket write
sgate: TCP/websocket read → proto.Unmarshal(MessageFrame) → pipeline全量检查(已认证则跳过) → StreamShard channel → proto.Marshal(StreamData) → gRPC stream.Send
Logic: gRPC stream.Recv() → proto.Unmarshal(StreamData) → 正确解析loginreq建立useruuid的session通信库(可以在这边等一段时间再去走推送逻辑) → logic推送逻辑组建立 → 执行推送逻辑(主要逻辑所在) → proto.Marshal(StreamData) → gRPC stream.Send
sgate: gRPC stream.Recv() → proto.Unmarshal(StreamData) → StreamShard channel → TCP/websocket write
Client: TCP/websocket read → proto.Unmarshal(MessageFrame) → 丢弃

## 编译和运行

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
```

### tools/client_logincheck（非压测工具）

`tools/client_logincheck` 是手动联调工具：调用 loginserver HTTP `/api/v1/login` 取 token 后走 LoginGate 验证登录链路。
**它不属于压测程序**（放在 `bench/` 下会违反 AGENTS.md「bench 禁止调用 /api/v1/login」），吞吐压测时不要编译/运行它。

## 已验证结果

原始数据：`bench/latest_results.json`（含逐秒明细与 `/stats` 快照）；bench2 明细见 `logs/sgate.log`（tlog 只写文件）。绝对数值随后台负载波动，同配置多轮约 ±10%。

### 测量条件（清单）

- 100 连接、单轮 10s、64B 载荷；登录仅 LoginGate（`loginValidation.enabled: false`，禁 HTTP login / token 校验）
- 压测网关统一用 `config_batch_off.yaml` / `config_batch_on.yaml`：`security.rateLimit.enabled: false`（限流关闭，热路径不计令牌桶）、`protection.benchMode: true`（关闭过载检查、WS 心跳清理、连接空闲检查等后台周期任务，避免与热路径抢调度）
- 网关 `shardCount=96`、`connGroupCount=4`、`sendChannelSize=65536`、`disableTracer`、`disableMetricsLog`
- 逻辑服 96 流（`NumCPU*8`）；logic2 `push-size=64`、`push-workers=12`、`expected-members=0`（登录即开推）
- bench2 用 `run.bat on|off` / `run.sh [on|off]` 切换 `config_batch_on.yaml` / `config_batch_off.yaml`（`batchPush: true|false`）

### bench1：client→sgate→logic 转发

| 协议 | 总转发量(10s) | 平均速率 | 登录失败 | droppedAuth |
| --- | ---: | ---: | ---: | ---: |
| TCP | 4,565,055 | **456,501/s** | 0 | 0 |
| WebSocket | 4,420,326 | **442,009/s** | 0 | 0 |

### bench2：logic→sgate→client 推送

| 协议 | batchPush | 平均接收速率 | 总接收(10s) | 连接失败 | droppedAuth |
| --- | --- | ---: | ---: | ---: | ---: |
| TCP | true | **1,330,690/s** | 13,330,558 | 0 | 0 |
| TCP | false | 704,763/s | 7,445,618 | 0 | 0 |
| WebSocket | true | **758,811/s** | 7,725,836 | 0 | 0 |
| WebSocket | false | 548,545/s | 6,170,304 | 0 | 0 |

### bench2 复验（2026-10-13，全量优化后，batch-on 30s）

| 协议 | 平均接收速率 | 基线 | 变化 | 连接失败 |
| --- | ---: | ---: | ---: | ---: |
| TCP | **1,277,758/s** | 1,330,690/s | -4%（±10% 内） | 0 |
| WebSocket | **1,174,324/s** | 758,811/s | **+55%**（WS 缓冲 offset 复用） | 0 |

- 测量日期 2026-10-10（限流关闭 + benchMode 后的口径）；绝对数值随后台负载波动，同配置多轮约 ±10%
- 校验：各轮 `connections failed=0`、`droppedAuth=0`，日志无 401 / authfail / login-key 拒绝
- etcd key 使用 protocol 枚举：`/services/{belong}/{ServerTypeName}:{zone}/{instanceId}`（如 `SGATE` / `LOGINSERVER` / `LOGICSERVER`）

## 30 分钟混合 churn 压测（bench/churn30）

工具：`bench/churn30`（常驻用户随机突发 + 每分钟 50 新用户波次登录后大突发、5min 生存期 churn 关闭，JSONL 逐 10s 结果）；配置 `config_batch_rl.yaml`（限流开 maxTokens=2M）+ `config/log_bench.yaml`。**最终二进制复跑（2026-10-13 16:16，b21ed7c+全量优化）**：1802s、4702 万条、26,089 msg/s 均值；1650 登录全成功、1200 churn 零错误（write_err=0/read_closed=0）；dropped* 全 0、failed=0；forwarded 4750 万；CPU 空载归零；latency P50=1.3µs / P99=6.4µs。与 bd0b96b 基线（26,091/s）持平，优化无回归。

### 观测修复（e83f150）

- **延迟 QPC 高精度**：Go `time.Now` 连续读 99.99% 相同（粒度 ~1.2µs），亚微秒管道延迟被截成 0，`/stats` P50/P95/P99 恒 0。改 `obs.MonoNowNs()` 裸 QPC（100ns 粒度，Windows 文档保证跨核单调，`SyscallN` 直调零分配）。负载下 P50=1.3µs / P95=4.5 / P99=7~9。
- **CPU 量子混叠**：Windows 进程 CPU 时间 ~15.6ms 粒子，200ms 单点采样频繁 delta=0，`cpuPercent` 恒 0。改 EMA 平滑（~1s 时间常数），过载阈值用平滑值防毛刺误判。空闲时 0 是正确行为。
- **observeOnly**：`BenchMode` 下 `check()` 只采样 cpu/mem 供 `/stats` 展示，不置过载标志，压测不因过载丢消息。
- **droppedFull 语义**：转发失败只计 `messagesFailed`（`/stats` 新增 `failed` 字段），不再污染 `droppedTotal`（专指 overload 队列满）。

### 热点优化（25609c2，-37% 每消息分配）

- **QPC 零分配**：`MonoNowNs` 改 `SyscallN` 直调预取地址（原 `LazyProc.Call` 每次构造 `[]uintptr` = 9.6B/条）。
- **延迟采样降频**：每 8 条采样 1 条（QPC 为 syscall ~1µs，500K msg/s × 2 次烧 ~1 核；采样后 ~1.4% 单核，62K 样本/s 对百分位足够）。
- **TCP 帧缓冲复用**：`ConnContext.FrameBuf` 加 `frameOff` 偏移（旧实现消费完置 nil 每次读事件重分配 6.2B/条 = 34MB；全耗尽 `[:0]` 保容量、残留下次调用前移，append 热路径零重分配，-74%）。
- **slog 空转消除**（treasure-slog `eb8be47`）：`rbShard` 加 cap=1 邮箱唤醒 worker，删 50µs~2ms 空转退避（原 15% CPU）；`Async` 198ns/op。
- **profile 工具**：`SGATE_MEM_PROFILE_RATE=4096` 环境变量（Go 默认 512KB 采样率把每消息小分配全采样掉，profile 只剩启动期）。

### 验收

- bench1_tcp（`config_batch_off.yaml`，RL off，benchMode）暖轮 559K/561K msg/s（基线 456-509K 无回退）。
- churn30 RL-on 75s 零丢弃零失败（856K 条）。
- 同采样率 allocs profile 每消息分配 -37%+（>25% 目标）。
- 全门禁：gofmt / build / vet / staticcheck / test / test -race 通过。
- **bench1_ws 暖轮 647K msg/s**（基线 442K，WS 缓冲 offset 复用后 +46%），零丢弃，P50=0.8µs / P99=2.6µs。

## 限流 maxTokens 下扫（2026-10-13）

bench1_tcp 100 连接 5s，`/stats.forwarded` 为准（客户端发送速率含被丢弃消息不可用）。per-IP 限额即 maxTokens（压测全走 127.0.0.1），全局硬顶 = maxTokens×10 不先触发。

| maxTokens | fwd/s | droppedRateLimit |
| ---: | ---: | ---: |
| 100 | 120 | 2,980,686 |
| 1,000 | 1,180 | 2,842,601 |
| 10,000 | 11,980 | 2,862,674 |
| 50,000 | 59,980 | 2,504,916 |
| 100,000 | 123,231 | 2,552,061 |
| 250,000 | 254,911 | 1,086,561 |
| 400,000 | 430,481 | 503,449 |
| 600,000 | 478,586 | **0** |
| 800,000 | 437,947 | **0** |
| 1,000,000 | 449,515 | **0** |
| 2,000,000 | 432,515 | **0** |

结论：fwd/s 随 maxTokens 线性扩展至膝点 ~600K（单 IP 独占场景机器容量上限），之后满吞吐零丢弃。**生产默认 `DefaultRateLimitMaxTokens` 100 → 10000**（覆盖 NAT 后 ~100 连接 × 100 msg/s 合理突发，硬顶异常单 IP；高吞吐部署按需上调，全局硬顶 = maxTokens×10）。`config/config.yaml` 同步 10000 并修复缺失的 `rateLimit:` 键（此前为无效 YAML 缩进）。
