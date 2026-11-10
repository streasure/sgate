package config

import (
	"strings"
	"testing"
)

// validUDPBase 最小合法配置（过 Validate 的其余必填项），UDP 段由各用例覆写。
func validUDPBase() *Config {
	return &Config{
		HttpPort:   8081,
		Belong:     "default",
		ServerID:   "gateway-test",
		ServerType: "Gateway",
		Zone:       "default",
		Transports: []Transport{{Port: 48080}},
		GRPC:       GRPCConfig{Port: 50051},
	}
}

// TestUDPDefaultsApplied 零值 UDP 段补齐默认（enabled 保持显式值不被改写）。
func TestUDPDefaultsApplied(t *testing.T) {
	cfg := validUDPBase()
	cfg.ApplyRuntimeDefaults()

	if cfg.UDP.Enabled {
		t.Error("UDP.Enabled default should stay false (预留通道现状)")
	}
	if cfg.UDP.AssociateTimeout != DefaultUDPAssociateTimeout {
		t.Errorf("AssociateTimeout=%q, want %q", cfg.UDP.AssociateTimeout, DefaultUDPAssociateTimeout)
	}
	if cfg.UDP.IdleTimeout != DefaultUDPIdleTimeout {
		t.Errorf("IdleTimeout=%q, want %q", cfg.UDP.IdleTimeout, DefaultUDPIdleTimeout)
	}
	if cfg.UDP.MaxPerIP != DefaultUDPMaxPerIP || cfg.UDP.MaxAssociations != DefaultUDPMaxAssociations {
		t.Errorf("limits = %d/%d, want %d/%d", cfg.UDP.MaxPerIP, cfg.UDP.MaxAssociations, DefaultUDPMaxPerIP, DefaultUDPMaxAssociations)
	}
	if cfg.UDP.SeqWindow != DefaultUDPSeqWindow || cfg.UDP.MaxDatagram != DefaultUDPMaxDatagram {
		t.Errorf("seqWindow/maxDatagram = %d/%d, want %d/%d", cfg.UDP.SeqWindow, cfg.UDP.MaxDatagram, DefaultUDPSeqWindow, DefaultUDPMaxDatagram)
	}
}

// TestValidateUDPEnabledRequiresUDPTransport enabled=true 但无 type=udp 监听：
// ASSOCIATE 永远到不了，属配置错误（fail-fast 而非静默失效）。
func TestValidateUDPEnabledRequiresUDPTransport(t *testing.T) {
	cfg := validUDPBase()
	cfg.ApplyRuntimeDefaults()
	cfg.UDP.Enabled = true // transports 只有 tcp

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "type=udp") {
		t.Fatalf("Validate err=%v, want requires type=udp transport", err)
	}

	cfg.Transports = append(cfg.Transports, Transport{Port: 48082, Type: "udp"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with udp transport: %v, want nil", err)
	}
}

// TestValidateUDPEnabledBadParams enabled=true 时超时/上限必须自洽。
func TestValidateUDPEnabledBadParams(t *testing.T) {
	base := func() *Config {
		cfg := validUDPBase()
		cfg.Transports = append(cfg.Transports, Transport{Port: 48082, Type: "udp"})
		cfg.ApplyRuntimeDefaults()
		cfg.UDP.Enabled = true
		return cfg
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad_associate_timeout", func(c *Config) { c.UDP.AssociateTimeout = "nonsense" }, "associateTimeout"},
		{"zero_idle_timeout", func(c *Config) { c.UDP.IdleTimeout = "0s" }, "idleTimeout"},
		{"zero_max_per_ip", func(c *Config) { c.UDP.MaxPerIP = 0 }, "maxPerIP"},
		{"zero_max_assoc", func(c *Config) { c.UDP.MaxAssociations = -1 }, "maxAssociations"},
		{"zero_seq_window", func(c *Config) { c.UDP.SeqWindow = 0 }, "seqWindow"},
		{"zero_max_datagram", func(c *Config) { c.UDP.MaxDatagram = 0 }, "maxDatagram"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base()
			c.mutate(cfg)
			// 0 值会被 ApplyRuntimeDefaults 补回，需在 defaults 之后再破坏
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate err=%v, want contains %q", err, c.want)
			}
		})
	}
}

// TestValidateUDPDisabledSkipsChecks enabled=false 时不校验 UDP 段（预留通道现状）。
func TestValidateUDPDisabledSkipsChecks(t *testing.T) {
	cfg := validUDPBase()
	cfg.ApplyRuntimeDefaults()
	cfg.UDP.AssociateTimeout = "bad" // 关闭态下脏值不阻断启动
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled UDP Validate: %v, want nil", err)
	}
}
