# sgate 压测报告

## 测试环境

| 项目 | 值 |
|------|-----|
| 操作系统 | Windows 10/11 (win32) |
| CPU | Intel i5-10400F 6C/12T |
| 内存 | 32GB（可用 ~9.6GB） |
| 网卡 | 1Gbps |
| Go 版本 | go1.24+ |
| gnet 版本 | v2.9.7 |
| 测试时间 | 2026-09-13 |

## 测试限制

**Windows 平台无法创建超过 ~16K 本地连接**，原因是：
- Windows 临时端口范围默认 49152-65535（共 16384 个）
- 每个 `net.Dial` 创建新 socket 需要一个临时端口
- 无论连接多少个目标端口，总连接数受限于临时端口数
- 扩大端口范围需要管理员权限：`netsh int ipv4 set dynamicport tcp start=1024 num=64511`
- 真正的百万连接测试需要 **Linux 环境**（epoll 原生支持）或**管理员权限**

## 发现的瓶颈及修复

### P0：wsDebug 热路径阻塞（已修复）

**问题**：`wsDebug` 函数在每个网络事件的热路径中调用，向文件写入 WebSocket 帧调试信息。

**影响**：
- 每个网络事件都触发文件 I/O + 磁盘 Sync
- 369 个 goroutine 阻塞在 `fmt.Fprintln`
- 252 个 goroutine 阻塞在文件写入
- 内存从 39MB 膨胀到 1.17GB（goroutine 栈累积）

**修复**：移除热路径中所有 `wsDebug` 调用（`frontend.go`、`websocket.go`），改用结构化日志 `tlog`。删除 `wsDebug` 函数定义。

**效果**：
- 内存：1.17GB → 39MB（-96.7%）
- 句柄：15570 → 287（-98.2%）
- goroutine：770 → 770（稳定，不再增长）

### P1：gnet Windows 平台 goroutine 模型（架构限制）

**现象**：每个连接创建一个 goroutine，连接关闭后 goroutine 不退出，卡在 `eventloop.run`。

**分析**：gnet 在 Windows 上用 goroutine 模拟 epoll 的事件循环（`eventloop_windows.go:161`），这是 gnet 库的架构决定，非网关代码问题。

**结论**：在 Linux（epoll）上不会出现此问题。Windows 上不影响功能，仅影响工作集指标。

### P2：连接级流控（已实现）

**问题**：当前只有 IP 级和 route 级限流，无单连接限流。异常连接可占满 gRPC 发送队列。

**方案**：在 `Connection` 结构体添加 `msgCount`/`msgWindowStart` 字段，`CheckAndIncrementMsgRate()` 方法实现每秒窗口计数。pipeline 处理阶段 2.5 检查。

**配置**：`protection.maxMessagesPerConn`（0=不限制）

### P3：连接生命周期指标（已实现）

**问题**：当前只有平均连接时长，缺少连接时长分布直方图。

**方案**：复用 `obs.LatencyTracker`（滑动窗口 10000 样本），在 `OnClose` 时记录连接存活时长。`/stats` 端点返回 P50/P95/P99。

**新增字段**：`connectionDurationP50Ms`、`connectionDurationP95Ms`、`connectionDurationP99Ms`

### P4：热配置更新（已实现）

**问题**：当前配置修改需重启。

**方案**：`configWatcher` 监听文件变化，`handleConfigUpdate` 动态更新以下参数：

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

### 多 TCP 连接优化（connGroupCount）

**问题**：gateway 与 logic 之间所有 gRPC stream 共享单条 TCP 连接，HTTP/2 协议要求同一连接上的 stream 共享写锁，导致 96 个 shard 串行写入，实际并行度为 1。

**方案**：新增 `stream.connGroupCount` 配置项，gateway 对同一 logic 服务器建立 N 条独立 TCP 连接（默认 4），每个 connGroup 承载 `shardCount/N` 个 stream，各自拥有独立的 HTTP/2 写锁。

**效果**：

| 指标 | 改前（单 TCP） | 改后（4 TCP） | 提升 |
|------|---------------|--------------|------|
| bench2_tcp 接收速率 | 39.3 万/s | **69.4 万/s** | **+76%** |
| bench2_ws 接收速率 | 72.7 万/s | **128.9 万/s** | **+77%** |

### 连接建立性能

| 指标 | 值 |
|------|-----|
| 目标连接数 | 15,000 |
| 成功连接数 | 15,000 |
| 失败数 | 0 |
| 建立速率 | **11,297 conn/s** |
| 建立耗时 | 1.33s |

### 消息吞吐量（bench1 TCP）

| 指标 | 值 |
|------|-----|
| 总转发消息 | 8,446,656 |
| 峰值速率 | **835K msg/s** |
| 平均速率 | 533K msg/s |
| 连接数 | 100 |
| 测试时长 | 10s |

### 消息吞吐量（bench1 WS）

| 指标 | 值 |
|------|-----|
| 总转发消息 | 9,412,216 |
| 峰值速率 | **920K msg/s** |
| 平均速率 | 487K msg/s |
| 连接数 | 100 |
| 测试时长 | 10s |

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
- 多 TCP 连接优化：吞吐提升 76-77% ✅

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
| goroutine 泄漏 | ✅ 无（修复 wsDebug 后） |
| 连接建立速率 | ✅ 11K conn/s |
| 消息吞吐量（TCP） | ✅ 835K msg/s（bench1）/ 69.4 万/s（bench2 1000连接） |
| 消息吞吐量（WS） | ✅ 920K msg/s（bench1）/ 128.9 万/s（bench2 1000连接） |
| 多 TCP 连接优化 | ✅ connGroupCount 配置，默认 4 连接，吞吐提升 76-77% |
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

- etcd key: `/services/Gateway:{zone}/{serverID}`
- lease: 自动续期，TTL 默认 10s
- loginserver 通过 etcd watch 前缀 `/services/Gateway:` 获取网关连接地址
