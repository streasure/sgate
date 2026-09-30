package gateway

import (
	"testing"

	"github.com/streasure/sgate/internal/config"
)

func minimalConfig() *config.Config {
	return &config.Config{
		HttpPort:   8081,
		Belong:     "default",
		ServerID:   "gateway-test",
		ServerType: "Gateway",
		Zone:       "default",
		Transports: []config.Transport{{Protocol: "tcp", Port: 48080}},
		GRPC:       config.GRPCConfig{Port: 50051},
		Cluster:    config.ClusterConfig{NodeID: "node-test"},
	}
}

func TestNewGatewayPanicsWithoutConfig(t *testing.T) {
	// 清空全局配置，确保 Get() 返回 nil
	// config 包未导出清空接口，通过 Load 失败路径无法清；此处仅在确认未 Load 时跳过
	if config.Get() != nil {
		t.Skip("config already loaded in this process; cannot assert nil path")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewGateway should panic when config not loaded")
		}
	}()
	_ = NewGateway()
}

func TestNewGatewayAppliesDefaultsAndStoresCopy(t *testing.T) {
	cfg := minimalConfig()
	gw := newGateway(cfg)
	if gw == nil {
		t.Fatal("newGateway returned nil")
	}

	// 零值 protection 应被补齐
	p := gw.getProtection()
	if p.MaxFrameSize != config.DefaultMaxFrameSize {
		t.Errorf("MaxFrameSize = %d, want %d", p.MaxFrameSize, config.DefaultMaxFrameSize)
	}
	if p.WSHeartbeatTimeout != config.DefaultWSHeartbeatTimeoutSec {
		t.Errorf("WSHeartbeatTimeout = %d, want %d", p.WSHeartbeatTimeout, config.DefaultWSHeartbeatTimeoutSec)
	}

	// grpc/stream 快照应有默认值
	if got := gw.GetGRPCConfig(); got.Port != config.DefaultGRPCPort {
		t.Errorf("GRPC port = %d, want %d", got.Port, config.DefaultGRPCPort)
	}
	if got := gw.GetStreamConfig(); got.SendChannelSize != config.DefaultGatewayStreamSendChannelSize {
		t.Errorf("stream sendChannelSize = %d, want %d", got.SendChannelSize, config.DefaultGatewayStreamSendChannelSize)
	}

	// 应存指针副本：改入参 cfg 不影响内部存储
	cfg.ServerID = "mutated"
	stored := gw.cfg.Load()
	if stored == nil {
		t.Fatal("gw.cfg not stored as *config.Config")
	}
	if stored.ServerID == "mutated" {
		t.Error("internal config shares caller pointer (should be a copy)")
	}
	if stored.ServerID != "gateway-test" {
		t.Errorf("stored ServerID = %q, want gateway-test", stored.ServerID)
	}

	// 基础字段
	if gw.GetServerID() != "gateway-test" {
		t.Errorf("GetServerID = %q", gw.GetServerID())
	}
	if gw.pipeline == nil {
		t.Error("pipeline not initialized")
	}
	if gw.connectionManager == nil {
		t.Error("connectionManager not initialized")
	}
	if gw.stopChan == nil {
		t.Error("stopChan not initialized")
	}
	if cap(gw.configUpdateChan) != 1 {
		t.Errorf("configUpdateChan cap = %d, want 1", cap(gw.configUpdateChan))
	}

	// 避免泄漏：Close 走一次（closeOnce 保护）
	gw.Close()
}

func TestNewGatewayBuildsTLSConfig(t *testing.T) {
	cfg := minimalConfig()
	cfg.TLS.Enabled = false
	gw := newGateway(cfg)
	if gw.tlsConfig == nil {
		t.Fatal("tlsConfig should not be nil even when TLS disabled")
	}
	if len(gw.tlsConfig.Certificates) != 0 {
		t.Error("TLS disabled should not load certificates")
	}
	if gw.tlsConfig.MinVersion < 0x0303 { // TLS1.2
		t.Errorf("MinVersion too low: %d", gw.tlsConfig.MinVersion)
	}
	gw.Close()
}

func TestBuildTLSConfigDisabled(t *testing.T) {
	cfg := minimalConfig()
	c := buildTLSConfig(cfg)
	if c == nil {
		t.Fatal("buildTLSConfig returned nil")
	}
	if len(c.Certificates) != 0 {
		t.Error("expected no certs when TLS disabled")
	}
}

func TestMetricsLogEnabledDefault(t *testing.T) {
	cfg := minimalConfig()
	gw := newGateway(cfg)
	defer gw.Close()

	if !gw.metricsLogEnabled() {
		t.Error("default should enable metrics log (DisableMetricsLog=false)")
	}

	// 热更关闭
	cfg2 := minimalConfig()
	cfg2.Monitoring.DisableMetricsLog = true
	gw.cfg.Store(cfg2)
	if gw.metricsLogEnabled() {
		t.Error("DisableMetricsLog=true should disable metrics log")
	}
}

func TestTryEnqueueConfigKeepsLatest(t *testing.T) {
	cfg := minimalConfig()
	gw := newGateway(cfg)
	defer gw.Close()

	cfg1 := minimalConfig()
	cfg1.ServerID = "v1"
	cfg2 := minimalConfig()
	cfg2.ServerID = "v2"

	gw.tryEnqueueConfig(cfg1)
	gw.tryEnqueueConfig(cfg2) // 通道 cap=1，应丢弃 v1 保留 v2

	got := <-gw.configUpdateChan
	if got.ServerID != "v2" {
		t.Errorf("got ServerID=%q, want v2 (latest wins)", got.ServerID)
	}
	select {
	case extra := <-gw.configUpdateChan:
		t.Errorf("unexpected extra config: %v", extra)
	default:
	}
}
