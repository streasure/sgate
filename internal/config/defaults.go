package config

// ============================================================================
// sgate 默认常量
// ----------------------------------------------------------------------------
// 用途：把"不常用/设置一次基本不改"的参数集中到代码常量，从 yaml 中移除
//
// 使用方法：
//   1) 这些常量由 loadDefaultConfig() 注入默认配置
//   2) LoadConfig() 解析 yaml 时采用"合并"语义：
//      - yaml 中显式出现的字段，覆盖默认值
//      - yaml 中未出现的字段，保留这里的默认值
//      因此 yaml 文件可以非常精简，只保留环境相关 + 运营策略字段
//   3) 如需调整某个"不常用"参数（如 gRPC 窗口大小、限流阈值），直接改本文件常量
//      或在 yaml 中显式覆盖（推荐后者，便于环境差异化）
//
// 分类：
//   - 网络与协议常量（gRPC 窗口、stream 队列、帧大小）
//   - 过载保护常量（检查间隔、超时）
//   - 安全防护常量（限流阈值、熔断阈值、WAF 上限）
//   - 集群常量（Leader 锁 key、TTL）
//   - 组件运维常量（追踪采样率、告警频控、队列容量）
// ============================================================================

// --- 网络与协议 ---

const (
	// DefaultGRPCWindowSize gRPC flow-control 窗口大小（字节）
	// 调大可提升吞吐，但占用内存更多。一般 16MB 足够千万 QPS
	DefaultGRPCWindowSize = 16 * 1024 * 1024

	// DefaultGRPCMaxMessageSize 单条 gRPC 消息上限（字节）
	// 批量转发场景按 batchSize × frameSize 估算，留 2x 余量
	DefaultGRPCMaxMessageSize = 8 * 1024 * 1024

	// DefaultStreamSendChannelSize 正向 shard 发送队列容量
	// 队列满则触发 ErrNotConnected 快速失败，防止背压堆积
	DefaultStreamSendChannelSize = 131072 // 128K

	// DefaultStreamReceiveBatchSize 反向接收批量大小（每批 ACK 数）
	DefaultStreamReceiveBatchSize = 64

	// DefaultMaxFrameSize 单帧 payload 上限（TCP/UDP）
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

	// DefaultWSHeartbeatTimeoutSec WebSocket 心跳超时（秒）
	DefaultWSHeartbeatTimeoutSec = 60

	// DefaultWSCheckIntervalSec WebSocket 健康检查间隔（秒）
	DefaultWSCheckIntervalSec = 30

	// DefaultConnCheckInterval 连接清理检查间隔
	// 周期扫描空闲连接，超时则关闭
	DefaultConnCheckInterval = "5m"

	// DefaultConnIdleTimeout 连接空闲超时（无数据则断开）
	DefaultConnIdleTimeout = "30s"
)

// --- 安全防护 ---

const (
	// DefaultRateLimitMaxTokens 每秒令牌数（per IP/route）
	// 10000 = 单 IP 每秒最多 1 万请求，可按业务调整
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

// --- 集群 ---

const (
	// DefaultClusterServiceName 网关节点注册到 Nacos 的服务名
	// 集群 Leader 选举基于该服务的实例列表：同 zone 内按 ip:port 字典序排序，排名第一者为 Leader
	DefaultClusterServiceName = "sgate-gateway"

	// DefaultClusterLockTTL 集群心跳/选举 TTL
	// 必须 > renewInterval（TTL/3），否则续期不及时丢主
	DefaultClusterLockTTL = "10s"
)

// --- 负载均衡 ---

const (
	// DefaultBalancerFailureThreshold 连续失败次数后摘除节点
	DefaultBalancerFailureThreshold = 3

	// DefaultBalancerRecoverInterval 故障节点恢复探测间隔
	DefaultBalancerRecoverInterval = "30s"
)

// --- JWT 鉴权 ---

const (
	// DefaultJWTHeaderField JWT token 所在 header 字段名
	DefaultJWTHeaderField = "X-Auth-Token"
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

// --- 配置中心 ---

const (
	// DefaultConfigCenterPollInterval 配置中心拉取间隔
	// Nacos/Apollo 长轮询可设更短；etcd watch 模式可设长一些
	DefaultConfigCenterPollInterval = "5s"
	// DefaultConfigCenterAPIVersion Nacos API 版本
	// "v3" = Nacos 3.x（路径 /nacos/v3/admin/cs/config，响应 JSON 包装）
	// "v1" = Nacos 2.x（路径 /nacos/v1/cs/configs，响应纯文本）
	DefaultConfigCenterAPIVersion = "v3"
)

// --- 告警 ---

const (
	// DefaultAlertRateLimitPerMin 每分钟最大告警数（防止告警风暴）
	DefaultAlertRateLimitPerMin = 30
)

// --- 监控接入 ---

const (
	// DefaultPrometheusAddr sgate Prometheus 指标端点监听地址
	// 使用方法：在 config.yaml 的 monitoring.prometheus.addr 字段覆盖
	// 注意：默认 :9100 是为了让出 Prometheus 自身默认的 :9090，
	//       这样 Prometheus + Grafana 都可用各自默认端口启动，无需 --web.listen-address 参数
	DefaultPrometheusAddr = ":9100"

	// DefaultPrometheusPath Prometheus 指标路径
	// 使用方法：Prometheus scrape_config 的 metrics_path 必须与此一致
	DefaultPrometheusPath = "/metrics"

	// DefaultPrometheusPrefix Prometheus 指标前缀
	// 使用方法：所有指标自动加此前缀（如 app_connections_total）
	DefaultPrometheusPrefix = "app"
)

// --- 灰度 ---

const (
	// DefaultCanaryPercent 灰度默认百分比（0 = 关闭）
	DefaultCanaryPercent = 0
)
