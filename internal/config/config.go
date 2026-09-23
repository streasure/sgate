package config

import (
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/streasure/util/uconfig"
)

// Config 网关服务的完整配置结构体，包含所有模块配置
type Config struct {
	Port            int                 `yaml:"port"`
	LogLevel        string              `yaml:"logLevel"`
	Belong          string              `yaml:"belong"` // 所属应用/团队标识，与 serverType+zone+serverId 唯一确定一个服务
	ServerID        string              `yaml:"serverId"`
	ServerType      string              `yaml:"serverType"`
	Zone            string              `yaml:"zone"`
	Discovery       DiscoveryConfig     `yaml:"discovery"`
	Transports      []Transport         `yaml:"transports"`
	GRPC            GRPCConfig          `yaml:"grpc"`
	LogicServerType string              `yaml:"logicServerType"`
	Etcd            EtcdConfig          `yaml:"etcd"`
	Stream          StreamConfig        `yaml:"stream"`
	Protection      ProtectionConfig    `yaml:"protection"`
	Security        SecurityConfig      `yaml:"security"`
	WAF             WAFConfig           `yaml:"waf"`
	TLS             TLSConfig           `yaml:"tls"`
	Cluster         ClusterConfig       `yaml:"cluster"`
	Balancer        BalancerConfig      `yaml:"balancer"`
	JWTAuth         JWTAuthConfig       `yaml:"jwtAuth"`
	Canary          CanaryConfig        `yaml:"canary"`
	TrafficMirror   TrafficMirrorConfig `yaml:"trafficMirror"`
	OTelTracer      OTelTracerConfig    `yaml:"otelTracer"`
	ConfigCenter    ConfigCenterConfig  `yaml:"configCenter"`
	Alert           AlertWebhookConfig  `yaml:"alert"`
	Degradation     DegradationConfig   `yaml:"degradation"`
	FilterChain     FilterChainConfig   `yaml:"filterChain"`
	Monitoring      MonitoringConfig    `yaml:"monitoring"`
	Perf            PerfConfig          `yaml:"perf"`
	Pipeline        PipelineConfig      `yaml:"pipeline"`
}

// PipelineConfig Pipeline 异步化配置
type PipelineConfig struct {
	AsyncEnabled   bool `yaml:"asyncEnabled"`   // 启用异步 pipeline（worker pool 模式）
	WorkerShards   int  `yaml:"workerShards"`   // worker 分片数（默认 CPU×4）
	WorkerQueueSize int  `yaml:"workerQueueSize"` // 每个分片的任务队列大小
}

// PerfConfig 运行时性能调优参数（GC、内存限制），参数以机器资源百分比表示，跨机器通用。
type PerfConfig struct {
	GcPercent          int `yaml:"gcPercent"`          // GOGC 等价：堆增长触发 GC 的百分比（默认 100）
	MemoryLimitPercent int `yaml:"memoryLimitPercent"` // GOMEMLIMIT 等价：软内存上限占总内存百分比（推荐 80-90）
}

// Validate 校验配置参数的合法性，返回错误信息
func (c *Config) Validate() error {
	if c.GRPC.Port <= 0 || c.GRPC.Port > 65535 {
		return fmt.Errorf("grpc.port must be between 1 and 65535")
	}
	seen := make(map[int]bool, len(c.Transports))
	for _, transport := range c.Transports {
		if transport.Protocol != "tcp" {
			return fmt.Errorf("transport %d must use protocol tcp; UDP and other protocols are unsupported", transport.Port)
		}
		if transport.Port <= 0 || transport.Port > 65535 {
			return fmt.Errorf("transport port must be between 1 and 65535")
		}
		if seen[transport.Port] {
			return fmt.Errorf("duplicate transport port: %d", transport.Port)
		}
		seen[transport.Port] = true
		if transport.Type != "" && transport.Type != "websocket" {
			return fmt.Errorf("unsupported transport type %q", transport.Type)
		}
	}
	if c.TLS.Enabled {
		return fmt.Errorf("TLS/WSS is not supported by gnet v2 transport; disable tls.enabled")
	}
	return nil
}

// PortAddress 返回格式化的监听地址（如 ":8080"），端口无效时返回空字符串
func (c *Config) PortAddress() string {
	if c.Port <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(c.Port)
}

// MonitoringConfig 监控接入配置（可插拔）
// 通过 enabled 开关控制是否启动 Prometheus 指标服务
// 关闭时 sgate 单体也能正常运行，只是不暴露 /metrics 端点
type MonitoringConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
	PprofAddr  string           `yaml:"pprofAddr"`
}

// PrometheusConfig Prometheus 指标暴露配置
type PrometheusConfig struct {
	Enabled bool   `yaml:"enabled"` // 是否启动 /metrics 端点（关闭则 sgate 不暴露 Prometheus 指标）
	Addr    string `yaml:"addr"`    // 监听地址（如 :9090）
	Path    string `yaml:"path"`    // 指标路径（默认 /metrics）
	Prefix  string `yaml:"prefix"`  // 指标前缀（默认 "app"）
}

// BalancerConfig 负载均衡配置
type BalancerConfig struct {
	Algorithm        string `yaml:"algorithm"`        // 支持 roundRobin、weighted、leastConn、consistent
	FailureThreshold int    `yaml:"failureThreshold"` // 连续失败次数后摘除
	RecoverInterval  string `yaml:"recoverInterval"`  // 恢复探测间隔
}

// JWTAuthConfig JWT 鉴权配置
type JWTAuthConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Secret      string   `yaml:"secret"`
	Issuer      string   `yaml:"issuer"`
	HeaderField string   `yaml:"headerField"`
	SkipRoutes  []string `yaml:"skipRoutes"`
}

// CanaryConfig 灰度发布配置
type CanaryConfig struct {
	Enabled     bool              `yaml:"enabled"`
	Percent     int               `yaml:"percent"`
	Headers     map[string]string `yaml:"headers"`
	UserIDs     []string          `yaml:"userIDs"`
	TargetRoute string            `yaml:"targetRoute"`
}

// TrafficMirrorConfig 流量镜像配置
type TrafficMirrorConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Percent    int    `yaml:"percent"`
	TargetAddr string `yaml:"targetAddr"`
	QueueSize  int    `yaml:"queueSize"`
	Workers    int    `yaml:"workers"`
}

// OTelTracerConfig OpenTelemetry / Zipkin 分布式追踪配置
type OTelTracerConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"serviceName"`
	SampleRate  int    `yaml:"sampleRate"`
	QueueSize   int    `yaml:"queueSize"`
	Workers     int    `yaml:"workers"`
}

// ConfigCenterConfig 配置中心配置（保留用于基于 HTTP 的动态配置）
type ConfigCenterConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Type         string `yaml:"type"`
	Endpoint     string `yaml:"endpoint"`
	DataID       string `yaml:"dataID"`
	Group        string `yaml:"group"`
	Token        string `yaml:"token"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PollInterval string `yaml:"pollInterval"`
}

// EtcdConfig etcd 服务注册与发现配置
type EtcdConfig struct {
	Endpoints     []string `yaml:"endpoints"`
	Endpoint      string   `yaml:"endpoint"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	DialTimeout   string   `yaml:"dialTimeout"`
	ServicePrefix string   `yaml:"servicePrefix"`
	LeaseTTL      string   `yaml:"leaseTTL"`
}

// AlertWebhookConfig 告警 webhook 配置
type AlertWebhookConfig struct {
	Enabled   bool                `yaml:"enabled"`
	Webhooks  []WebhookItemConfig `yaml:"webhooks"`
	RateLimit int                 `yaml:"rateLimit"`
}

// WebhookItemConfig 单个 webhook 项
type WebhookItemConfig struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Type   string `yaml:"type"` // 支持 wecom、dingtalk、generic
	Secret string `yaml:"secret"`
}

// DegradationConfig 降级配置
type DegradationConfig struct {
	Enabled bool                    `yaml:"enabled"`
	Rules   []DegradationRuleConfig `yaml:"rules"`
}

// DegradationRuleConfig 降级规则
type DegradationRuleConfig struct {
	Route          string  `yaml:"route"`
	ErrorThreshold float64 `yaml:"errorThreshold"`
	WindowSize     int     `yaml:"windowSize"`
	FallbackData   string  `yaml:"fallbackData"`
	CoolDown       string  `yaml:"coolDown"`
}

// FilterChainConfig SPI 过滤器链配置
type FilterChainConfig struct {
	Enabled bool               `yaml:"enabled"`
	Filters []FilterItemConfig `yaml:"filters"`
}

// FilterItemConfig 单个过滤器配置
type FilterItemConfig struct {
	Name   string                 `yaml:"name"`
	Config map[string]interface{} `yaml:"config"`
}

// SecurityConfig 安全防护配置（白名单/黑名单/限流/熔断）
type SecurityConfig struct {
	Enabled        bool                 `yaml:"enabled"`
	Whitelist      []string             `yaml:"whitelist"`
	Blacklist      []string             `yaml:"blacklist"`
	RateLimit      RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuitBreaker"`
}

// RateLimitConfig 限流配置
type RateLimitConfig struct {
	Enabled      bool   `yaml:"enabled"`
	MaxTokens    int    `yaml:"maxTokens"`
	TokenRefresh string `yaml:"tokenRefresh"`
}

// CircuitBreakerConfig 熔断器配置
type CircuitBreakerConfig struct {
	Enabled          bool   `yaml:"enabled"`
	FailureThreshold int    `yaml:"failureThreshold"`
	SuccessThreshold int    `yaml:"successThreshold"`
	Timeout          string `yaml:"timeout"`
}

// WAFConfig Web应用防火墙配置
type WAFConfig struct {
	Enabled        bool     `yaml:"enabled"`
	SQLPatterns    []string `yaml:"sqlPatterns"`
	XSSPatterns    []string `yaml:"xssPatterns"`
	MaxPayloadSize int      `yaml:"maxPayloadSize"`
	BlockAction    string   `yaml:"blockAction"`
}

// TLSConfig TLS加密配置
type TLSConfig struct {
	Enabled    bool   `yaml:"enabled"`
	CertFile   string `yaml:"certFile"`
	KeyFile    string `yaml:"keyFile"`
	MinVersion string `yaml:"minVersion"`
}

// ClusterConfig 集群配置
type ClusterConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Mode           string `yaml:"mode"` // standalone: 单体模式（默认）, cluster: 集群协作模式
	NodeID         string `yaml:"nodeID"`
	LeaderElection bool   `yaml:"leaderElection"`
	LockTTL        string `yaml:"lockTTL"`
}

// DiscoveryConfig 服务发现配置
type DiscoveryConfig struct {
	Enabled          bool   `yaml:"enabled"`
	ServiceName      string `yaml:"serviceName"`
	Zone             string `yaml:"zone"`
	GatewayDiscovery bool   `yaml:"gatewayDiscovery"` // 启用网关间服务发现
	RegisterSelf     bool   `yaml:"registerSelf"`     // standalone模式下是否向etcd注册网关自身连接信息
}

// GRPCConfig gRPC 服务端配置
type GRPCConfig struct {
	Port           int `yaml:"port"`
	WindowSize     int `yaml:"windowSize"`
	MaxMessageSize int `yaml:"maxMessageSize"`
}

// QueuePolicy 发送队列满时的行为策略
type QueuePolicy string

const (
	// QueuePolicyDrop 队列满时丢弃最旧消息（默认策略）
	QueuePolicyDrop QueuePolicy = "drop"
	// QueuePolicyBlock 阻塞调用者直到队列有空间
	QueuePolicyBlock QueuePolicy = "block"
	// QueuePolicyTimeout 阻塞调用者最多等待 BlockTimeout 时长，然后返回错误
	QueuePolicyTimeout QueuePolicy = "timeout"
	// QueuePolicyBackpressure 当队列填充率超过阈值时返回错误，通知调用者减速
	QueuePolicyBackpressure QueuePolicy = "backpressure"
)

// StreamQueueConfig 分片发送队列和重连缓冲队列的配置
type StreamQueueConfig struct {
	// Policy 队列满时的策略：支持 "drop"（默认）、"block"、"timeout"、"backpressure"
	Policy QueuePolicy `yaml:"policy"`
	// MaxSize 重连缓冲队列的最大消息数
	MaxSize int `yaml:"maxSize"`
	// BlockTimeout 使用 "timeout" 策略时的最大等待时间（Go 时长格式，如 "500ms"、"2s"）
	BlockTimeout string `yaml:"blockTimeout"`
	// BackpressureThreshold 背压触发阈值（队列填充率 0.0-1.0），仅 "backpressure" 策略生效
	BackpressureThreshold float64 `yaml:"backpressureThreshold"`
	// SendTimeout 分片发送通道超时时间（Go 时长格式），超时后消息转入重连队列
	SendTimeout string `yaml:"sendTimeout"`
}

type StreamConfig struct {
	ShardCount       int               `yaml:"shardCount"`
	ConnGroupCount   int               `yaml:"connGroupCount"`   // gateway→logic 独立 TCP 连接组数（默认 4）
	SendChannelSize  int               `yaml:"sendChannelSize"`
	ReceiveBatchSize int               `yaml:"receiveBatchSize"`
	BatchPush        bool              `yaml:"batchPush"`
	QueuePolicy      StreamQueueConfig `yaml:"queuePolicy"`
}

type ProtectionConfig struct {
	MaxFrameSize        int             `yaml:"maxFrameSize"`
	MaxFrameBufSize     int             `yaml:"maxFrameBufSize"`
	MaxWSFrameSize      int             `yaml:"maxWSFrameSize"`
	MaxWSBufferSize     int             `yaml:"maxWSBufferSize"`
	MaxConnections      int             `yaml:"maxConnections"`      // 网关最大总连接数，0=不限制
	MaxConnectionsPerIP int             `yaml:"maxConnectionsPerIP"` // 单 IP 最大连接数，0=不限制
	MaxMessagesPerConn  int             `yaml:"maxMessagesPerConn"`  // 单连接最大消息数/秒，0=不限制
	CPUThreshold        float64         `yaml:"cpuThreshold"`
	DropOnOverload      bool            `yaml:"dropOnOverload"`
	CheckIntervalMs     int             `yaml:"checkIntervalMs"`
	WSHeartbeatTimeout  int             `yaml:"wsHeartbeatTimeout"`
	WSCheckInterval     int             `yaml:"wsCheckInterval"`
	ConnCheckInterval   string          `yaml:"connCheckInterval"`
	ConnIdleTimeout     string          `yaml:"connIdleTimeout"`
	VerifyInbound       bool            `yaml:"verifyInbound"`
	PreAuthCommands     []int32         `yaml:"preAuthCommands"`
	LoginAuth           LoginAuthConfig `yaml:"loginAuth"`
}

// LoginAuthConfig 定义网关侧的登录认证行为。
type LoginAuthConfig struct {
	// Mode 控制登录密钥校验方式：
	//   "none"：跳过校验，始终接受（默认，用于测试）
	//   "hmac"：按 HMAC-SHA256(userId, secret) 校验 login_key
	//   "delegate"：转发到逻辑服校验（会增加延迟）
	Mode string `yaml:"mode"`
	// Secret 是 HMAC 共享密钥（Mode 为 "hmac" 时必填）。
	Secret string `yaml:"secret"`
}

// Transport 网络传输配置
type Transport struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	Type     string `yaml:"type"`
}

// LoadConfig 从指定的 YAML 文件加载配置，若未找到则使用默认配置
// 采用合并语义：默认配置 + YAML 覆盖
func LoadConfig(configFiles ...string) (*Config, error) {
	cfg, err := uconfig.Load[Config](configFiles...)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

var conf atomic.Pointer[Config]

// Load 从指定的 YAML 文件加载配置并存入全局变量，返回配置指针。
func Load(configFiles ...string) (*Config, error) {
	cfg, err := uconfig.Load[Config](configFiles...)
	if err != nil {
		return nil, err
	}
	conf.Store(cfg)
	return cfg, nil
}

// Get 返回当前全局配置指针。必须在 Load 之后调用。
func Get() *Config {
	return conf.Load()
}
