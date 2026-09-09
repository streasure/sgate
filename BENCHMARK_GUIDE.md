# sgate 压测指南

## 概述

本指南包含 sgate 网关的完整压测流程，覆盖：
- TCP/WebSocket 双向通信压测
- 组推送压测
- 单用户推送压测
- 全服广播压测

## 环境要求

### 硬件

| 场景 | CPU | 内存 | 网络 |
|------|-----|------|------|
| 基础测试 | 4+ 核 | 8GB | 1Gbps |
| 千万级连接 | 32+ 核 | 32GB+ | 10Gbps |
| 生产压测 | 64+ 核 | 64GB+ | 25Gbps |

### 软件

- Go 1.22+
- Windows/Linux
- PowerShell/Bash

---

## 快速开始

### 1. 构建所有组件

```powershell
# 网关
go build -o sgate.exe ./cmd/gateway

# 最小逻辑服（回显）
go build -o logic_server_min.exe ./examples/logic_server_min

# TCP 压测客户端
go build -o tcp_bench.exe ./examples/bench

# WebSocket 压测客户端
go build -o ws_bench.exe ./examples/ws_bench

# 推送驱动（单用户/组/广播）
go build -o push_driver.exe ./examples/push_driver
```

### 2. 启动服务

```powershell
# 终端 1: 启动逻辑服
.\logic_server_min.exe

# 终端 2: 启动网关
.\sgate.exe -conf config/config.yaml
```

---

## 压测场景

### 场景 1: TCP 双向通信

**目标**: 测试客户端发送心跳，服务端回显的双向吞吐

```powershell
# 基础测试（10 连接，10 秒，batch=16）
.\tcp_bench.exe 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1

# 高并发测试（1000 连接，30 秒）
.\tcp_bench.exe 127.0.0.1:48080 1000 30 32 16384 127.0.0.1:8081 logic-1

# 极限测试（10000 连接，60 秒）
.\tcp_bench.exe 127.0.0.1:48080 10000 60 64 32768 127.0.0.1:8081 logic-1
```

**参数说明**:
```
tcp_bench.exe <addr> <conns> [duration] [batchSize] [inflight] [statsAddr] [serverId]
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| addr | - | 网关 TCP 地址 |
| conns | 100 | 并发连接数 |
| duration | 10 | 压测时长（秒） |
| batchSize | 16 | 每批发送消息数 |
| inflight | 8192 | 最大在途消息数 |
| statsAddr | 127.0.0.1:8081 | 网关统计地址 |
| serverId | logic-1 | 目标逻辑服 ID |

**预期结果**:
- 10 连接: ~10K QPS
- 1000 连接: ~100K QPS
- 10000 连接: ~500K QPS

---

### 场景 2: WebSocket 双向通信

**目标**: 测试 WebSocket 长连接的双向吞吐

```powershell
# 基础测试（10 连接，10 秒）
.\ws_bench.exe ws://127.0.0.1:48081/ 10 10

# 高并发测试（1000 连接，30 秒）
.\ws_bench.exe ws://127.0.0.1:48081/ 1000 30
```

**参数说明**:
```
ws_bench.exe <addr> <conns> [duration]
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| addr | - | 网关 WebSocket 地址 |
| conns | 100 | 并发连接数 |
| duration | 10 | 压测时长（秒） |

**预期结果**:
- 10 连接: ~10K QPS
- 1000 连接: ~80K QPS

---

### 场景 3: 单用户推送

**目标**: 测试 logic 主动向单个用户推送消息

```powershell
# 100 客户端，10 秒，1000 事件/秒
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 personal 100 10 1000

# 1000 客户端，30 秒，10000 事件/秒
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 personal 1000 30 10000
```

**参数说明**:
```
push_driver.exe <gateway-addr> <logic-listen-addr> <mode> [clients] [duration] [events-per-second]
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| mode | - | personal / group / broadcast |
| clients | 100 | 客户端数量 |
| duration | 10 | 压测时长（秒） |
| events-per-second | 1000 | 每秒事件数 |

**预期结果**:
- 100 客户端: ~650 QPS
- 1000 客户端: ~3000 QPS

---

### 场景 4: 组推送

**目标**: 测试 logic 向组内所有成员广播消息

```powershell
# 100 客户端，10 秒，1000 事件/秒
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 group 100 10 1000

# 1000 客户端，30 秒，10000 事件/秒
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 group 1000 30 10000
```

**预期结果**:
- 10 人组: ~6.6K QPS
- 100 人组: ~50K QPS
- 1000 人组: ~200K QPS

---

### 场景 5: 全服广播

**目标**: 测试 logic 向所有在线用户广播消息

```powershell
# 100 客户端，10 秒，1000 事件/秒
.\push_driver.exe 127.0.0.1:48080 127.0.0.1:50052 broadcast 100 10 1000
```

**预期结果**:
- 100 用户: ~6.5K QPS
- 1000 用户: ~50K QPS

---

## 纯转发测试

**目标**: 测试网关到 logic 的纯转发性能（无回显）

### 启动 noop 逻辑服

```powershell
# 编译
go build -o logic_noop.exe ./examples/logic_noop

# 启动
.\logic_noop.exe
```

### 运行转发压测

```powershell
# 编译
go build -o forward_bench.exe ./examples/forward_bench

# 100 连接，10 秒，100K msg/s
.\forward_bench.exe 127.0.0.1:48080 100 10 127.0.0.1:8081 100000

# 1000 连接，30 秒，1M msg/s
.\forward_bench.exe 127.0.0.1:48080 1000 30 127.0.0.1:8081 1000000
```

---

## 生产配置测试

### 配置文件

使用 `config/config.yaml` 的生产配置：

```yaml
transports:
  - protocol: tcp
    port: 48080
  - protocol: tcp
    port: 48081
    type: websocket

grpc:
  port: 50051
  logicAddr: "localhost:50052"

logicServers:
  - serverId: "logic-1"
    serverType: "Logic"
    zone: "default"
    address: "localhost:50052"

monitoring:
  prometheus:
    enabled: true
    addr: ":9101"

cluster:
  enabled: true
```

### 启用 etcd 集群

```powershell
# 启动 etcd（Windows）
etcd.exe

# 启动多个网关实例
.\sgate.exe -conf config/config.yaml -id gateway-1
.\sgate.exe -conf config/config.yaml -id gateway-2
```

---

## 监控指标

### 网关统计

访问 `http://127.0.0.1:8081/stats` 获取 JSON 统计：

```json
{
  "received": 1000000,
  "forwarded": 999500,
  "droppedTotal": 500,
  "pushedToClient": 999000,
  "pushDroppedNoConn": 1000,
  "activeConnections": 10000,
  "cpuPercent": 45.2,
  "memPercent": 62.1
}
```

### Prometheus 指标

访问 `http://127.0.0.1:9101/metrics` 获取 Prometheus 格式指标。

关键指标：
- `sgate_connections_active` - 活跃连接数
- `sgate_messages_received_total` - 接收消息总数
- `sgate_messages_forwarded_total` - 转发消息总数
- `sgate_messages_pushed_total` - 推送消息总数
- `sgate_messages_dropped_total` - 丢弃消息总数

---

## 压测矩阵

### 连接数阶梯

| 连接数 | TCP QPS | WS QPS | 内存占用 | CPU 占用 |
|--------|---------|--------|----------|----------|
| 100 | 10K | 8K | 50MB | 10% |
| 1,000 | 100K | 80K | 100MB | 30% |
| 10,000 | 500K | 400K | 500MB | 60% |
| 100,000 | 2M | 1.5M | 2GB | 80% |
| 1,000,000 | 5M | 3M | 10GB | 90% |
| 10,000,000 | 8M | 5M | 20GB | 95% |

### 组大小阶梯

| 组成员数 | 推送 QPS | 内存占用 | 延迟 P99 |
|----------|----------|----------|----------|
| 10 | 6.6K | 1MB | 0.5ms |
| 100 | 50K | 10MB | 1ms |
| 1,000 | 200K | 100MB | 5ms |
| 10,000 | 500K | 1GB | 20ms |
| 100,000 | 1M | 10GB | 100ms |

---

## 故障排查

### 问题 1: 连接数上不去

**症状**: 连接数卡在某个值

**排查**:
```bash
# 检查文件描述符限制
ulimit -n

# 检查端口范围
cat /proc/sys/net/ipv4/ip_local_port_range

# 检查 TIME_WAIT
netstat -an | grep TIME_WAIT | wc -l
```

**解决**:
```bash
ulimit -n 16777216
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sysctl -w net.ipv4.tcp_tw_reuse=1
```

### 问题 2: QPS 上不去

**症状**: CPU 占用不高但 QPS 停滞

**排查**:
```bash
# 检查 gRPC stream 状态
curl http://127.0.0.1:8081/stats

# 检查逻辑服是否响应
curl http://127.0.0.1:50052/health
```

**解决**:
- 增加 `stream.shardCount`
- 增加 `stream.sendChannelSize`
- 检查逻辑服处理能力

### 问题 3: 内存持续增长

**症状**: 内存使用持续上升不下降

**排查**:
```bash
# 获取 heap profile
go tool pprof http://127.0.0.1:8081/debug/pprof/heap

# 分析内存分配
go tool pprof -alloc_objects http://127.0.0.1:8081/debug/pprof/heap
```

**解决**:
- 检查连接是否正确关闭
- 检查 buffer pool 归还
- 调整 GOMEMLIMIT

---

## 自动化脚本

### Windows PowerShell

```powershell
# run_benchmark.ps1

param(
    [int]$Connections = 100,
    [int]$Duration = 10,
    [int]$BatchSize = 16
)

# 构建
go build -o sgate.exe ./cmd/gateway
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o tcp_bench.exe ./examples/bench

# 启动服务
$logic = Start-Process -FilePath ".\logic_server_min.exe" -PassThru
Start-Sleep -Seconds 2

$gateway = Start-Process -FilePath ".\sgate.exe" -ArgumentList "-conf config/config.yaml" -PassThru
Start-Sleep -Seconds 2

# 运行压测
Write-Host "Starting benchmark: $Connections connections, $Duration seconds"
& .\tcp_bench.exe 127.0.0.1:48080 $Connections $Duration $BatchSize

# 清理
$gateway | Stop-Process -Force
$logic | Stop-Process -Force
```

### Linux Bash

```bash
#!/bin/bash
# run_benchmark.sh

CONNECTIONS=${1:-100}
DURATION=${2:-10}
BATCH_SIZE=${3:-16}

# 构建
go build -o sgate ./cmd/gateway
go build -o logic_server_min ./examples/logic_server_min
go build -o tcp_bench ./examples/bench

# 启动服务
./logic_server_min &
LOGIC_PID=$!
sleep 2

./sgate -conf config/config.yaml &
GATEWAY_PID=$!
sleep 2

# 运行压测
echo "Starting benchmark: $CONNECTIONS connections, $DURATION seconds"
./tcp_bench 127.0.0.1:48080 $CONNECTIONS $DURATION $BATCH_SIZE

# 清理
kill $GATEWAY_PID
kill $LOGIC_PID
```

---

## 结果记录模板

```
## 压测结果

**日期**: YYYY-MM-DD
**环境**: [CPU 核心数] 核, [内存] GB, [OS]

### TCP 双向通信
- 连接数: 
- 持续时间: 
- 发送 QPS: 
- 接收 QPS: 
- 丢弃数: 
- P99 延迟: 

### WebSocket 双向通信
- 连接数: 
- 持续时间: 
- 发送 QPS: 
- 接收 QPS: 
- 丢弃数: 
- P99 延迟: 

### 组推送
- 组大小: 
- 事件数: 
- 推送 QPS: 
- 接收 QPS: 
- P99 延迟: 

### 资源占用
- CPU 峰值: 
- 内存峰值: 
- 网络带宽: 
```
