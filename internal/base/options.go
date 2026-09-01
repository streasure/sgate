package base

import (
	"sync/atomic"

	"github.com/streasure/sgate/internal/config"
)

var _options atomic.Pointer[Options]

// Option is a functional option for Options.
type Option func(*Options)

// Options caches derived values from Config for fast access.
type Options struct {
	GRPCPort           int
	GRPCLogicAddr      string
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

// RefreshOptions forces a re-derive from the current Config.
func RefreshOptions() *Options {
	_config := GetConfig()
	if _config == nil {
		return nil
	}

	opts := deriveOptions(_config)
	_options.Store(opts)
	return opts
}

// GetOptions returns the cached Options, deriving from Config if needed.
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
		GRPCLogicAddr:      cfg.GRPC.LogicAddr,
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

// WithGRPCPort sets the gRPC port.
func WithGRPCPort(port int) Option {
	return func(o *Options) { o.GRPCPort = port }
}

// WithGRPCLogicAddr sets the logic server address.
func WithGRPCLogicAddr(addr string) Option {
	return func(o *Options) { o.GRPCLogicAddr = addr }
}

// WithDiscoveryEnabled sets whether service discovery is enabled.
func WithDiscoveryEnabled(enabled bool) Option {
	return func(o *Options) { o.DiscoveryEnabled = enabled }
}

// WithMonitoringEnabled sets whether monitoring is enabled.
func WithMonitoringEnabled(enabled bool) Option {
	return func(o *Options) { o.MonitoringEnabled = enabled }
}
