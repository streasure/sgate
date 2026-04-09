# SGate 网关服务

SGate 是一个高性能、可扩展的网关服务，支持 TCP、UDP 和 WebSocket 协议，使用 Protocol Buffers 作为通信协议，提供用户连接管理和推送组功能。

## 功能特性

- **多协议支持**：支持 TCP、UDP 和 WebSocket 协议
- **Protocol Buffers**：使用 Protocol Buffers 作为通信协议，提高序列化和反序列化性能
- **用户连接管理**：支持连接和用户 UUID 的映射，建立连接时初始化，断开连接时删除
- **推送组功能**：支持用户绑定到推送组，向组推送消息时，组内所有用户都会收到
- **路由系统**：支持注册和处理不同路由的消息
- **速率限制**：支持基于 IP 的速率限制，防止恶意请求
- **白名单和黑名单**：支持 IP 白名单和黑名单功能
- **性能优化**：通过对象池、零拷贝等技术提高性能

## 目录结构

```
sgate/
├── gateway/           # 核心网关代码
│   ├── protobuf/      # Protocol Buffers 定义
│   ├── auth.go        # 认证处理
│   ├── cache.go       # 缓存管理
│   ├── connection.go  # 连接管理
│   ├── gateway.go     # 网关核心逻辑
│   ├── load_balancer.go  # 负载均衡
│   ├── rate_limiter.go  # 速率限制
│   ├── route.go       # 路由管理
│   ├── websocket.go   # WebSocket 处理
│   └── whitelist_blacklist.go  # 白名单和黑名单管理
├── examples/          # 示例代码
│   ├── high_concurrency_gateway/  # 高并发网关示例
│   │   ├── config/    # 配置文件
│   │   └── main.go    # 主入口文件
│   ├── loadtest/      # 压测工具
│   │   └── load_client.go  # 压测客户端
│   └── test/          # 测试客户端
│       └── test_client.go  # 测试客户端
├── internal/          # 内部包
│   └── config/        # 配置管理
│       └── config.go   # 配置加载和管理
├── metrics/           # 指标收集
│   └── metrics.go     # 指标收集和管理
├── tests/             # 测试文件
│   └── gateway_test.go  # 网关测试
├── config/            # 配置文件
│   ├── config.yaml    # 主配置文件
│   └── logs.yaml      # 日志配置文件
├── bin/               # 可执行文件
├── .github/           # GitHub 配置
├── Dockerfile         # Docker 配置
├── docker-compose.yml # Docker Compose 配置
├── README.md          # 说明文档
├── deploy.sh          # 部署脚本
├── go.mod             # Go 模块文件
├── go.sum             # Go 依赖校验文件
├── performance_test.md  # 性能测试报告
├── 上线功能补全方案.md  # 上线功能补全方案
└── 实施计划.md        # 实施计划
```

## 核心功能

### 1. 连接管理

- **添加连接**：建立连接时，生成临时用户 UUID，并将连接添加到连接管理器
- **删除连接**：断开连接时，从连接管理器中移除连接，并清理用户映射
- **更新用户连接**：当收到用户 UUID 时，更新连接的用户映射
- **广播消息**：向所有连接广播消息

### 2. 推送组管理

- **创建组**：创建一个推送组
- **删除组**：删除一个推送组
- **添加用户到组**：将用户添加到推送组
- **从组中移除用户**：从推送组中移除用户
- **向组推送消息**：向推送组中的所有用户推送消息

### 3. 路由系统

- **注册路由**：注册路由处理函数
- **处理路由**：根据路由名称调用对应的处理函数
- **默认路由**：内置 ping、health 等默认路由

### 4. 协议处理

- **TCP 处理**：处理 TCP 连接的消息
- **UDP 处理**：处理 UDP 连接的消息
- **WebSocket 处理**：处理 WebSocket 连接的消息
- **HTTP 处理**：处理 HTTP 请求

## 快速开始

### 1. 安装依赖

```bash
go mod tidy
```

### 2. 编译 Protocol Buffers

使用项目提供的编译脚本编译Protocol Buffers：

```bash
cd gateway/protobuf
./compile_proto.bat
```

脚本会使用项目中的protoc可执行文件来生成pb.go文件。

### 3. 运行示例

```bash
cd demo/high_concurrency_gateway
go run main.go
```

### 4. 测试

```bash
cd example/loadtest
go run load_client.go
```

## 配置说明

配置文件位于 `examples/high_concurrency_gateway/config/config.yaml`，主要配置项包括：

- **server**：服务器配置
  - **port**：服务器端口
  - **logLevel**：日志级别

- **protocols**：协议配置
  - **protocol**：协议类型（tcp/udp）
  - **port**：端口号
  - **type**：类型（如 websocket）

- **workerPool**：工作池配置
  - **minWorkers**：最小工作线程数
  - **maxWorkers**：最大工作线程数
  - **queueSize**：队列大小
  - **queueSizeThreshold**：队列大小阈值

- **rateLimiter**：速率限制配置
  - **rate**：速率限制
  - **window**：时间窗口

- **security**：安全配置
  - **authSecret**：认证密钥
  - **authRoutes**：需要认证的路由

- **alerts**：告警配置
  - **activeConnectionsThreshold**：活跃连接数阈值
  - **failedMessagesThreshold**：失败消息数阈值
  - **processingTimeThreshold**：处理时间阈值
  - **redisErrorsThreshold**：Redis 错误数阈值
  - **queueLengthThreshold**：队列长度阈值

## 示例代码

### 1. 创建网关实例

```go
import (
	"github.com/streasure/sgate/gateway"
	"github.com/streasure/sgate/gateway/protobuf"
	"time"
)

func main() {
	// 创建网关实例
	gw := gateway.NewGateway()
	
	// 注册自定义路由
	gw.routeManager.RegisterRoute("custom", func(connectionID string, payload map[string]string, callback func(*protobuf.Message)) {
		callback(&protobuf.Message{
			Route: "customResponse",
			Payload: map[string]string{
				"message":   "Hello from custom route",
				"timestamp": time.Now().UnixMilli(),
			},
		})
	})
	
	// 启动服务器
	// ...
}
```

### 2. 发送消息

#### TCP 客户端

```go
import (
	"net"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

func main() {
	// 连接到服务器
	conn, err := net.Dial("tcp", "localhost:8083")
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	
	// 发送消息
	message := &protobuf.Message{
		Route:    "ping",
		UserUuid: "user123",
		Payload:  map[string]string{},
	}
	data, _ := proto.Marshal(message)
	conn.Write(data)
	
	// 接收响应
	buffer := make([]byte, 1024)
	n, _ := conn.Read(buffer)
	response := &protobuf.Message{}
	proto.Unmarshal(buffer[:n], response)
	fmt.Println("Received response:", response)
}
```

#### HTTP 客户端

```bash
# 使用Protocol Buffers格式发送请求
# 注意：需要使用工具或代码生成Protocol Buffers格式的数据
```

### 3. 使用推送组

```go
// 创建组
connectionManager.CreateGroup("group1")

// 添加用户到组
connectionManager.AddUserToGroup("user123", "group1")

// 向组推送消息
message := &protobuf.Message{
	Route: "broadcast",
	Payload: map[string]string{
		"message": "Hello from group",
	},
}
connectionManager.SendToGroup("group1", message)

// 从组中移除用户
connectionManager.RemoveUserFromGroup("user123", "group1")

// 删除组
connectionManager.DeleteGroup("group1")
```

## 性能优化

### 1. 对象池

使用对象池复用消息对象和错误响应对象，减少内存分配：

- **消息对象池**：复用 `Message` 对象
- **错误响应对象池**：复用错误响应映射
- **HTTP 响应缓冲区池**：复用 HTTP 响应缓冲区

### 2. 零拷贝技术

使用 gnet 的 `Next` 方法读取数据，避免数据拷贝：

```go
data, err := c.Next(-1) // 读取所有可用数据
```

### 3. 锁优化

- **读写锁**：使用 `sync.RWMutex` 保护路由映射
- **减少锁持有时间**：限制锁的作用域，减少锁的持有时间

### 4. 并发优化

- **工作池**：使用工作池处理消息，提高并发处理能力
- **动态调整工作线程数**：根据负载动态调整工作线程数

### 5. 日志优化

- **减少调试日志**：在生产环境中减少调试日志的输出
- **结构化日志**：使用结构化日志，便于分析和监控

## 部署说明

### 1. 编译

```bash
go build -o sgate main.go
```

### 2. 运行

```bash
./sgate
```

### 3. 环境变量

| 环境变量 | 说明 | 默认值 |
|---------|------|--------|
| `CONFIG_PATH` | 配置文件路径 | `config/config.yaml` |
| `LOG_LEVEL` | 日志级别 | `info` |

## 监控和告警

### 1. 指标收集

- **连接指标**：总连接数、活跃连接数
- **消息指标**：接收消息数、处理消息数、失败消息数
- **性能指标**：平均处理时间、最大处理时间
- **队列指标**：队列长度

### 2. 告警

当指标超过阈值时，会触发告警：

- **活跃连接数告警**：当活跃连接数超过阈值时
- **失败消息数告警**：当失败消息数超过阈值时
- **处理时间告警**：当处理时间超过阈值时
- **Redis 错误数告警**：当 Redis 错误数超过阈值时
- **队列长度告警**：当队列长度超过阈值时

## 安全最佳实践

### 1. 认证和授权

- **JWT 认证**：使用 JWT 令牌进行认证
- **路由认证**：为敏感路由配置认证

### 2. 速率限制

- **IP 速率限制**：限制每个 IP 的请求速率
- **用户速率限制**：限制每个用户的请求速率

### 3. 输入验证

- **XSS 防护**：清理输入中的 HTML 特殊字符
- **SQL 注入防护**：验证输入，防止 SQL 注入攻击
- **命令注入防护**：验证输入，防止命令注入攻击

### 4. 白名单和黑名单

- **IP 白名单**：只允许白名单中的 IP 访问
- **IP 黑名单**：禁止黑名单中的 IP 访问

## 故障排查

### 1. 常见错误

| 错误 | 原因 | 解决方案 |
|------|------|----------|
| `Route not found` | 路由不存在 | 检查路由名称是否正确，确保路由已注册 |
| `Invalid message format` | 消息格式错误 | 检查消息格式是否正确，确保消息是有效的 Protocol Buffers 格式 |
| `Rate limit exceeded` | 速率限制 | 减少请求频率，或调整速率限制配置 |
| `IP address is blacklisted` | IP 在黑名单中 | 检查 IP 是否在黑名单中，如需要，将其从黑名单中移除 |

### 2. 日志分析

- **错误日志**：查看错误日志，了解错误原因
- **性能日志**：查看性能日志，了解系统性能
- **连接日志**：查看连接日志，了解连接状态

## 版本说明

| 版本 | 特性 |
|------|------|
| v1.0.0 | 初始版本，支持 TCP、UDP 和 WebSocket 协议，使用 Protocol Buffers 作为通信协议，提供用户连接管理和推送组功能 |

## 贡献指南

1. Fork 仓库
2. 创建分支
3. 提交代码
4. 运行测试
5. 提交 Pull Request

## .gitignore 文件

本项目包含一个 `.gitignore` 文件，用于忽略以下类型的文件：

- **构建产物**：`*.exe`、`*.dll`、`*.so` 等
- **日志文件**：`*.log`、`logs/` 目录
- **临时文件**：`*.tmp`、`*.temp` 等
- **编辑器文件**：`.vscode/`、`.idea/`、`*.swp` 等
- **依赖目录**：`vendor/`
- **测试覆盖率文件**：`coverage.out`
- **环境变量文件**：`.env`
- **配置文件**：`config/*.yaml`（除了 `config/config.yaml.example`）
- **构建目录**：`build/`、`bin/`
- **文档生成目录**：`docs/_build/`
- **操作系统文件**：`Thumbs.db`、`.DS_Store`
- **其他无需上传的文件**：`*.backup`、`*.bak`、`*.old`、`*.orig`

这确保了只有必要的源代码和配置文件被提交到版本控制系统中，减少了仓库的体积，提高了代码的可维护性。

## 许可证

MIT License


