# sgate 性能与压测规范

## 1. 目标定义

“千万级双向通信”必须拆成两个指标：

- **在线连接数**：集群同时保持的 TCP/WebSocket session 数。千万在线不是单个网关目标，必须通过多节点、分片和连接接入层实现。
- **消息吞吐**：客户端上行和服务端下行的消息数/秒。当前代码和现有实测不能证明千万消息/秒。

“百万级组推送”指单个组包含 1,000,000 个在线成员时，一次逻辑事件完成 1,000,000 次客户端投递。它受出口带宽、消息大小、客户端读速率和节点 fan-out 能力约束，不应只用 gRPC RPC QPS 判定。

容量估算：

```text
出口带宽 ~= 推送消息数 * (消息体 + 协议开销) * 8
组推送成本 ~= 组成员数 / 单节点可稳定 fan-out 速率
```

例如 1,000,000 人、每人 256 bytes 的一次推送，业务数据约 256 MB，实际 TCP/加密/系统开销更高。单机没有足够 NIC、内存和写队列时，必须拆分组成员到多个 gateway 节点。

## 2. 当前基线

历史基线来自 Windows、12 logical CPUs、Go 1.22.5、本机 loopback、10 个连接、10 秒测试：

| 场景 | 结果 |
|---|---:|
| TCP 稳定双向回显 | 约 997 recv msg/s |
| WebSocket 双向回显 | 约 998 recv msg/s |
| no-op 上行转发无丢弃 | 约 6.6K msg/s |
| 10 人主动组推送 | 约 6.6K client deliveries/s |

这些数据只用于回归对比，不是生产容量承诺。特别是旧的 `push_bench` stream echo 不代表 fan-out，已从工程删除。所有新结果必须同时记录 offered load、accepted、forwarded、pushed、dropped、P95/P99、CPU、RSS、GC、网卡吞吐和测试拓扑。

## 3. 保留的压测工具

源码工具位于 `examples/`，不把 exe 或日志提交到仓库：

- `bench`：TCP 双向回显。
- `ws_bench`：WebSocket 双向回显。
- `logic_noop` + `forward_bench`：隔离 logic 业务后的上行转发。
- `push_driver`：主动单用户、组推送、全服广播，内嵌 logic driver。
- `logic_server_min`：本地回显 logic。

## 4. 构建与启动

PowerShell：

```powershell
$out = Join-Path $env:TEMP "sgate-bench"
New-Item -ItemType Directory -Force $out | Out-Null
go build -o "$out\sgate.exe" ./cmd/gateway
go build -o "$out\logic_server_min.exe" ./examples/logic_server_min
go build -o "$out\tcp_bench.exe" ./examples/bench
go build -o "$out\ws_bench.exe" ./examples/ws_bench
go build -o "$out\logic_noop.exe" ./examples/logic_noop
go build -o "$out\forward_bench.exe" ./examples/forward_bench
go build -o "$out\push_driver.exe" ./examples/push_driver
```

使用两个终端启动本地基线：

```powershell
& "$out\logic_server_min.exe"
& "$out\sgate.exe" -conf config/config.yaml
```

配置文件默认关闭 etcd、discovery、cluster、configCenter 和 Prometheus，避免压测依赖外部服务。正式集群测试必须使用部署系统覆盖这些配置，不要复制出第二套 yaml。

## 5. 基线测试命令

TCP 和 WebSocket 必须串行执行，每轮前清理上一轮进程。客户端和 gateway 同机时：

```powershell
& "$out\tcp_bench.exe" 127.0.0.1:48080 100 60 16 256 127.0.0.1:8081 logic-1
& "$out\ws_bench.exe" ws://127.0.0.1:48081/ 100 60
```

纯转发：

```powershell
& "$out\logic_noop.exe"
& "$out\sgate.exe" -conf config/config.yaml
& "$out\forward_bench.exe" 127.0.0.1:48080 100 60 127.0.0.1:8081 100000
```

主动推送三种模式必须分别执行：

```powershell
& "$out\push_driver.exe" 127.0.0.1:48080 127.0.0.1:50052 personal 100 60 1000
& "$out\push_driver.exe" 127.0.0.1:48080 127.0.0.1:50052 group 100 60 1000
& "$out\push_driver.exe" 127.0.0.1:48080 127.0.0.1:50052 broadcast 100 60 1000
```

## 6. 观测和判定

每轮测试前后保存 `/stats`；日志输出到测试机临时目录，不写入仓库：

```powershell
Invoke-WebRequest http://127.0.0.1:8081/stats | Select-Object -Expand Content
```

必须检查：

- `received >= forwarded + dropped`，差异需说明批处理或采样原因。
- 无丢弃档：`droppedTotal == 0`、`pushDroppedNoConn == 0`，客户端收到数与 gateway `pushedToClient` 一致。
- 稳态阶段 P99 延迟、CPU、RSS 和 GC 不持续增长；不能只看平均 QPS。
- fan-out 测试必须报告成员数、实际投递数和消息大小，不能用 RPC 请求数代替客户端投递数。
- 压测客户端、gateway、logic、监控端口和进程必须记录 PID 与主机名，避免旧进程污染结果。

建议使用 Go pprof、Windows Performance Monitor 或 Linux `perf`/`sar` 采集 CPU、RSS、网络和 GC。跨主机测试还要记录 NIC 速率、RTT、丢包和 MTU。

## 7. 分阶段容量矩阵

先在单节点完成 100、1K、10K 连接阶梯，再进行 100K 和多节点测试。每档至少预热 5 分钟、稳定采样 30 分钟、冷却并检查连接泄漏。

| 阶段 | 在线连接 | 组成员 | 单次 fan-out | 目的 |
|---|---:|---:|---:|---|
| 回归 | 100 | 100 | 1K | 检查功能和零丢弃 |
| 单机容量 | 10K | 10K | 10K | 找 CPU、内存、写队列拐点 |
| 节点容量 | 100K | 100K | 100K | 确定单节点安全上限 |
| 集群容量 | 1M+ | 1M | 1M | 验证分片、跨节点路由和带宽 |
| 目标验证 | 10M | 1M/组 | 1M | 验证多节点部署而非单进程极限 |

最终上线容量取满足零丢弃、P99、RSS、CPU 和带宽全部门槛的最大稳定档，并保留至少 30% 资源余量。当前仓库不应宣称已经达到千万在线或百万 fan-out；达到目标还需要多节点连接分片、组成员分片/分层 fan-out、可控背压、慢客户端隔离、跨网关广播协议和真实网络长稳压测。
