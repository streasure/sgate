package config

import (
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/streasure/util/uconfig"
)

// Config 网关服务的完整配置结构体，包含所有模块配置
type Config struct {
	HttpPort        int             `validate:"required"`
	Belong          string          `validate:"required"` // 所属应用/团队标识，与 serverType+zone+serverId 唯一确定一个服务
	ServerID        string          `validate:"required"`
	ServerType      string          `validate:"required"`
	Zone            string          `validate:"required"`
	Discovery       DiscoveryConfig `validate:"required"`
	Transports      []Transport     `validate:"required"`
	GRPC            GRPCConfig      `validate:"required"`
	Etcd            EtcdConfig      `validate:"required"`
	Stream          StreamConfig
	Protection      ProtectionConfig
	Security        SecurityConfig
	LoginValidation LoginValidationConfig
	WAF             WAFConfig
	TLS             TLSConfig
	Cluster         ClusterConfig
	Balancer        BalancerConfig
	JWTAuth         JWTAuthConfig
	Canary          CanaryConfig
	TrafficMirror   TrafficMirrorConfig
	OTelTracer      OTelTracerConfig
	ConfigCenter    ConfigCenterConfig
	Alert           AlertWebhookConfig
	Degradation     DegradationConfig
	FilterChain     FilterChainConfig
	Monitoring      MonitoringConfig
	Perf            PerfConfig
	Pipeline        PipelineConfig
	Admin           AdminConfig
}

// AdminConfig 管理端 HTTP 接口鉴权（stats server 上的 /admin/*）。
// token 为空 = 管理接口关闭（fail-closed）。
type AdminConfig struct {
	Token string `yaml:"token"`
}

// PipelineConfig Pipeline 异步化配置
type PipelineConfig struct {
	AsyncEnabled    bool `yaml:"asyncEnabled"`    // 启用异步 pipeline（worker pool 模式）
	WorkerShards    int  `yaml:"workerShards"`    // worker 分片数（默认 CPU×4）
	WorkerQueueSize int  `yaml:"workerQueueSize"` // 每个分片的任务队列大小
}

// PerfConfig 运行时性能调优参数（GC、内存限制），参数以机器资源百分比表示，跨机器通用。
type PerfConfig struct {
	GcPercent          int `yaml:"gcPercent"`          // GOGC 等价：堆增长触发 GC 的百分比（默认 100）
	MemoryLimitPercent int `yaml:"memoryLimitPercent"` // GOMEMLIMIT 等价：软内存上限占总内存百分比（推荐 80-90）
}

// PortAddress 返回格式化的监听地址（如 ":8080"），端口无效时返回空字符串
func (c *Config) PortAddress() string {
	if c.HttpPort <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(c.HttpPort)
}

// MonitoringConfig 监控接入配置（可插拔）
// 关闭时 sgate 单体也能正常运行
type MonitoringConfig struct {
	PprofAddr string `yaml:"pprofAddr"`
	// DisableMetricsLog 关闭每秒打印 gateway metrics 日志（OnTick输出）。
	// 配置为 true 关闭该日志（水位压测/演练时 false 打开）；默认 false=保持每秒
	DisableMetricsLog bool `yaml:"disableMetricsLog"`
	// DisableTracer 关闭内部 Tracer（每消息 span/属性采样）。
	// 关闭后消息管道走超级快速路径（跳过安全链/过滤器链的追踪分支），热路径分配大幅下降。
	// 仅需日志级排障时开启；压测与生产吞吐场景应为 true。默认 false=开启（兼容旧行为）。
	DisableTracer bool `yaml:"disableTracer"`
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
	// Enabled 是否启用 etcd（nil=true；yaml `etcd.enabled`）。
	// 关闭时跳过 etcd 启动（无逻辑服务发现/网关注册，standalone 纯静态地址模式）。
	Enabled       *bool    `yaml:"enabled"`
	Endpoints     []string `yaml:"endpoints"`
	Endpoint      string   `yaml:"endpoint"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	DialTimeout   string   `yaml:"dialTimeout"`
	ServicePrefix string   `yaml:"servicePrefix"`
	LeaseTTL      string   `yaml:"leaseTTL"`
}

// IsEnabled 返回 etcd 是否启用（缺省 true）。
func (e EtcdConfig) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

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
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config"`
}

// SecurityConfig 安全防护配置（白名单/黑名单/限流/熔断）
type SecurityConfig struct {
	Enabled        bool                 `yaml:"enabled"`
	Whitelist      []string             `yaml:"whitelist"`
	Blacklist      []string             `yaml:"blacklist"`
	RateLimit      RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuitBreaker"`
}

// LoginValidationConfig LoginGate loginKey 校验开关。
// enabled=false：永远放行（压测/本地免登）。
// enabled=true：必须经 loginserver ValidateLoginToken；空 loginKey 也不能绕过。
type LoginValidationConfig struct {
	Enabled bool `yaml:"enabled"`
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
	ShardCount       int  `yaml:"shardCount"`
	ConnGroupCount   int  `yaml:"connGroupCount"` // gateway��logic ���� TCP ���������Ĭ�� 4��
	SendChannelSize  int  `yaml:"sendChannelSize"`
	ReceiveBatchSize int  `yaml:"receiveBatchSize"`
	BatchPush        bool `yaml:"batchPush"`
	// BatchUpstream 将 gateway<->logic 数据流的多条 StreamData 合并为单个
	// StreamBatch gRPC 帧（双向均生效）。两端必须同时开启；对端未升级时严禁开启。
	BatchUpstream bool              `yaml:"batchUpstream"`
	QueuePolicy   StreamQueueConfig `yaml:"queuePolicy"`
}

type ProtectionConfig struct {
	MaxFrameSize        int     `yaml:"maxFrameSize"`
	MaxFrameBufSize     int     `yaml:"maxFrameBufSize"`
	MaxWSFrameSize      int     `yaml:"maxWSFrameSize"`
	MaxWSBufferSize     int     `yaml:"maxWSBufferSize"`
	MaxConnections      int     `yaml:"maxConnections"`      // 网关最大总连接数，0=不限制
	MaxConnectionsPerIP int     `yaml:"maxConnectionsPerIP"` // 单 IP 最大连接数，0=不限制
	MaxMessagesPerConn  int     `yaml:"maxMessagesPerConn"`  // 单连接最大消息数/秒，0=不限制
	CPUThreshold        float64 `yaml:"cpuThreshold"`
	DropOnOverload      bool    `yaml:"dropOnOverload"`
	CheckIntervalMs     int     `yaml:"checkIntervalMs"`
	WSHeartbeatTimeout  int     `yaml:"wsHeartbeatTimeout"`
	WSCheckInterval     int     `yaml:"wsCheckInterval"`
	ConnCheckInterval   string  `yaml:"connCheckInterval"`
	ConnIdleTimeout     string  `yaml:"connIdleTimeout"`
	VerifyInbound       bool    `yaml:"verifyInbound"`
	PreAuthCommands     []int32 `yaml:"preAuthCommands"`
}

// Transport 网络传输配置
type Transport struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	Type     string `yaml:"type"`
}

var conf atomic.Pointer[Config]
var confPath atomic.Pointer[string] // 最近一次 Load 的配置文件路径

// Validate 校验必填配置项。Load/LoadConfig 解析成功后调用。
func (c *Config) Validate() error {
	if c.HttpPort <= 0 {
		return fmt.Errorf("httpPort is required and must be > 0")
	}
	if c.Belong == "" {
		return fmt.Errorf("belong is required")
	}
	if c.ServerID == "" {
		return fmt.Errorf("serverId is required")
	}
	if c.ServerType == "" {
		return fmt.Errorf("serverType is required")
	}
	if c.Zone == "" {
		return fmt.Errorf("zone is required")
	}
	if len(c.Transports) == 0 {
		return fmt.Errorf("transports is required")
	}
	return nil
}

// ApplyRuntimeDefaults 为运行时零值字段补齐安全默认值。
// Load/LoadConfig 与配置热更新必须在 Store 前调用；幂等，可重复调用。
// 默认值与压测/生产网关当前行为保持一致（勿随意改数值，避免吞吐/内存回归）。
func (c *Config) ApplyRuntimeDefaults() {
	p := &c.Protection
	if p.MaxFrameSize <= 0 {
		p.MaxFrameSize = DefaultMaxFrameSize
	}
	if p.MaxFrameBufSize <= 0 {
		// 与历史网关运行时默认一致（4MB）；百万连接场景请在 yaml 显式设 64KB
		p.MaxFrameBufSize = DefaultMaxFrameSize
	}
	if p.MaxWSFrameSize <= 0 {
		p.MaxWSFrameSize = DefaultMaxWSFrameSize
	}
	if p.MaxWSBufferSize <= 0 {
		p.MaxWSBufferSize = DefaultMaxWSFrameSize
	}
	if p.WSHeartbeatTimeout <= 0 {
		p.WSHeartbeatTimeout = DefaultWSHeartbeatTimeoutSec
	}
	if p.WSCheckInterval <= 0 {
		p.WSCheckInterval = DefaultWSCheckIntervalSec
	}
	if p.ConnCheckInterval == "" {
		p.ConnCheckInterval = DefaultConnCheckInterval
	}
	if p.ConnIdleTimeout == "" {
		p.ConnIdleTimeout = DefaultConnIdleTimeout
	}

	g := &c.GRPC
	if g.Port <= 0 {
		g.Port = DefaultGRPCPort
	}
	// WindowSize/MaxMessageSize 与历史网关零值默认保持一致（勿改成 defaults.go 的 16MB/8MB，避免压测回归）
	if g.WindowSize <= 0 {
		g.WindowSize = DefaultGatewayGRPCWindowSize
	}
	if g.MaxMessageSize <= 0 {
		g.MaxMessageSize = DefaultGatewayGRPCMaxMessageSize
	}

	s := &c.Stream
	// 与历史网关零值默认一致
	if s.SendChannelSize <= 0 {
		s.SendChannelSize = DefaultGatewayStreamSendChannelSize
	}
	if s.ReceiveBatchSize <= 0 {
		s.ReceiveBatchSize = DefaultStreamReceiveBatchSize
	}
	if s.QueuePolicy.Policy == "" {
		s.QueuePolicy.Policy = DefaultStreamQueuePolicy
	}
	if s.QueuePolicy.MaxSize <= 0 {
		s.QueuePolicy.MaxSize = DefaultStreamQueueMaxSize
	}
	if s.QueuePolicy.BlockTimeout == "" {
		s.QueuePolicy.BlockTimeout = DefaultStreamBlockTimeout
	}
	if s.QueuePolicy.BackpressureThreshold <= 0 {
		s.QueuePolicy.BackpressureThreshold = DefaultBackpressureThreshold
	}
	if s.QueuePolicy.SendTimeout == "" {
		s.QueuePolicy.SendTimeout = DefaultSendTimeout
	}

	if c.Security.RateLimit.MaxTokens <= 0 {
		c.Security.RateLimit.MaxTokens = DefaultRateLimitMaxTokens
	}
	if c.Security.RateLimit.TokenRefresh == "" {
		c.Security.RateLimit.TokenRefresh = DefaultRateLimitTokenRefresh
	}
	if c.Security.CircuitBreaker.FailureThreshold <= 0 {
		c.Security.CircuitBreaker.FailureThreshold = DefaultCircuitBreakerFailureThreshold
	}
	if c.Security.CircuitBreaker.SuccessThreshold <= 0 {
		c.Security.CircuitBreaker.SuccessThreshold = DefaultCircuitBreakerSuccessThreshold
	}
	if c.Security.CircuitBreaker.Timeout == "" {
		c.Security.CircuitBreaker.Timeout = DefaultCircuitBreakerTimeout
	}
	if c.JWTAuth.HeaderField == "" {
		c.JWTAuth.HeaderField = DefaultJWTHeaderField
	}
	if c.WAF.MaxPayloadSize <= 0 {
		c.WAF.MaxPayloadSize = DefaultWAFMaxPayloadSize
	}
	if c.WAF.BlockAction == "" {
		c.WAF.BlockAction = DefaultWAFBlockAction
	}
	if c.Pipeline.WorkerShards <= 0 {
		c.Pipeline.WorkerShards = 0 // 0 = runtime.NumCPU()*4，由 pool 计算
	}
	if c.Pipeline.WorkerQueueSize <= 0 {
		c.Pipeline.WorkerQueueSize = 4096
	}
	if c.Protection.CheckIntervalMs <= 0 {
		c.Protection.CheckIntervalMs = DefaultOverloadCheckIntervalMs
	}
	if c.Protection.CPUThreshold <= 0 {
		c.Protection.CPUThreshold = DefaultOverloadCPUThreshold
	}
	// Monitoring.PprofAddr 空串 = 关闭 pprof，不填默认值
	if c.OTelTracer.ServiceName == "" {
		c.OTelTracer.ServiceName = DefaultOTelServiceName
	}
	if c.OTelTracer.SampleRate <= 0 {
		c.OTelTracer.SampleRate = DefaultOTelSampleRate
	}
	if c.OTelTracer.QueueSize <= 0 {
		c.OTelTracer.QueueSize = DefaultOTelQueueSize
	}
	if c.OTelTracer.Workers <= 0 {
		c.OTelTracer.Workers = DefaultOTelWorkers
	}
	if c.Alert.RateLimit <= 0 {
		c.Alert.RateLimit = DefaultAlertRateLimitPerMin
	}
	if c.TrafficMirror.QueueSize <= 0 {
		c.TrafficMirror.QueueSize = DefaultMirrorQueueSize
	}
	if c.TrafficMirror.Workers <= 0 {
		c.TrafficMirror.Workers = DefaultMirrorWorkers
	}
}

// LoadConfig 从指定的 YAML 文件加载配置，若未找到则使用默认配置
// 采用合并语义：默认配置 + YAML 覆盖
func LoadConfig(configFiles ...string) (*Config, error) {
	cfg, err := uconfig.Load[Config](configFiles...)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ApplyRuntimeDefaults()
	return cfg, nil
}

// Load 从指定的 YAML 文件加载配置并存入全局变量，返回配置指针。
func Load(configFiles ...string) (*Config, error) {
	cfg, err := uconfig.Load[Config](configFiles...)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ApplyRuntimeDefaults()
	conf.Store(cfg)
	if len(configFiles) > 0 && configFiles[0] != "" {
		p := configFiles[0]
		confPath.Store(&p)
	}
	return cfg, nil
}

// Get 返回当前全局配置指针。必须在 Load 之后调用。
func Get() *Config {
	return conf.Load()
}

// Path 返回最近一次 Load 使用的配置文件路径（未 Load 时返回空串）。
func Path() string {
	if v := confPath.Load(); v != nil {
		return *v
	}
	return ""
}
