package base

import (
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
)

var _options atomic.Pointer[Options]

// Option 是用于修改派生选项的函数式选项。
type Option func(*Options)

// Options 保存从完整配置派生出的高频访问参数，减少业务路径上的配置查找和锁竞争。
type Options struct {
	GRPCPort           int
	GRPCWindowSize     int
	GRPCMaxMessageSize int

	StreamShardCount       int
	StreamSendChannelSize  int
	StreamReceiveBatchSize int

	ProtectionMaxFrameSize    int
	ProtectionMaxFrameBufSize int
	ProtectionMaxWSFrameSize  int
	ProtectionMaxWSBufferSize int
	ProtectionCPUThreshold    float64
	ProtectionDropOnOverload  bool

	VerifyInbound bool

	SecurityEnabled       bool
	RateLimitEnabled      bool
	RateLimitMaxTokens    int
	RateLimitTokenRefresh string

	DiscoveryEnabled     bool
	DiscoveryServiceName string

	MonitoringEnabled bool
	MonitoringAddr    string
	MonitoringPath    string
}

// RefreshOptions 根据当前全局配置重新生成并缓存派生选项。
func RefreshOptions() *Options {
	_config := GetConfig()
	if _config == nil {
		return nil
	}

	opts := deriveOptions(_config)
	_options.Store(opts)
	return opts
}

// GetOptions 返回缓存的派生选项；缓存不存在时先根据全局配置生成。
func GetOptions() *Options {
	opts := _options.Load()
	if opts != nil {
		return opts
	}
	return RefreshOptions()
}

func deriveOptions(cfg *config.Config) *Options {
	return &Options{
		GRPCPort:           cfg.GRPC.Port,
		GRPCWindowSize:     cfg.GRPC.WindowSize,
		GRPCMaxMessageSize: cfg.GRPC.MaxMessageSize,

		StreamShardCount:       cfg.Stream.ShardCount,
		StreamSendChannelSize:  cfg.Stream.SendChannelSize,
		StreamReceiveBatchSize: cfg.Stream.ReceiveBatchSize,

		ProtectionMaxFrameSize:    cfg.Protection.MaxFrameSize,
		ProtectionMaxFrameBufSize: cfg.Protection.MaxFrameBufSize,
		ProtectionMaxWSFrameSize:  cfg.Protection.MaxWSFrameSize,
		ProtectionMaxWSBufferSize: cfg.Protection.MaxWSBufferSize,
		ProtectionCPUThreshold:    cfg.Protection.CPUThreshold,
		ProtectionDropOnOverload:  cfg.Protection.DropOnOverload,

		VerifyInbound: cfg.Protection.VerifyInbound,

		SecurityEnabled:       cfg.Security.Enabled,
		RateLimitEnabled:      cfg.Security.RateLimit.Enabled,
		RateLimitMaxTokens:    cfg.Security.RateLimit.MaxTokens,
		RateLimitTokenRefresh: cfg.Security.RateLimit.TokenRefresh,

		DiscoveryEnabled:     cfg.Discovery.Enabled,
		DiscoveryServiceName: cfg.Discovery.ServiceName,

		MonitoringEnabled: cfg.Monitoring.Prometheus.Enabled,
		MonitoringAddr:    cfg.Monitoring.Prometheus.Addr,
		MonitoringPath:    cfg.Monitoring.Prometheus.Path,
	}
}

// WithGRPCPort 设置 gRPC 监听端口。
func WithGRPCPort(port int) Option {
	return func(o *Options) { o.GRPCPort = port }
}

// WithDiscoveryEnabled 设置是否启用服务发现。
func WithDiscoveryEnabled(enabled bool) Option {
	return func(o *Options) { o.DiscoveryEnabled = enabled }
}

// WithMonitoringEnabled 设置是否启用监控。
func WithMonitoringEnabled(enabled bool) Option {
	return func(o *Options) { o.MonitoringEnabled = enabled }
}
