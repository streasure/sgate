package gateway

import (
	"context"
	"testing"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
)

// newConfigUpdateTestGateway 构造仅 handleConfigUpdate 所需字段的 Gateway。
func newConfigUpdateTestGateway(cfg *config.Config) *Gateway {
	g := &Gateway{
		ctx:               context.Background(),
		connectionManager: connection.NewConnectionManager(0, 0),
	}
	g.cfg.Store(cfg)
	return g
}

func baseTestConfig() *config.Config {
	c := &config.Config{
		HttpPort:   8081,
		Belong:     "test",
		ServerID:   "gw-test",
		ServerType: "gate",
		Zone:       "z1",
		Transports: []config.Transport{{Port: 10000, Type: "tcp"}},
	}
	c.ApplyRuntimeDefaults()
	return c
}

// TestHandleConfigUpdate_RejectsSecurityEnableToggle 安全组件 enable 开关热切整单拒绝，保留旧配置。
func TestHandleConfigUpdate_RejectsSecurityEnableToggle(t *testing.T) {
	oldCfg := baseTestConfig()
	g := newConfigUpdateTestGateway(oldCfg)

	newCfg := baseTestConfig()
	newCfg.JWTAuth.Enabled = true // 启动时未启用，热切 true 属 fail-open 风险
	newCfg.JWTAuth.Secret = "s"

	g.handleConfigUpdate(newCfg)

	got := g.cfg.Load()
	if got != oldCfg {
		t.Fatal("update with jwtAuth.enable toggle should be rejected, config must stay unchanged")
	}
}

// TestHandleConfigUpdate_AllowsSameFlagsUpdate 开关不变时正常应用新配置。
func TestHandleConfigUpdate_AllowsSameFlagsUpdate(t *testing.T) {
	oldCfg := baseTestConfig()
	g := newConfigUpdateTestGateway(oldCfg)

	newCfg := baseTestConfig()
	newCfg.Protection.CPUThreshold = 80

	g.handleConfigUpdate(newCfg)

	got := g.cfg.Load()
	if got == oldCfg {
		t.Fatal("update without enable-flag change should be applied")
	}
	if got.Protection.CPUThreshold != 80 {
		t.Fatalf("CPUThreshold = %v, want 80", got.Protection.CPUThreshold)
	}
}
