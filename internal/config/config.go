package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
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
	JWTAuth         JWTAuthConfig
	Canary          CanaryConfig
	TrafficMirror   TrafficMirrorConfig
	OTelTracer      OTelTracerConfig
	ConfigCenter    ConfigCenterConfig
	Alert           AlertWebhookConfig
	Degradation     DegradationConfig
	FilterChain     FilterChainConfig
	WasmRuntime     WasmRuntimeConfig
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
	WorkerQueueSize int  `yaml:"workerQueueSize"` // 任务队列总大小（全池共享，启动时按分片数均分，默认 65536）
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

// EtcdConfig etcd 服务注册与发现配置（etcd 默认接入，无启用开关）
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

// WasmRuntimeConfig WASM 运行时配置（启动时加载 .wasm 模块，供 wasm-filter SPI 使用）
type WasmRuntimeConfig struct {
	Enabled bool               `yaml:"enabled"`
	Modules []WasmModuleConfig `yaml:"modules"`
}

// WasmModuleConfig 单个 WASM 模块
type WasmModuleConfig struct {
	Name string `yaml:"name"` // 模块名（filterChain 中 wasm-filter 的 module 引用此名）
	Path string `yaml:"path"` // .wasm 文件路径
}

// FilterItemConfig 单个过滤器配置
type FilterItemConfig struct {
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config"`
}

// SecurityConfig 安全防护配置（白名单/黑名单/限流/熔断/Kick RPC 鉴权/反代信任）
type SecurityConfig struct {
	Enabled   bool     `yaml:"enabled"`
	Whitelist []string `yaml:"whitelist"`
	Blacklist []string `yaml:"blacklist"`
	// KickInternalToken KickService（loginserver→sgate 单向踢人 RPC）鉴权 token；
	// 空=拒绝全部 Kick RPC。与 loginserver sgate.internalToken 保持一致。
	KickInternalToken string               `yaml:"kickInternalToken"`
	RateLimit         RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker    CircuitBreakerConfig `yaml:"circuitBreaker"`
	// TrustedProxies 反向代理信任列表（IP 或 CIDR）。仅对 WebSocket 生效：
	// 握手时若直连对端命中此列表，则从 X-Real-IP / X-Forwarded-For 解析真实
	// 客户端 IP 并更新连接（RemoteHost + per-IP 计数重键）。TCP 无 HTTP 头
	// 无法传递真实 IP，反代场景请用 WS 或在代理层终止后直连。
	// 空（默认）= 不信任任何代理头，防 XFF 伪造。
	TrustedProxies []string `yaml:"trustedProxies"`
	// ProxyProtocol 启用 PROXY protocol v1 解析（HAProxy/Nginx `proxy_protocol on`）。
	// TCP 连接首包若以 "PROXY " 开头则解析并取真实客户端 IP（更新 RemoteHost
	// 与 per-IP 计数）。仅对 TCP 生效；与 TrustedProxies（WS XFF）互补。
	// 未启用时首包 "PROXY " 会被误判为非法帧长而断连。
	ProxyProtocol bool `yaml:"proxyProtocol"`
}

// LoginValidationConfig LoginGate loginKey 校验开关与周期安全网。
// enabled=false：永远放行（压测/本地免登），周期安全网不跑。
// enabled=true：必须经 loginserver ValidateLoginToken；空 loginKey 也不能绕过。
// batchInterval>0：周期调 BatchValidateLoginTokens 重校验全部在线会话，
// 失效/被封禁的本地踢下线（收口 mid-login 竞态与会话缓存 TTL 过期踢不中）。
type LoginValidationConfig struct {
	Enabled       bool   `yaml:"enabled"`
	BatchInterval string `yaml:"batchInterval"` // 周期安全网间隔；空/0=关闭
	BatchSize     int    `yaml:"batchSize"`     // 单次批量校验上限；0=默认 200（loginserver 硬上限 500）
}

// RateLimitConfig 限流配置
type RateLimitConfig struct {
	Enabled      bool   `yaml:"enabled"`
	MaxTokens    int    `yaml:"maxTokens"`
	TokenRefresh string `yaml:"tokenRefresh"`
	// RouteMaxTokens route 维度每路由速率；0 = maxTokens*4。全局消息量 ≈ 该值×路由数，
	// 高吞吐生产需调大或关闭限流，否则 route 维度先成为天花板。
	RouteMaxTokens int `yaml:"routeMaxTokens"`
	// GlobalMaxTokens 全局硬顶（单令牌桶，每消息 1 次 CAS）；0 = maxTokens*10。
	// 兜底 overload 未运行（benchMode/未配 dropOnOverload）时的全局总量控制。
	GlobalMaxTokens int `yaml:"globalMaxTokens"`
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
	Mode   string `yaml:"mode"` // standalone: 单机模式（默认），cluster: 集群协同模式
	NodeID string `yaml:"nodeID"`
}

// DiscoveryConfig 服务发现配置
type DiscoveryConfig struct {
	Enabled      bool   `yaml:"enabled"`
	ServiceName  string `yaml:"serviceName"`
	Zone         string `yaml:"zone"`
	RegisterSelf bool   `yaml:"registerSelf"` // standalone模式下是否注册到etcd提供发现服务
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
	ShardCount      int               `yaml:"shardCount"`
	ConnGroupCount  int               `yaml:"connGroupCount"` // gateway→logic 并行 TCP 连接组数（默认 4）
	SendChannelSize int               `yaml:"sendChannelSize"`
	BatchPush       bool              `yaml:"batchPush"`
	QueuePolicy     StreamQueueConfig `yaml:"queuePolicy"`
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
	// BenchMode 压测模式：关闭与压测无关的后台周期任务（过载保护检查、
	// 限流桶清理、WS 心跳清理、连接空闲检查），避免其与热路径争抢调度。
	// 仅用于压测/性能测量，生产保持 false。
	BenchMode bool `yaml:"benchMode"`
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
	if c.JWTAuth.Enabled && c.JWTAuth.Secret == "" {
		// 空 secret时 HS256 用空键验签，任何知道此事实的人都能伪造任意用户令牌
		return fmt.Errorf("jwtAuth.secret is required when jwtAuth.enabled=true")
	}
	for _, p := range c.Security.TrustedProxies {
		if err := validateIPOrCIDR(p); err != nil {
			return fmt.Errorf("security.trustedProxies %q: %w", p, err)
		}
	}
	return nil
}

// validateIPOrCIDR 校验字符串为合法 IP 或 CIDR 前缀
func validateIPOrCIDR(s string) error {
	if strings.Contains(s, "/") {
		if _, err := netip.ParsePrefix(s); err != nil {
			return err
		}
		return nil
	}
	if _, err := netip.ParseAddr(s); err != nil {
		return err
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
	if len(p.PreAuthCommands) == 0 {
		p.PreAuthCommands = []int32{DefaultPreAuthCommands}
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
	if c.LoginValidation.BatchInterval == "" {
		c.LoginValidation.BatchInterval = DefaultLoginValidationBatchInterval
	}
	if c.LoginValidation.BatchSize <= 0 {
		c.LoginValidation.BatchSize = DefaultLoginValidationBatchSize
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
		c.Pipeline.WorkerQueueSize = DefaultPipelineWorkerQueueSize
	}
	if c.Protection.CheckIntervalMs <= 0 {
		c.Protection.CheckIntervalMs = DefaultOverloadCheckIntervalMs
	}
	if c.Protection.CPUThreshold <= 0 {
		c.Protection.CPUThreshold = DefaultOverloadCPUThreshold
	}

	// 发现 / etcd / 集群 / stream / perf 等「基本不改」项的零值默认
	if !c.Discovery.Enabled && c.Discovery.ServiceName == "" && c.Discovery.Zone == "" {
		// 未配置 discovery 段时补默认（enabled 仍为 false 需显式开）
		c.Discovery.ServiceName = DefaultDiscoveryServiceName
		if c.Zone != "" {
			c.Discovery.Zone = c.Zone
		} else {
			c.Discovery.Zone = "default"
		}
	}
	if c.Discovery.ServiceName == "" {
		c.Discovery.ServiceName = DefaultDiscoveryServiceName
	}
	if c.Discovery.Zone == "" {
		c.Discovery.Zone = c.Zone
		if c.Discovery.Zone == "" {
			c.Discovery.Zone = "default"
		}
	}
	if c.Etcd.ServicePrefix == "" {
		c.Etcd.ServicePrefix = DefaultEtcdServicePrefix
	}
	if c.Etcd.LeaseTTL == "" {
		c.Etcd.LeaseTTL = DefaultEtcdLeaseTTL
	}
	if c.Cluster.Mode == "" {
		c.Cluster.Mode = DefaultClusterMode
	}
	if c.Stream.ShardCount <= 0 {
		c.Stream.ShardCount = DefaultStreamShardCount
	}
	if c.Stream.ConnGroupCount <= 0 {
		c.Stream.ConnGroupCount = DefaultStreamConnGroupCount
	}
	if c.Perf.GcPercent <= 0 {
		c.Perf.GcPercent = DefaultPerfGcPercent
	}
	if c.Perf.MemoryLimitPercent <= 0 {
		c.Perf.MemoryLimitPercent = DefaultPerfMemoryLimitPercent
	}
	if c.Security.RateLimit.RouteMaxTokens < 0 {
		c.Security.RateLimit.RouteMaxTokens = 0
	}
	if c.Security.RateLimit.GlobalMaxTokens < 0 {
		c.Security.RateLimit.GlobalMaxTokens = 0
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
