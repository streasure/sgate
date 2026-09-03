package gateway

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/types"
	"github.com/streasure/util/nacos"
)

// GatewayDeps holds all external dependencies for Gateway construction.
// Each dependency is owned by its respective Component and passed here
// for Gateway to reference during request processing.
type GatewayDeps struct {
	Config       config.Config
	FilterChain  *types.FilterChain
	LogSanitizer *obs.LogSanitizer

	// Security
	WhitelistBlacklist *security.WhitelistBlacklist
	WAF                *security.WAF
	RateLimiter        *security.RateLimiter
	JWTAuth            *security.JWTAuthFilter
	CircuitBreakerMgr  *security.CircuitBreakerManager

	// Observability
	Tracer         *obs.Tracer
	OTelTracer     *obs.OTelTracer
	LatencyTracker *obs.LatencyTracker

	// Traffic
	CanaryFilter  *traffic.CanaryFilter
	TrafficMirror *traffic.TrafficMirror
	Degradation   *traffic.DegradationManager

	// Cluster
	Discovery    *nacos.Discovery
	Balancer     *cluster.Balancer
	ConfigCenter cluster.ConfigCenter
	ClusterNode  *cluster.Cluster
	AlertWebhook *cluster.AlertWebhook
}

// NewGatewayWithDeps constructs a Gateway from externally managed components.
// This is the preferred constructor when using the Component lifecycle.
func NewGatewayWithDeps(deps GatewayDeps) *Gateway {
	protection := deps.Config.Protection
	if protection.MaxFrameSize <= 0 {
		protection.MaxFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxFrameBufSize <= 0 {
		protection.MaxFrameBufSize = 4 * 1024 * 1024
	}
	if protection.MaxWSFrameSize <= 0 {
		protection.MaxWSFrameSize = 4 * 1024 * 1024
	}
	if protection.MaxWSBufferSize <= 0 {
		protection.MaxWSBufferSize = 4 * 1024 * 1024
	}
	if protection.WSHeartbeatTimeout <= 0 {
		protection.WSHeartbeatTimeout = 60
	}
	if protection.WSCheckInterval <= 0 {
		protection.WSCheckInterval = 30
	}

	grpcCfg := deps.Config.GRPC
	if grpcCfg.Port <= 0 {
		grpcCfg.Port = 50051
	}
	if grpcCfg.WindowSize <= 0 {
		grpcCfg.WindowSize = 524288
	}
	if grpcCfg.MaxMessageSize <= 0 {
		grpcCfg.MaxMessageSize = 4 * 1024 * 1024
	}

	streamCfg := deps.Config.Stream
	if streamCfg.SendChannelSize <= 0 {
		streamCfg.SendChannelSize = 65536
	}
	if streamCfg.ReceiveBatchSize <= 0 {
		streamCfg.ReceiveBatchSize = 64
	}

	gw := &Gateway{
		connectionManager: NewConnectionManager(),
		stopChan:          make(chan struct{}),
		protection:        protection,
		grpcCfg:           grpcCfg,
		streamCfg:         streamCfg,
		serverID:          deps.Config.ServerID,
		zone:              deps.Config.Zone,

		// From components
		filterChain:        deps.FilterChain,
		logSanitizer:       deps.LogSanitizer,
		whitelistBlacklist: deps.WhitelistBlacklist,
		waf:                deps.WAF,
		rateLimiter:        deps.RateLimiter,
		jwtAuth:            deps.JWTAuth,
		circuitBreakerMgr:  deps.CircuitBreakerMgr,
		tracer:             deps.Tracer,
		otelTracer:         deps.OTelTracer,
		latencyTracker:     deps.LatencyTracker,
		canaryFilter:       deps.CanaryFilter,
		trafficMirror:      deps.TrafficMirror,
		degradation:        deps.Degradation,
		serviceDiscovery:   deps.Discovery,
		balancer:           deps.Balancer,
		configCenter:       deps.ConfigCenter,
		cluster:            deps.ClusterNode,
		alertWebhook:       deps.AlertWebhook,

		configUpdateChan:  make(chan *config.Config),
		overloadProtector: NewOverloadProtector(protection),
		logicClient:       NewLogicClient(GatewayInterface(nil)),
		msgRate:           newMessageRateTracker(60 * time.Second),
		clusterID:         "sgate-cluster",
		gatewayID:         gatewayInstanceID(deps.Config),
		isLeader:          false,
	}

	gw.cfg.Store(&deps.Config)
	gw.ctx = context.Background()

	return gw
}

func gatewayInstanceID(cfg config.Config) string {
	if cfg.Cluster.NodeID != "" {
		return cfg.Cluster.NodeID
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "sgate"
	}
	return fmt.Sprintf("%s-%d-%d", hostname, os.Getpid(), cfg.GRPC.Port)
}
