package gateway

import (
	"context"
	"testing"

	"github.com/streasure/sgate/internal/component"
	"github.com/streasure/sgate/internal/config"
)

func minimalConfig() *config.Config {
	return &config.Config{
		HttpPort:   8081,
		Belong:     "default",
		ServerID:   "gateway-test",
		ServerType: "Gateway",
		Zone:       "default",
		Transports: []config.Transport{{Port: 48080}},
		GRPC:       config.GRPCConfig{Port: 50051},
		Cluster:    config.ClusterConfig{NodeID: "node-test"},
	}
}

// newTestGateway 以空资源容器构造网关壳（gateway 包测试均不调 Init/Start，
// 容器仅满足构造期注入；组件写入路径由 component 包测试覆盖）。
// 业务半区断言见 dataplane 包 engine_test.go。
func newTestGateway(cfg *config.Config) *Gateway {
	return newGateway(cfg, "", component.NewResources())
}

func TestNewGatewayPanicsOnNilConfig(t *testing.T) {
	// 构造器显式收参后，nil 配置是编程错误——必须 fail-fast panic，
	// 而不是在 Start 阶段以 nil deref 形式更难排查地崩。
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewGateway(nil) should panic")
		}
	}()
	_ = NewGateway(nil, "", component.NewResources())
}

func TestNewGatewayBuildsTLSConfig(t *testing.T) {
	cfg := minimalConfig()
	cfg.TLS.Enabled = false
	gw := newTestGateway(cfg)
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
	c := buildTLSConfig(context.Background(), cfg)
	if c == nil {
		t.Fatal("buildTLSConfig returned nil")
	}
	if len(c.Certificates) != 0 {
		t.Error("expected no certs when TLS disabled")
	}
}
