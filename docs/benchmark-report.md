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
| 最近测量 | 2026-10-09（后台有负载）；安静时段参考见「压测结果」 |

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

100 连接、10 秒、64B 推送载荷、12 推送协程、`shardCount=96`、`connGroupCount=4`、`sendChannelSize=65536`、`expected-members=0`（登录即开推）、`loginValidation.enabled: false`。登录仅走 LoginGate（cmd 1000001），无 HTTP login、无 token 校验。bench2 通过 `config_batch_on.yaml` / `config_batch_off.yaml` 切换 `batchPush`。

### 当前实测（2026-10-09，后台有负载）

| 轮次 | 方向 | 协议 | 总消息量 | 平均速率 | 失败 | droppedAuth |
|------|------|------|----------|----------|------|-------------|
| bench1_tcp | client→sgate→logic | TCP | 4,260,438 | **423,989 msg/s** | 0 | 0 |
| bench1_ws | client→sgate→logic | WebSocket | 4,264,923 | **424,967 msg/s** | 0 | 0 |
| bench2_tcp（batch on） | logic→sgate→client | TCP | 14,263,061 | **1,424,357 msg/s** | 0 | 0 |
| bench2_ws（batch on） | logic→sgate→client | WebSocket | 13,755,700 | **1,372,644 msg/s** | 0 | 0 |
| bench2_tcp（batch off） | logic→sgate→client | TCP | — | 888,463 msg/s（2 轮中位） | 0 | 0 |
| bench2_ws（batch off） | logic→sgate→client | WebSocket | — | 855,332 msg/s（2 轮中位） | 0 | 0 |

**校验**：各轮 `connections failed=0` / `droppedAuth=0`，日志无 401 / authfail / login-key 拒绝。原始结果（含逐秒明细与 `/stats` 快照）见 `bench/latest_results.json`；bench2 明细在 `logs/sgate.log`（tlog 只写文件，无 stdout）。

**负载说明**：绝对数值受本机后台应用（IM/游戏等占核）影响，同配置多轮约 ±10%。安静后台时段参考值：bench1 TCP 498,490/s、WS 527,297/s，bench2 on TCP 1,934,329/s、WS 1,713,708/s。对比吞吐时应以同批测量内部对比为准。

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

1. **结构化日志**：`tlog.Info("gateway metrics", ...)` 每秒输出一次
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
| goroutine 泄漏 | ✅ 无（热路径已无调试 I/O） |
| 消息吞吐量（TCP，100 连接） | ✅ bench1 **423,989 msg/s** / bench2(batch on) **1,424,357 msg/s** |
| 消息吞吐量（WS，100 连接） | ✅ bench1 **424,967 msg/s** / bench2(batch on) **1,372,644 msg/s** |
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

standalone 和 cluster 模式均向 etcd 注册网关连接信息，格式为 JSON：

```json
{
  "ip": "192.168.1.100",
  "grpc": 50051,
  "tcp": "192.168.1.100:48080",
  "websocket": "192.168.1.100:48081"
}
```

- etcd key: `/services/{belong}/{SERVER_TYPE_SGATE}:{zone}/{instanceId}`（protocol 枚举）
- lease: 自动续期，TTL 默认 10s
- loginserver 通过 etcd watch 前缀 `/services/{belong}/SERVER_TYPE_SGATE:` 获取网关连接地址
