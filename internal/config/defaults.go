package config

import "time"

// ============================================================================
// sgate 默认常量
// ----------------------------------------------------------------------------
// 用途：把"不常用/设置一次基本不改"的参数集中到代码常量，作为 yaml 缺省值的参考。
//
// 使用方法：
//   1) 这些常量是文档化的默认值参考；Load 使用 uconfig 合并语义，
//      yaml 中显式出现的字段覆盖默认，未出现的字段保持 uconfig 解析零值。
//      Validate 通过后调用 ApplyRuntimeDefaults 对零值字段统一补齐默认。
//   2) 如需调整某个"不常用"参数（如 gRPC 窗口大小、限流阈值），直接改本文件常量
//      或在 yaml 中显式覆盖（推荐后者，便于环境差异化）
//
// 分类：
//   - 网络与协议常量（gRPC 窗口、stream 队列、帧大小）
//   - 过载保护常量（检查间隔、超时）
//   - 安全防护常量（限流阈值、熔断阈值、WAF 上限）
//   - 发现 / etcd 常量
//   - 组件运维常量（追踪采样率、告警频控、队列容量）
// ============================================================================

// --- 网络与协议 ---

const (
	// DefaultGRPCPort gRPC 服务默认监听端口
	DefaultGRPCPort = 50051

	// DefaultGatewayGRPCWindowSize 网关零值回退：历史运行时默认（压测口径，勿改）
	DefaultGatewayGRPCWindowSize = 524288

	// DefaultGatewayGRPCMaxMessageSize 网关零值回退：历史运行时默认（压测口径，勿改）
	DefaultGatewayGRPCMaxMessageSize = 4 * 1024 * 1024

	// DefaultGatewayStreamSendChannelSize 网关零值回退：历史运行时默认（压测口径，勿改）
	// backend 分片发送队列容量的零值回退同源于此常量
	DefaultGatewayStreamSendChannelSize = 65536

	// DefaultPipelineWorkerQueueSize 异步 pipeline worker 任务队列总大小
	// （全池共享，启动时按分片数均分；A/B 实测同步路径更优，仅异步模式生效）
	DefaultPipelineWorkerQueueSize = 65536

	// DefaultStreamQueuePolicy 队列满时默认策略：丢弃最旧消息
	DefaultStreamQueuePolicy = "drop"

	// DefaultStreamQueueMaxSize 重连缓冲队列最大容量
	DefaultStreamQueueMaxSize = 100000

	// DefaultStreamBlockTimeout 阻塞/限时等待模式的超时时间
	// （time.Duration 强类型：yaml 缺省回填写 .String()，代码零值回退直接引用）
	DefaultStreamBlockTimeout = 500 * time.Millisecond

	// DefaultBackpressureThreshold 背压触发阈值（队列填充率 0.0-1.0）
	DefaultBackpressureThreshold = 0.8

	// DefaultSendTimeout 分片发送通道超时时间
	// （time.Duration 强类型：yaml 缺省回填写 .String()，代码零值回退直接引用）
	DefaultSendTimeout = 200 * time.Millisecond

	// DefaultMaxFrameSize 单帧 payload 上限（TCP）
	// protobuf 业务消息一般 < 4KB；4MB 足够大业务消息
	DefaultMaxFrameSize = 4 * 1024 * 1024

	// DefaultMaxWSFrameSize 单帧 payload 上限（WebSocket）
	DefaultMaxWSFrameSize = 4 * 1024 * 1024
)

// --- 过载保护 ---

const (
	// DefaultOverloadCheckIntervalMs 过载检查间隔（毫秒）
	// 200ms 检查一次 CPU/内存水位，平衡灵敏度与开销
	DefaultOverloadCheckIntervalMs = 200

	// DefaultOverloadCPUThreshold 过载 CPU 阈值（百分比）
	DefaultOverloadCPUThreshold = 90.0

	// DefaultWSHeartbeatTimeoutSec WebSocket 心跳超时（秒）
	DefaultWSHeartbeatTimeoutSec = 60

	// DefaultWSCheckIntervalSec WebSocket 健康检查间隔（秒）
	DefaultWSCheckIntervalSec = 30

	// DefaultConnCheckInterval 连接清理检查间隔
	// 周期扫描空闲连接，超时则关闭
	DefaultConnCheckInterval = "5m"

	// DefaultConnIdleTimeout 连接空闲超时（无数据则断开）
	// 百万连接场景适当放宽，避免频繁重连
	DefaultConnIdleTimeout = "5m"
)

// --- 安全防护 ---

const (
	// DefaultRateLimitMaxTokens 每秒令牌数（按 IP/路由）
	// 下扫压测：fwd/s 随 maxTokens 线性扩展至膝点 ~600K（单 IP 独占机器容量上限），
	// 之后满吞吐零丢弃。默认 10000 覆盖 NAT 后多客户端共享 IP 的
	// 合理突发（~100 连接 × 100 msg/s），同时硬顶异常单 IP；高吞吐部署按需上调。
	// 全局硬顶 = maxTokens*10；路由上限 = routeMaxTokens（0=maxTokens*4）。
	DefaultRateLimitMaxTokens = 10000

	// DefaultRateLimitTokenRefresh 令牌桶补充周期
	DefaultRateLimitTokenRefresh = "1s"

	// DefaultCircuitBreakerFailureThreshold 连续失败次数触发熔断
	DefaultCircuitBreakerFailureThreshold = 5

	// DefaultCircuitBreakerSuccessThreshold 半开状态连续成功次数恢复
	DefaultCircuitBreakerSuccessThreshold = 3

	// DefaultCircuitBreakerTimeout 熔断打开后等待恢复时间
	DefaultCircuitBreakerTimeout = "30s"

	// DefaultWAFMaxPayloadSize WAF 单帧 payload 上限（防大包攻击）
	DefaultWAFMaxPayloadSize = 1 * 1024 * 1024 // 1MB

	// DefaultWAFBlockAction WAF 命中后的动作（drop=断连，log=仅记录）
	DefaultWAFBlockAction = "drop"
)

// --- 发现 / 集群 ---

const (
	// DefaultEtcdServicePrefix etcd key 前缀
	DefaultEtcdServicePrefix = "/services"

	// DefaultEtcdLeaseTTL 注册 lease TTL
	DefaultEtcdLeaseTTL = "10s"

	// DefaultStreamShardCount 网关↔logic 流分片数（压测口径 96）
	DefaultStreamShardCount = 96

	// DefaultStreamConnGroupCount gateway→logic 独立 TCP 连接组数
	DefaultStreamConnGroupCount = 4

	// DefaultPerfGcPercent GOGC
	DefaultPerfGcPercent = 300

	// DefaultPerfMemoryLimitPercent GOMEMLIMIT 占总内存百分比
	DefaultPerfMemoryLimitPercent = 80
)

// --- JWT 鉴权 ---

const (
	// DefaultJWTHeaderField JWT token 所在 header 字段名
	DefaultJWTHeaderField = "X-Auth-Token"
)

// --- 登录校验周期安全网 ---

const (
	// DefaultLoginValidationBatchInterval BatchValidate 周期安全网间隔
	// 收口 mid-login 竞态与会话缓存 TTL 过期踢不中；60s 兼顾及时性与 loginserver 压力
	DefaultLoginValidationBatchInterval = "60s"

	// DefaultLoginValidationBatchSize 单次批量校验条数（loginserver 硬上限 500，留余量）
	DefaultLoginValidationBatchSize = 200
)

// --- 流量镜像 ---

const (
	// DefaultMirrorQueueSize 镜像异步队列大小
	// 队列满则丢弃镜像样本（不影响主流量）
	DefaultMirrorQueueSize = 1024

	// DefaultMirrorWorkers 镜像异步发送 worker 数
	DefaultMirrorWorkers = 2
)

// --- OpenTelemetry 追踪 ---

const (
	// DefaultOTelServiceName 追踪服务名（在 Zipkin/Jaeger UI 中展示）
	DefaultOTelServiceName = "sgate"

	// DefaultOTelSampleRate 采样率倒数（100 = 每 100 个请求采 1 个）
	// 千万 QPS 下采样率应调高（如 1000/10000），避免上报打满 collector
	DefaultOTelSampleRate = 100

	// DefaultOTelQueueSize span 异步上报队列大小
	DefaultOTelQueueSize = 1024

	// DefaultOTelWorkers span 上报 worker 数
	DefaultOTelWorkers = 2
)

// --- 告警 ---

const (
	// DefaultAlertRateLimitPerMin 每分钟最大告警数（防止告警风暴）
	DefaultAlertRateLimitPerMin = 30
)

// --- 协议 / 预鉴权 ---

const (
	// DefaultPreAuthCommands 绑定 serverID 前放行的命令（LoginGate）
	DefaultPreAuthCommands = 1000001
)
