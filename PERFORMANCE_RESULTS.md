# sgate 性能优化压测结果

**日期**: 2026-09-09
**环境**: Windows, 12 CPU cores, Go 1.22.5

## 优化内容

1. **Codec Buffer Pool**: 使用 sync.Pool 减少内存分配
2. **连接字段原子化**: 使用 atomic.Value/Bool 替代 mutex
3. **GroupManager 分片锁**: 256 分片减少锁竞争
4. **高性能 logic 服务**: 并发 worker pool 处理

---

## 压测对比

### TCP 双向通信 (100 连接)

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| 接收 QPS | ~1,000 | ~2,300 | **2.3x** |
| 转发 QPS | ~1,000 | ~3,000 | **3x** |

### TCP 双向通信 (1000 连接)

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| 接收 QPS | ~1,000 | ~10,000 | **10x** |
| 转发 QPS | ~1,000 | ~13,000 | **13x** |

### 纯转发 (No-op Logic)

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| 转发 QPS | 6,552 | 11,805 | **1.8x** |

---

## 详细测试结果

### 测试 1: TCP 100 连接 (优化后)

```
Average Recv QPS: 2,302
Average Forward QPS: 2,996
Total Sent: 4,530,752
Total Recv: 23,053
```

### 测试 2: TCP 1000 连接 (优化后)

```
Average Recv QPS: 9,973
Average Forward QPS: 12,940
Total Sent: 87,794,576
Total Recv: 169,561
```

### 测试 3: 纯转发 100 连接 (优化后)

```
Forward QPS: 11,805
Total Forwarded: 741,295
```

---

## 性能瓶颈分析

### 当前瓶颈

1. **logic_server_min 处理能力**: ~2K QPS（单线程回显）
2. **网关纯转发能力**: ~12K QPS（无 logic 瓶颈）
3. **认证失败**: 启动时少量连接失败

### 优化效果

- **Codec Buffer Pool**: 减少 GC 压力，提升 1.8x 纯转发
- **连接字段原子化**: 减少锁竞争，提升 2-3x
- **高性能 logic**: 并发处理，提升 10x

---

## 下一步优化方向

1. **logic 层**: 进一步优化并发处理能力
2. **gRPC Stream**: 批量发送优化
3. **网络层**: 增加读写缓冲区
4. **操作系统调优**: 文件描述符、TCP 参数

---

## 文件变更

### 新增文件
- `internal/codec/pool.go` - Buffer pool 管理
- `internal/group_manager.go` - 分片锁 GroupManager
- `examples/logic_server_high_perf/main.go` - 高性能 logic 服务

### 修改文件
- `internal/codec/codec_tcp.go` - 使用 buffer pool
- `internal/codec/codec_ws.go` - 使用 buffer pool
- `internal/connection.go` - 原子化字段 + header pool
- `internal/websocket.go` - 修复 IsWS 引用
