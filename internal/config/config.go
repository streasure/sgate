package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port         int                 `yaml:"port"`
	LogLevel     string              `yaml:"logLevel"`
	ServerID     string              `yaml:"serverId"`
	ServerType   string              `yaml:"serverType"`
	Zone         string              `yaml:"zone"`
	Discovery    DiscoveryConfig     `yaml:"discovery"`
	Transports   []Transport         `yaml:"transports"`
	GRPC         GRPCConfig          `yaml:"grpc"`
	LogicServers []LogicServerConfig `yaml:"logicServers"`
	Etcd         EtcdConfig          `yaml:"etcd"`
	Stream       StreamConfig        `yaml:"stream"`
	Protection   ProtectionConfig    `yaml:"protection"`
	Security     SecurityConfig      `yaml:"security"`
	WAF          WAFConfig           `yaml:"waf"`
	TLS          TLSConfig           `yaml:"tls"`
	Cluster      ClusterConfig       `yaml:"cluster"`
	// 企业级网关扩展能力
	Balancer      BalancerConfig      `yaml:"balancer"`
	JWTAuth       JWTAuthConfig       `yaml:"jwtAuth"`
	Canary        CanaryConfig        `yaml:"canary"`
	TrafficMirror TrafficMirrorConfig `yaml:"trafficMirror"`
	OTelTracer    OTelTracerConfig    `yaml:"otelTracer"`
	ConfigCenter  ConfigCenterConfig  `yaml:"configCenter"`
	Alert         AlertWebhookConfig  `yaml:"alert"`
	Degradation   DegradationConfig   `yaml:"degradation"`
	FilterChain   FilterChainConfig   `yaml:"filterChain"`
	Monitoring    MonitoringConfig    `yaml:"monitoring"`
}

// MonitoringConfig 监控接入配置（可插拔）
// 通过 enabled 开关控制是否启动 Prometheus metrics server
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
	Algorithm        string `yaml:"algorithm"`        // roundRobin | weighted | leastConn | consistent
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

// ConfigCenterConfig is retained for HTTP-based dynamic configuration.
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

type EtcdConfig struct {
	Enabled       bool     `yaml:"enabled"`
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
	Type   string `yaml:"type"` // wecom | dingtalk | generic
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
	NodeID         string `yaml:"nodeID"`
	LeaderElection bool   `yaml:"leaderElection"`
	LockTTL        string `yaml:"lockTTL"`
}

type DiscoveryConfig struct {
	Enabled           bool          `yaml:"enabled"`
	ServiceName       string        `yaml:"serviceName"`
	Zone              string        `yaml:"zone"`
	HeartbeatInterval time.Duration `yaml:"heartbeatInterval"`
	HeartbeatTTL      time.Duration `yaml:"heartbeatTTL"`
	DeregisterDelay   time.Duration `yaml:"deregisterDelay"`
	ScanInterval      time.Duration `yaml:"scanInterval"`
}

type GRPCConfig struct {
	Port           int    `yaml:"port"`
	LogicAddr      string `yaml:"logicAddr"`
	WindowSize     int    `yaml:"windowSize"`
	MaxMessageSize int    `yaml:"maxMessageSize"`
}

// LogicServerConfig is the authoritative static serverID-to-address mapping.
// Discovery may add dynamic instances, but a login gate request is accepted
// only for a known, connected server ID in the gateway's zone.
type LogicServerConfig struct {
	ServerID   string `yaml:"serverId"`
	ServerType string `yaml:"serverType"`
	Zone       string `yaml:"zone"`
	Address    string `yaml:"address"`
}

func (c *Config) LogicServer(serverID string) (LogicServerConfig, bool) {
	for _, server := range c.LogicServers {
		if server.ServerType == "" {
			server.ServerType = "Logic"
		}
		if server.ServerType == "Logic" && server.ServerID == serverID && (server.Zone == "" || server.Zone == c.Zone) {
			return server, true
		}
	}
	return LogicServerConfig{}, false
}

type StreamConfig struct {
	ShardCount       int `yaml:"shardCount"`
	SendChannelSize  int `yaml:"sendChannelSize"`
	ReceiveBatchSize int `yaml:"receiveBatchSize"`
}

type ProtectionConfig struct {
	MaxFrameSize       int     `yaml:"maxFrameSize"`
	MaxFrameBufSize    int     `yaml:"maxFrameBufSize"`
	MaxWSFrameSize     int     `yaml:"maxWSFrameSize"`
	MaxWSBufferSize    int     `yaml:"maxWSBufferSize"`
	CPUThreshold       float64 `yaml:"cpuThreshold"`
	DropOnOverload     bool    `yaml:"dropOnOverload"`
	CheckIntervalMs    int     `yaml:"checkIntervalMs"`
	WSHeartbeatTimeout int     `yaml:"wsHeartbeatTimeout"`
	WSCheckInterval    int     `yaml:"wsCheckInterval"`
	ConnCheckInterval  string  `yaml:"connCheckInterval"`
	ConnIdleTimeout    string  `yaml:"connIdleTimeout"`
	// VerifyInbound 是否对入方向消息执行完整性校验（checksum/timestamp/重放）。
	// 默认 true：对带 checksum 的入站消息做完整校验，未携带 checksum 的消息零开销直通。
	VerifyInbound bool `yaml:"verifyInbound"`
	// PreAuthCommands are the only client commands accepted before logic
	// authenticates the connection by returning StreamData.user_key.
	PreAuthCommands []int32 `yaml:"preAuthCommands"`
}

type Transport struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	Type     string `yaml:"type"`
}

func LoadConfig(configFiles ...string) (*Config, error) {
	var file *os.File
	candidates := configFiles
	if len(candidates) == 0 {
		candidates = []string{"config/config.yaml", "../config/config.yaml", "../../config/config.yaml"}
	}
	for _, name := range candidates {
		candidate, err := os.Open(filepath.Clean(name))
		if err == nil {
			file = candidate
			break
		}
	}
	if file == nil {
		return loadDefaultConfig(), nil
	}
	defer file.Close()

	// 合并语义：先加载默认（含硬编码常量），再用 yaml 覆盖
	// yaml 中未出现的字段保留默认；显式 false/0/"" 也算"出现"，会覆盖
	cfg := loadDefaultConfig()
	if err := yaml.NewDecoder(file).Decode(cfg); err != nil {
		return loadDefaultConfig(), nil
	}

	return cfg, nil
}

func loadDefaultConfig() *Config {
	port := getEnvInt("PORT", 8080)
	logLevel := getEnvString("LOG_LEVEL", "info")

	defaultTransports := []Transport{
		{Protocol: "tcp", Port: 8080},
		{Protocol: "udp", Port: 8081},
		{Protocol: "tcp", Port: 8082, Type: "websocket"},
	}

	return &Config{
		Port:       port,
		LogLevel:   logLevel,
		ServerID:   getEnvString("GATEWAY_SERVER_ID", "gateway-1"),
		ServerType: "Gateway",
		Discovery: DiscoveryConfig{
			Enabled:           true,
			ServiceName:       "logic",
			HeartbeatInterval: 3 * time.Second,
			HeartbeatTTL:      10 * time.Second,
			DeregisterDelay:   5 * time.Second,
			ScanInterval:      10 * time.Second,
		},
		Transports: defaultTransports,
		GRPC: GRPCConfig{
			Port:           50051,
			WindowSize:     DefaultGRPCWindowSize,
			MaxMessageSize: DefaultGRPCMaxMessageSize,
		},
		Stream: StreamConfig{
			ShardCount:       0,
			SendChannelSize:  DefaultStreamSendChannelSize,
			ReceiveBatchSize: DefaultStreamReceiveBatchSize,
		},
		Protection: ProtectionConfig{
			MaxFrameSize:       DefaultMaxFrameSize,
			MaxFrameBufSize:    DefaultMaxFrameSize,
			MaxWSFrameSize:     DefaultMaxWSFrameSize,
			MaxWSBufferSize:    DefaultMaxWSFrameSize,
			CPUThreshold:       90.0,
			DropOnOverload:     true,
			CheckIntervalMs:    DefaultOverloadCheckIntervalMs,
			WSHeartbeatTimeout: DefaultWSHeartbeatTimeoutSec,
			WSCheckInterval:    DefaultWSCheckIntervalSec,
			ConnCheckInterval:  DefaultConnCheckInterval,
			ConnIdleTimeout:    DefaultConnIdleTimeout,
			VerifyInbound:      false,
			PreAuthCommands:    []int32{1000001},
		},
		Security: SecurityConfig{
			Enabled: true,
			RateLimit: RateLimitConfig{
				Enabled:      true,
				MaxTokens:    DefaultRateLimitMaxTokens,
				TokenRefresh: DefaultRateLimitTokenRefresh,
			},
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:          true,
				FailureThreshold: DefaultCircuitBreakerFailureThreshold,
				SuccessThreshold: DefaultCircuitBreakerSuccessThreshold,
				Timeout:          DefaultCircuitBreakerTimeout,
			},
		},
		WAF: WAFConfig{
			Enabled:        true,
			MaxPayloadSize: DefaultWAFMaxPayloadSize,
			BlockAction:    DefaultWAFBlockAction,
		},
		TLS: TLSConfig{
			Enabled:    false,
			MinVersion: "TLS1.2",
		},
		Cluster: ClusterConfig{
			Enabled:        true,
			NodeID:         "",
			LeaderElection: true,
			LockTTL:        DefaultClusterLockTTL,
		},
		Balancer: BalancerConfig{
			Algorithm:        "roundRobin",
			FailureThreshold: DefaultBalancerFailureThreshold,
			RecoverInterval:  DefaultBalancerRecoverInterval,
		},
		JWTAuth: JWTAuthConfig{
			Enabled:     false,
			HeaderField: DefaultJWTHeaderField,
		},
		Canary: CanaryConfig{
			Enabled: false,
			Percent: DefaultCanaryPercent,
		},
		TrafficMirror: TrafficMirrorConfig{
			Enabled:   false,
			QueueSize: DefaultMirrorQueueSize,
			Workers:   DefaultMirrorWorkers,
		},
		OTelTracer: OTelTracerConfig{
			Enabled:     false,
			ServiceName: DefaultOTelServiceName,
			SampleRate:  DefaultOTelSampleRate,
			QueueSize:   DefaultOTelQueueSize,
			Workers:     DefaultOTelWorkers,
		},
		ConfigCenter: ConfigCenterConfig{
			Enabled:      false,
			PollInterval: DefaultConfigCenterPollInterval,
		},
		Alert: AlertWebhookConfig{
			Enabled:   false,
			RateLimit: DefaultAlertRateLimitPerMin,
		},
		Degradation: DegradationConfig{
			Enabled: false,
		},
		FilterChain: FilterChainConfig{
			Enabled: true,
		},
		Monitoring: MonitoringConfig{
			PprofAddr: DefaultPprofAddr,
			Prometheus: PrometheusConfig{
				Enabled: false, // 默认关闭，单体运行不依赖 Prometheus
				Addr:    DefaultPrometheusAddr,
				Path:    DefaultPrometheusPath,
				Prefix:  DefaultPrometheusPrefix,
			},
		},
	}
}

func getEnvString(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}
