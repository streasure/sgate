# SGate 高性能网关使用指南

## 目录

1. [简介](#简介)
2. [快速开始](#快速开始)
3. [性能优化配置](#性能优化配置)
4. [压测工具使用](#压测工具使用)
5. [协议说明](#协议说明)
6. [配置详解](#配置详解)
7. [示例代码](#示例代码)
8. [性能指标](#性能指标)

## 简介

SGate 是一个基于 gnet v2 的高性能网关服务，支持 TCP、UDP 和 WebSocket 三种协议。

### 核心特性

- **多协议支持**: TCP、UDP、WebSocket 同时支持
- **高性能**: 基于 gnet 事件驱动架构，支持多核并发
- **高吞吐量**: 实测 QPS 达到 100,000+
- **低延迟**: 平均延迟 < 3ms，P99 延迟 < 12ms
- **可扩展**: 模块化设计，支持自定义路由
- **高可靠性**: 100% 压测成功率

## 快速开始

### 1. 编译项目

```bash
go build -o sgate.exe ./examples/high_concurrency_gateway
```

### 2. 编译压测工具

```bash
go build -o loadtest.exe ./loadtest.go
```

### 3. 启动服务器

```bash
./sgate.exe
```

服务器默认监听：
- **TCP**: 48080
- **UDP**: 48081
- **WebSocket**: 48082

### 4. 运行压测

```bash
./loadtest.exe
```

## 性能优化配置

### 已启用的性能优化

#### 1. 多核模式
```go
gnet.WithMulticore(true)  // 启用多核模式，充分利用 CPU
```

#### 2. 端口复用
```go
gnet.WithReusePort(true)  // 启用端口复用，提高并发能力
```

#### 3. 禁用 Nagle 算法
```go
gnet.WithTCPNoDelay(gnet.TCPNoDelay)  // 禁用 Nagle 算法，降低延迟
```

#### 4. 缓冲区优化
```go
// 读取/写入缓冲区：64KB
gnet.WithReadBufferCap(65536)
gnet.WithWriteBufferCap(65536)

// 系统 socket 缓冲区：256KB
gnet.WithSocketRecvBuffer(262144)
gnet.WithSocketSendBuffer(262144)
```

#### 5. 工作池动态调整
```yaml
workerPool:
  minWorkers: 0  # 动态：CPU * 4
  maxWorkers: 0  # 动态：CPU * 16
  queueSize: 5000000
```

## 压测工具使用

### 压测配置参数

```go
config := &StressConfig{
    TCPConcurrency:  100,              // TCP 并发连接数
    UDPConcurrency:  100,              // UDP 并发连接数
    WSConcurrency:   100,              // WebSocket 并发连接数
    RequestsPerConn: 100,              // 每个连接的请求数
    ServerAddr:     "localhost",        // 服务器地址
    Timeout:        10 * time.Second,  // 请求超时时间
    MessageSize:    100,                // 消息大小
}
```

### 压测指标说明

- **QPS**: 每秒查询率
- **成功率**: 成功请求数 / 总请求数
- **平均延迟**: 所有请求响应时间的平均值
- **P95 延迟**: 95% 请求的响应时间
- **P99 延迟**: 99% 请求的响应时间
- **最小/最大延迟**: 最快和最慢的响应时间

## 协议说明

### 消息格式

使用 Protocol Buffers 序列化，消息结构：

```protobuf
message Message {
    string connection_id = 1;    // 连接 ID
    string route = 2;            // 路由
    map<string, string> payload = 3;  // 负载
    string checksum = 4;        // 校验和
}
```

### 握手流程

1. 客户端连接服务器
2. 客户端发送握手消息（包含协议版本等信息）
3. 服务器返回握手响应
4. 开始业务消息交互

### WebSocket 特殊处理

WebSocket 连接需要额外的 HTTP 握手：

```
GET /ws HTTP/1.1
Host: localhost:48082
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13
```

## 配置详解

### 配置文件位置

`config/config.yaml`

### 完整配置项

```yaml
# 服务配置
port: 48080
logLevel: info

# 支持的协议列表
transports:
  - protocol: tcp
    port: 48080
  - protocol: udp
    port: 48081
  - protocol: tcp
    port: 48082
    type: websocket

# 网络配置（性能优化）
network:
  tcpKeepAlive: 5m
  readBufferCapBytes: 65536    # 64KB
  writeBufferCapBytes: 65536   # 64KB
  socketRecvBuffer: 262144     # 256KB
  socketSendBuffer: 262144     # 256KB
  eventLoopCount: 0            # 0=自动检测 CPU 核心数
  reusePort: true              # 端口复用
  tcpNoDelay: true             # 禁用 Nagle 算法

# 工作池配置（性能优化）
workerPool:
  minWorkers: 0               # 0=CPU核心数*4
  maxWorkers: 0                # 0=CPU核心数*16
  queueSize: 5000000           # 500 万队列容量
  queueSizeThreshold: 10000    # 队列阈值

# 速率限制器配置
rateLimiter:
  rate: 1000000
  burst: 2000000
  window: 1s

# 安全配置
security:
  authSecret: "your_secret_key"
  authRoutes:
    - getConnections
    - broadcast
  enableTLS: false

# 资源限制配置
resources:
  memoryThreshold: 95.0
  cpuThreshold: 95.0
  enableResourceCircuitBreaker: true
  checkInterval: 5s
```

## 示例代码

### TCP 客户端示例

```go
package main

import (
    "fmt"
    "net"
    "time"

    "github.com/streasure/sgate/gateway/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    // 建立连接
    conn, err := net.Dial("tcp", "localhost:48080")
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    // 创建消息
    msg := &protobuf.Message{
        ConnectionId: "client-1",
        Route: "ping",
        Payload: map[string]string{
            "message": "Hello, World!",
        },
    }

    // 序列化并发送
    data, _ := proto.Marshal(msg)
    conn.Write(data)

    // 读取响应
    buf := make([]byte, 4096)
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    n, _ := conn.Read(buf)

    // 解析响应
    var response protobuf.Message
    proto.Unmarshal(buf[:n], &response)

    fmt.Printf("收到响应: %+v\n", response)
}
```

### UDP 客户端示例

```go
package main

import (
    "fmt"
    "net"
    "time"

    "github.com/streasure/sgate/gateway/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    // 建立连接
    conn, err := net.Dial("udp", "localhost:48081")
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    // 创建消息
    msg := &protobuf.Message{
        ConnectionId: "client-udp-1",
        Route: "ping",
        Payload: map[string]string{
            "message": "Hello, UDP!",
        },
    }

    // 序列化并发送
    data, _ := proto.Marshal(msg)
    conn.Write(data)

    // 读取响应
    buf := make([]byte, 4096)
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    n, _ := conn.Read(buf)

    // 解析响应
    var response protobuf.Message
    proto.Unmarshal(buf[:n], &response)

    fmt.Printf("收到响应: %+v\n", response)
}
```

### WebSocket 客户端示例

```go
package main

import (
    "fmt"
    "net"
    "time"

    "github.com/streasure/sgate/gateway/protobuf"
    "google.golang.org/protobuf/proto"
)

func main() {
    // 建立 TCP 连接
    conn, err := net.Dial("tcp", "localhost:48082")
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    // WebSocket 握手
    handshakeReq := "GET /ws HTTP/1.1\r\n" +
        "Host: localhost:48082\r\n" +
        "Upgrade: websocket\r\n" +
        "Connection: Upgrade\r\n" +
        "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
        "Sec-WebSocket-Version: 13\r\n\r\n"

    conn.Write([]byte(handshakeReq))

    // 读取握手响应
    buf := make([]byte, 4096)
    conn.Read(buf)

    // 创建消息
    msg := &protobuf.Message{
        ConnectionId: "client-ws-1",
        Route: "ping",
        Payload: map[string]string{
            "message": "Hello, WebSocket!",
        },
    }

    // 序列化消息
    data, _ := proto.Marshal(msg)

    // 封装为 WebSocket 帧并发送
    wsFrame := createWebSocketFrame(data)
    conn.Write(wsFrame)

    // 读取响应
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    n, _ := conn.Read(buf)

    // 解析 WebSocket 帧
    responseData := parseWebSocketFrame(buf[:n])

    // 解析响应
    var response protobuf.Message
    proto.Unmarshal(responseData, &response)

    fmt.Printf("收到响应: %+v\n", response)
}

func createWebSocketFrame(data []byte) []byte {
    frame := make([]byte, 0, len(data)+10)
    frame = append(frame, 0x81) // FIN=1, opcode=1 (text)

    if len(data) < 126 {
        frame = append(frame, byte(len(data)))
    } else if len(data) < 65536 {
        frame = append(frame, 126)
        frame = append(frame, byte(len(data)>>8), byte(len(data)))
    }

    frame = append(frame, data...)
    return frame
}

func parseWebSocketFrame(data []byte) []byte {
    if len(data) < 2 {
        return data
    }

    opcode := data[0] & 0x0F
    if opcode != 1 && opcode != 2 {
        return data
    }

    length := int(data[1] & 0x7F)
    offset := 2

    if length == 126 {
        length = int(data[2])<<8 | int(data[3])
        offset = 4
    }

    if data[1]&0x80 != 0 {
        offset += 4
    }

    return data[offset : offset+length]
}
```

## 性能指标

### 压测结果（100 并发 × 100 请求 × 3 协议）

```
【TCP 协议压测结果】
  总请求数: 10000
  成功请求数: 10000
  成功率: 100.00%
  QPS: 37047.71
  平均延迟: 2.15ms
  P95 延迟: 6.88ms
  P99 延迟: 11.13ms

【UDP 协议压测结果】
  总请求数: 10000
  成功请求数: 10000
  成功率: 100.00%
  QPS: 33011.83
  平均延迟: 2.75ms
  P95 延迟: 7.70ms
  P99 延迟: 11.57ms

【WebSocket 协议压测结果】
  总请求数: 10000
  成功请求数: 10000
  成功率: 100.00%
  QPS: 36911.03
  平均延迟: 2.24ms
  P95 延迟: 7.00ms
  P99 延迟: 11.00ms

【汇总】
  总请求数: 30000
  总成功请求数: 30000
  总成功率: 100.00%
  总 QPS: 106970.58
```

### 性能优化建议

1. **增加并发数**: 提高压测工具的并发连接数
2. **调整缓冲区大小**: 根据消息大小调整缓冲区
3. **启用压缩**: 在高延迟网络中使用压缩
4. **连接池**: 复用连接减少连接建立开销

## 常见问题

### 1. 连接被拒绝

检查服务器是否启动，端口是否被占用：

```bash
netstat -ano | findstr 48080
```

### 2. 压测成功率低

检查网络延迟和服务器负载，确保压测工具和服务器在同一网络环境中。

### 3. 性能不佳

- 检查 CPU 和内存使用情况
- 调整工作池大小
- 增加缓冲区大小

## 技术支持

如有问题，请提交 Issue 或联系开发团队。
