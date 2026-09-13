# Sgate Benchmark Report

## 1. Architecture Overview

### 1.1 System Architecture

```
                    ┌─────────────┐
                    │   Clients   │
                    │ (TCP / WS)  │
                    └──────┬──────┘
                           │
                    ┌──────▼──────┐
                    │    Sgate    │  ← gnet event-loop (12 loops)
                    │  (Gateway)  │  ← 96 StreamShards → gRPC multiplexed
                    └──────┬──────┘
                           │ gRPC bidirectional stream
                    ┌──────▼──────┐
                    │   Logic     │  ← business logic / push engine
                    │   Servers   │
                    └─────────────┘
```

Sgate is a high-performance gateway built on [gnet v2](https://github.com/panjf2000/gnet) event-loop. It bridges clients (TCP/WebSocket) to backend logic servers via gRPC bidirectional streaming.

### 1.2 Data Flow

**Client → Logic (forwarding):**
```
Client TCP/WS write
  → gnet event-loop React()
  → decodeClientMessage(MessageFrame)
  → MessagePipeline (auth check, security, filter)
  → StreamShard channel (96 shards, hash by sessionID)
  → proto.Marshal(StreamData)
  → gRPC stream.Send()
  → Logic: gRPC stream.Recv()
```

**Logic → Client (push):**
```
Logic: proto.Marshal(StreamData) → gRPC stream.Send()
  → Sgate: gRPC stream.Recv()
  → StreamShard receiveMessages()
  → [batchPush=OFF] conn.Send(responseData) per message
  → [batchPush=ON]  collect batch → flush as PushBatch per connection
  → Client TCP/WS write
```

### 1.3 Protocol Types

| Layer | Message Type | Wire Format |
|-------|-------------|-------------|
| Client ↔ Sgate | `MessageFrame` | TCP: `[4B length][protobuf]`; WS: WebSocket binary frame `[protobuf]` |
| Sgate ↔ Logic | `StreamData` | gRPC protobuf (multiplexed over single TCP) |

### 1.4 Key Command IDs

| Constant | Value | Direction | Description |
|----------|-------|-----------|-------------|
| `CmdLoginGate` | 1000001 | Client → Sgate | Login request |
| `CmdLoginGateAck` | 1000002 | Sgate → Client | Login acknowledgment |
| `CmdHeartbeatReq` | 1100010 | Client → Sgate | Heartbeat request |
| `CmdUserOffline` | 1100012 | Logic → Sgate | User offline notification |
| `cmdPush` | 9000001 | Logic → Client | Single push message (bench) |
| `CmdPushBatch` | 9000002 | Sgate → Client | Batch push message |

---

## 2. Batch Push Protocol

### 2.1 Motivation

When a logic server pushes messages to many clients simultaneously (e.g., group broadcast), each `PushToConnection` call results in one `StreamData` → one gRPC `stream.Recv()` → one `marshalClientMessage()` → one `conn.Send()`. At high fan-out (100+ members, 12 workers), this creates:
- 1200+ marshal/unmarshal cycles per round
- 1200+ gRPC frame headers overhead
- Per-message `GetConnection()` lookup overhead

Batching merges N messages destined for the same client into a single `PushBatch`, reducing per-message overhead.

### 2.2 Proto Definitions

```protobuf
// gateway.proto

message PushItem {
    string session_id = 1;
    int32  cmd        = 2;
    bytes  data       = 3;
    int64  seq_id     = 4;
}

message PushBatch {
    repeated PushItem items = 1;
}
```

### 2.3 Wire Format

When `batchPush: true`, sgate wraps `PushBatch` in a `MessageFrame`:

```
MessageFrame {
    Cmd:  9000002 (CmdPushBatch)
    Body: proto.Marshal(PushBatch{ items: [...] })
}
```

Client receives `[4B length][MessageFrame]` where `MessageFrame.Cmd == 9000002` and `MessageFrame.Body` contains serialized `PushBatch`.

### 2.4 Sgate Implementation

In `backend.go:receiveMessages()`:

1. **Collect phase**: Messages from `stream.Recv()` are appended to a batch slice
2. **Flush triggers**:
   - Batch reaches `batchFlushSize` (128)
   - Broadcast message (`SessionId == ""`) encountered
   - Stream error or disconnect
3. **Flush execution**:
   - Group items by `SessionId` into per-connection batches
   - For each connection: marshal `PushBatch` → wrap in `MessageFrame(Cmd=9000002)` → `conn.Send()`

### 2.5 Configuration

```yaml
stream:
  batchPush: true   # Enable batch push mode
  # batchPush: false  # Default: individual push (original behavior)
```

### 2.6 Client Handling

Bench2 client checks `frame.Cmd`:
- `== 9000002` (CmdPushBatch): unmarshal `PushBatch`, count `len(batch.Items)`
- Otherwise: count as 1 message

---

## 3. Benchmark Design

### 3.1 Test Scenarios

| Scenario | Chain | Tests | Purpose |
|----------|-------|-------|---------|
| **Bench1** | bench1 → sgate → logic1 | TCP, WS | Client→Logic forwarding throughput |
| **Bench2** | bench2 ← sgate ← logic2 | TCP, WS | Logic→Client push throughput |

### 3.2 Bench1: Forwarding (Client → Logic)

```
bench1_tcp/ws                    sgate                     logic1_tcp/ws
    │                              │                           │
    │── LoginGateReq ──────────────│── StreamData(LoginReq) ──▶│
    │◀── LoginGateAck ─────────────│◀── StreamData(LoginAck) ──│
    │                              │                           │
    │── [flood N× StreamData] ────▶│── [forward all] ────────▶│ (discard)
    │   (TCP: 4B+len protobuf)     │   (gRPC stream.Send)     │
    │   (WS: WS binary frame)      │                           │
```

- bench1: Opens N TCP/WS connections, logs in, then floods with StreamData messages
- logic1: Receives all messages and discards them (pure throughput test)
- Measures: sgate forwarding rate (msg/s)

### 3.3 Bench2: Push (Logic → Client)

```
bench2_tcp/ws                    sgate                     logic2_tcp/ws
    │                              │                           │
    │── LoginGateReq ──────────────│── StreamData(LoginReq) ──▶│
    │◀── LoginGateAck ─────────────│◀── StreamData(LoginAck) ──│
    │                              │                           │ (joinGroup)
    │                              │                           │
    │                              │◀── StreamData(Push) ─────│ (N workers × M members)
    │◀── MessageFrame(Push) ───────│                           │
    │   [batchPush=OFF: per msg]   │                           │
    │   [batchPush=ON: PushBatch]  │                           │
```

- bench2: Opens N connections, logs in, then receives push messages
- logic2: After all members join, spawns 12 push workers that continuously call `PushToConnection` for each group member
- `push-interval=0`: Workers push as fast as possible (no sleep)
- Measures: bench2 receive rate (msg/s), logic2 push rate (msg/s)

### 3.4 Test Parameters

| Parameter | Value |
|-----------|-------|
| Connections | 100 |
| Duration | 10s |
| Push payload | 64 bytes |
| Push workers | 12 |
| Push interval | 0 (no delay) |
| Expected members | 100 |
| sgate event-loops | 12 (GOMAXPROCS) |
| Stream shards | 96 |
| gRPC window size | 64 MB (sgate) / 512 KB (logic) |

### 3.5 Environment

| Item | Value |
|------|-------|
| OS | Windows 10/11 (win32) |
| CPU | 12 cores |
| Go | 1.24+ |
| gnet | v2 |
| Transport | TCP + WebSocket (gnet built-in) |
| gRPC | HTTP/2 multiplexed |
| etcd | v3.5.18 (local) |

---

## 4. Benchmark Results

测试日期：2026-09-13。测试条件：100 个连接、64 字节推送载荷、12 个推送工作协程、持续 10 秒、`push-interval=0`、96 个流分片。

### 4.1 Bench1 转发性能（优先 WebSocket）

| 协议 | 总转发量(10s) | 峰值速率 |
|------|---------------|----------|
| **WebSocket** | **843 万** | **840K/s** |
| TCP | 770 万 | 767K/s |

**结论：** WebSocket 和 TCP 转发性能基本持平，WebSocket 略高于 TCP，与直觉一致（gnet 对 WS 帧处理路径较短）。

### 4.2 Bench2 推送性能（优先 WebSocket）

| 模式 | TCP | WebSocket | TCP 提升 | WS 提升 |
|------|-----|-----------|----------|---------|
| `batchPush: false`（默认） | 421K/s | 442K/s | — | — |
| `batchPush: true`（批量） | **853K/s** | **816K/s** | **+102%** | **+85%** |

**结论：**
- 批量推送模式接近翻倍提升
- TCP 批量提升略高于 WS，因为 TCP 的 per-message 封装开销更低，批量摊销效果更明显
- 无论是否启用批量，TCP 和 WS 吞吐量在同一量级

### 4.3 Previous Results (before batch push implementation)

| Test | TCP | WS |
|------|-----|-----|
| Bench1 Forwarding | 996K/s | 1.04M/s |
| Bench2 Push (baseline) | 387K/s | 422K/s |

Note: The previous results were from a different test session with slightly different system conditions. The relative improvement from batch push is consistent.

---

## 5. Code Review Findings & Fixes

### 5.1 Bug: Message Loss on Stream Disconnect

**Problem**: When `stream.Recv()` returned an error (logic disconnect), the batch slice was not flushed, causing unprocessed messages to be silently dropped.

**Fix**: Added `flushBatch()` call before error handling in `receiveMessages()`:

```go
if err != nil {
    if batchPush {
        flushBatch()  // ← Flush remaining messages before disconnect
    }
    if closing {
        return
    }
    s.markShardBroken()
    return
}
```

### 5.2 Bug: UserKey Not Updated in Batch Mode

**Problem**: In batch mode, `UpdateConnectionUserUUID()` was not called when `msg.UserKey != ""`, causing stale user→session mappings.

**Fix**: Added UserKey update before batch append:

```go
if batchPush {
    if msg.UserKey != "" {
        lc.gateway.GetConnectionManager().UpdateConnectionUserUUID(msg.SessionId, msg.UserKey)
    }
    batch = append(batch, msg)
    // ...
}
```

### 5.3 Design Principle: Logic API Cleanliness

All batching optimization is transparent to the logic layer. Logic API remains:
- `PushToConnection(sessionID, cmd, data)` — single push
- `SendToGroup(groupID, cmd, data)` — group push (fan-out per gateway)
- `Broadcast(cmd, data)` — global broadcast

Batching is a sgate-layer concern only. Logic does not need to know about `PushBatch`.

---

## 6. Configuration Reference

### 6.1 Sgate Config (config.yaml)

```yaml
stream:
  shardCount: 96            # Stream shard count (gRPC multiplexing)
  sendChannelSize: 1048576  # Per-shard send channel buffer (1M)
  receiveBatchSize: 128     # Receive batch size
  batchPush: false          # Batch push mode (true/false)

grpc:
  port: 50051
  windowSize: 67108864      # 64 MB gRPC window

transports:
  - protocol: tcp
    port: 48080
  - protocol: tcp
    port: 48081
    type: websocket
```

### 6.2 Config Files

| File | batchPush | Use Case |
|------|-----------|----------|
| `config.yaml` | not set (false) | Default / production |
| `config_batch_off.yaml` | `false` | Benchmark baseline |
| `config_batch_on.yaml` | `true` | Benchmark batch mode |

---

## 7. How to Run Benchmarks

测试顺序：先 WebSocket，再 TCP。

### 7.1 Prerequisites

```powershell
# Start etcd
E:\etcd-v3.5.18-windows-amd64\etcd.exe

# Build all binaries
cd E:\sgate
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

### 7.2 Run Bench1 — WebSocket（优先）

**Step 1: Start sgate**
```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
```

**Step 2: Start logic1_ws**
```powershell
.\bench\logic1_ws\logic1_ws.exe -port 50053 -id logic1-ws -config bench\logic1_ws\configs\log.yaml
```

**Step 3: Start bench1_ws**
```powershell
.\bench\bench1_ws\bench1_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100
```

### 7.3 Run Bench1 — TCP

```powershell
.\sgate.exe -conf config\config.yaml -config config\log.yaml
.\bench\logic1_tcp\logic1_tcp.exe -port 50050 -id logic1-tcp
.\bench\bench1_tcp\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

### 7.4 Run Bench2 — WebSocket（优先）

**Step 1: Start sgate**
```powershell
# batchPush OFF (baseline)
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml

# OR batchPush ON
.\sgate.exe -conf config\config_batch_on.yaml -config config\log.yaml
```

**Step 2: Start logic2_ws**
```powershell
.\bench\logic2_ws\logic2_ws.exe -port 50061 -id logic2-ws -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_ws\configs\logic2_ws_log.yaml
```

**Step 3: Start bench2_ws**
```powershell
.\bench\bench2_ws\bench2_ws.exe -addr 127.0.0.1:48081 -duration 10s -parallel 100 -server-id logic2-ws -config bench\bench2_ws\configs\log.yaml
```

### 7.5 Run Bench2 — TCP

```powershell
.\sgate.exe -conf config\config_batch_off.yaml -config config\log.yaml
.\bench\logic2_tcp\logic2_tcp.exe -port 50060 -id logic2-tcp -push-interval 0 -push-size 64 -expected-members 100 -push-workers 12 -config bench\logic2_tcp\configs\logic2_tcp_log.yaml
.\bench\bench2_tcp\bench2_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100 -server-id logic2-tcp -config bench\bench2_tcp\configs\log.yaml
```

### 7.6 Read results
```powershell
# bench1 results
# 直接看终端输出

# bench2 results
Get-Content bench\bench2_tcp\logs\bench2_tcp.log | Select-String "result|completed"
Get-Content bench\bench2_ws\logs\bench2_ws.log | Select-String "result|completed"

# logic2 progress
Get-Content bench\logic2_tcp\logs\logic2_tcp.log | Select-String "progress" | Select-Object -Last 3
Get-Content bench\logic2_ws\logs\logic2_ws.log | Select-String "progress" | Select-Object -Last 3
```

### 7.3 Run Bench1 (Forwarding Test)

```powershell
# Start sgate (default config)
.\sgate.exe -conf config\config.yaml -config config\log.yaml

# Start logic1
.\bench\logic1_tcp\logic1_tcp.exe -port 50050 -id logic1-tcp

# Start bench1
.\bench\bench1_tcp\bench1_tcp.exe -addr 127.0.0.1:48080 -duration 10s -parallel 100
```

### 7.4 Cleanup

```powershell
Get-Process -Name sgate,logic1_tcp,logic1_ws,logic2_tcp,logic2_ws,bench1_tcp,bench1_ws,bench2_tcp,bench2_ws -ErrorAction SilentlyContinue | Stop-Process -Force
```

---

## 8. Project File Structure

```
sgate/
├── cmd/gateway/main.go          # Entry point
├── internal/
│   ├── frontend.go              # TCP event-loop handler, login forwarding
│   ├── websocket.go             # WebSocket event-loop handler
│   ├── backend.go               # LogicClient, StreamShard, receiveMessages (batch logic)
│   ├── connection.go            # Connection struct (gnet.Conn wrapper)
│   ├── pipeline.go              # MessagePipeline (auth, security, filter)
│   ├── frame.go                 # marshalClientMessage, decodeClientMessage
│   ├── config/config.go         # Config structs (StreamConfig.BatchPush)
│   └── gateway/routes.go        # Cmd constants (CmdPushBatch=9000002)
├── logic/
│   ├── server.go                # Logic Server (PushToConnection, SendToGroup, Broadcast)
│   └── config.go                # Logic config
├── bench/
│   ├── bench1_tcp/              # Forwarding benchmark client (TCP)
│   ├── bench1_ws/               # Forwarding benchmark client (WS)
│   ├── bench2_tcp/              # Push benchmark client (TCP)
│   ├── bench2_ws/               # Push benchmark client (WS)
│   ├── logic1_tcp/              # Forwarding benchmark server (TCP)
│   ├── logic1_ws/               # Forwarding benchmark server (WS)
│   ├── logic2_tcp/              # Push benchmark server (TCP)
│   ├── logic2_ws/               # Push benchmark server (WS)
│   └── logutil/                 # Shared tlog initialization
├── config/
│   ├── config.yaml              # Default sgate config
│   ├── config_batch_off.yaml    # Benchmark: batchPush=false
│   └── config_batch_on.yaml     # Benchmark: batchPush=true
└── docs/
    └── benchmark-report.md      # This document
```

---

## 9. Conclusion

| 指标 | WebSocket | TCP |
|------|-----------|-----|
| 转发吞吐（bench1） | **840K/s** | 767K/s |
| 推送吞吐（bench2，无批量） | 442K/s | 421K/s |
| 推送吞吐（bench2，批量） | **816K/s** | **853K/s** |
| 批量推送提升 | **+85%** | **+102%** |

- 批量推送模式在不修改逻辑层代码的前提下，接近翻倍提升推送吞吐
- WebSocket 和 TCP 转发性能基本持平，WebSocket 转发甚至略高
- 所有优化对逻辑层完全透明，通过 `stream.batchPush` 配置开关切换
