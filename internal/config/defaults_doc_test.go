package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDocDefaults_MatchREADME README「默认值」列必须与代码零值回退一致（防文档漂移）。
func TestDocDefaults_MatchREADME(t *testing.T) {
	p := filepath.Join(t.TempDir(), "min.yaml")
	yaml := "httpPort: 8080\nbelong: b\nserverId: s\nserverType: Gateway\nzone: z\n" +
		"transports:\n  - protocol: tcp\n    port: 1\n"
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checks := []struct {
		name string
		got  bool
		want bool
	}{
		{"security.enabled", c.Security.Enabled, false},
		{"rateLimit.enabled", c.Security.RateLimit.Enabled, false},
		{"circuitBreaker.enabled", c.Security.CircuitBreaker.Enabled, false},
		{"dropOnOverload", c.Protection.DropOnOverload, false},
		{"discovery.enabled", c.Discovery.Enabled, false},
		{"gatewayDiscovery", c.Discovery.GatewayDiscovery, false},
		{"registerSelf", c.Discovery.RegisterSelf, false},
		{"stream.batchPush", c.Stream.BatchPush, false},
		{"loginValidation.enabled", c.LoginValidation.Enabled, false},
		{"cluster.enabled", c.Cluster.Enabled, false},
		{"waf.enabled", c.WAF.Enabled, false},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s default=%v want %v (update README 6.x table)", ch.name, ch.got, ch.want)
		}
	}
	// 非布尔默认值
	if c.GRPC.WindowSize != DefaultGatewayGRPCWindowSize {
		t.Errorf("grpc.windowSize=%d want %d", c.GRPC.WindowSize, DefaultGatewayGRPCWindowSize)
	}
	if c.Stream.SendChannelSize != DefaultGatewayStreamSendChannelSize {
		t.Errorf("stream.sendChannelSize=%d want %d", c.Stream.SendChannelSize, DefaultGatewayStreamSendChannelSize)
	}
	if c.Protection.ConnIdleTimeout != "5m" {
		t.Errorf("connIdleTimeout=%q want 5m", c.Protection.ConnIdleTimeout)
	}
	if c.Security.RateLimit.MaxTokens != DefaultRateLimitMaxTokens {
		t.Errorf("maxTokens=%d want %d", c.Security.RateLimit.MaxTokens, DefaultRateLimitMaxTokens)
	}
}
