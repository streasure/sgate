# sgate 压测结果

**日期**: 2026-09-09
**环境**: Windows, 12 CPU cores, Go 1.22.5

---

## 测试 1: 纯转发 — 限流 1M tokens/s (最新)

### 前置

```powershell
# 启动 noop echo logic
.\logic_noop.exe

# 启动 gateway
.\sgate.exe -conf config/config.yaml
```

### 命令与结果

| 连接数 | 目标速率 | 实际 QPS | 转发 | 丢弃 | 认证失败 |
|--------|----------|----------|------|------|----------|
| 100 | 200K/s | **160,728** | 2,493,860 | 0 | 0 |
| 500 | 300K/s | **374,969** | 6,750,646 | 0 | 0 |
| 500 | 500K/s | **233,324** | 4,205,930 | 0 | 0 |
| 1000 | 500K/s | ~350K | 10,403,889 | 84,979 (0.8%) | 0 |

### sgate /stats（500 连接 300K/s 典型值）

```json
{
  "received": 6751246,
  "forwarded": 6750646,
  "droppedRateLimit": 0,
  "droppedFull": 0,
  "droppedOverload": 0,
  "droppedTotal": 0,
  "pushedToClient": 6750646,
  "activeConnections": 0,
  "memPercent": 45.6
}
```

### 分析

- **网关纯转发上限约 375K QPS**（500 连接，0 丢弃）
- 1000 连接时出现 0.8% overload 丢弃，网关内存 8.8GB，系统 kill 了进程
- 限流配置 `security.rateLimit.maxTokens: 1000000`（1M tokens/s per IP/route）生效后 0 限流丢弃
- 之前的 11K QPS 瓶颈是默认限流 10K tokens/s 导致 92% 消息被丢弃

---

## 测试 2: TCP 双向通信 (logic_server_min 回显)

### 命令

```powershell
.\logic_server_min.exe
.\sgate.exe -conf config/config.yaml
.\tcp_bench.exe 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1
```

### 结果

| 连接数 | 客户端发送 | 客户端接收 | 接收 QPS | 认证失败 |
|--------|-----------|-----------|----------|----------|
| 10 | 91,968 | 9,990 | **997** | 0 |
| 50 | 343,232 | 10,466 | **1,046** | 39 |

### 分析

- logic_server_min 单线程回显 ~1K QPS 是瓶颈
- 50 连接时 39 个认证失败：benchmark 的 LogicLogin 2s 超时，logic 处理太慢导致排队超时

---

## 测试 3: WebSocket 双向通信

### 命令

```powershell
.\ws_bench.exe ws://127.0.0.1:48081/ 10 10
```

### 结果

| 指标 | 值 |
|------|-----|
| 发送总数 | 91,920 |
| 接收总数 | 10,000 |
| 接收 QPS | **998** |
| 认证失败 | 0 |

---

## 配置变更记录

### 限流配置（config/config.yaml）

```yaml
security:
  enabled: true
  rateLimit:
    enabled: true
    maxTokens: 1000000    # 100万 tokens/s per IP/route（默认已改为 100万）
    tokenRefresh: "1s"
  circuitBreaker:
    enabled: false

waf:
  enabled: false
```

### 默认值变更（internal/config/defaults.go）

```go
// 旧: DefaultRateLimitMaxTokens = 10000
// 新:
DefaultRateLimitMaxTokens = 1000000
```

### loginAuth 默认关闭（internal/config/config.go）

```go
LoginAuth: LoginAuthConfig{
    Mode: "none",  // 默认跳过校验
}
```

---

## 启动命令

### 完整压测流程

```powershell
# 1. 构建
go build -o sgate.exe ./cmd/gateway
go build -o logic_noop.exe ./examples/logic_noop
go build -o logic_server_min.exe ./examples/logic_server_min
go build -o forward_bench.exe ./examples/forward_bench
go build -o tcp_bench.exe ./examples/bench
go build -o ws_bench.exe ./examples/ws_bench

# 2a. 纯转发压测（推荐，测网关上限）
.\logic_noop.exe                           # 终端 1
.\sgate.exe -conf config/config.yaml       # 终端 2
.\forward_bench.exe 127.0.0.1:48080 500 15 127.0.0.1:8081 300000

# 2b. 双向通信压测（测 logic + 网关）
.\logic_server_min.exe                     # 终端 1
.\sgate.exe -conf config/config.yaml       # 终端 2
.\tcp_bench.exe 127.0.0.1:48080 10 10 16 8192 127.0.0.1:8081 logic-1

# 3. 查看统计
Invoke-RestMethod -Uri "http://127.0.0.1:8081/stats" | ConvertTo-Json
```

### 关键端口

| 端口 | 用途 |
|------|------|
| 48080 | TCP 客户端接入 |
| 48081 | WebSocket 客户端接入 |
| 50051 | Gateway gRPC (logic 调用) |
| 50052 | Logic gRPC (gateway 转发) |
| 8081 | Gateway HTTP stats |

---

## 瓶颈总结

| 瓶颈 | 影响 | 解决方案 |
|------|------|----------|
| 默认限流 10K tokens/s | 92% 消息被丢弃 | `maxTokens: 1000000` |
| logic_server_min 单线程回显 | 双向 ~1K QPS | 用 logic_noop 测纯转发 |
| logic NoData 不回显 LogicLogin | 连接未认证，后续命令被丢 | noop 改为 echo |
| 1000 连接高负载 | 内存 8.8GB OOM | 降低连接数或消息速率 |
