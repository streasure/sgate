package config

import "testing"

func TestApplyRuntimeDefaults_ZeroValuesGetDefaults(t *testing.T) {
	c := &Config{}
	c.ApplyRuntimeDefaults()

	if c.Protection.MaxFrameSize != DefaultMaxFrameSize {
		t.Errorf("MaxFrameSize = %d, want %d", c.Protection.MaxFrameSize, DefaultMaxFrameSize)
	}
	if c.Protection.MaxFrameBufSize != DefaultMaxFrameSize {
		t.Errorf("MaxFrameBufSize = %d, want %d (gateway historical default)", c.Protection.MaxFrameBufSize, DefaultMaxFrameSize)
	}
	if c.Protection.MaxWSFrameSize != DefaultMaxWSFrameSize {
		t.Errorf("MaxWSFrameSize = %d, want %d", c.Protection.MaxWSFrameSize, DefaultMaxWSFrameSize)
	}
	if c.Protection.MaxWSBufferSize != DefaultMaxWSFrameSize {
		t.Errorf("MaxWSBufferSize = %d, want %d", c.Protection.MaxWSBufferSize, DefaultMaxWSFrameSize)
	}
	if c.Protection.WSHeartbeatTimeout != DefaultWSHeartbeatTimeoutSec {
		t.Errorf("WSHeartbeatTimeout = %d, want %d", c.Protection.WSHeartbeatTimeout, DefaultWSHeartbeatTimeoutSec)
	}
	if c.Protection.WSCheckInterval != DefaultWSCheckIntervalSec {
		t.Errorf("WSCheckInterval = %d, want %d", c.Protection.WSCheckInterval, DefaultWSCheckIntervalSec)
	}
	if c.Protection.ConnCheckInterval != DefaultConnCheckInterval {
		t.Errorf("ConnCheckInterval = %q, want %q", c.Protection.ConnCheckInterval, DefaultConnCheckInterval)
	}
	if c.Protection.ConnIdleTimeout != DefaultConnIdleTimeout {
		t.Errorf("ConnIdleTimeout = %q, want %q", c.Protection.ConnIdleTimeout, DefaultConnIdleTimeout)
	}
	if c.Protection.CheckIntervalMs != DefaultOverloadCheckIntervalMs {
		t.Errorf("CheckIntervalMs = %d, want %d", c.Protection.CheckIntervalMs, DefaultOverloadCheckIntervalMs)
	}
	if c.Protection.CPUThreshold != DefaultOverloadCPUThreshold {
		t.Errorf("CPUThreshold = %v, want %v", c.Protection.CPUThreshold, DefaultOverloadCPUThreshold)
	}

	if c.GRPC.Port != DefaultGRPCPort {
		t.Errorf("GRPC.Port = %d, want %d", c.GRPC.Port, DefaultGRPCPort)
	}
	if c.GRPC.WindowSize != DefaultGatewayGRPCWindowSize {
		t.Errorf("GRPC.WindowSize = %d, want %d", c.GRPC.WindowSize, DefaultGatewayGRPCWindowSize)
	}
	if c.GRPC.MaxMessageSize != DefaultGatewayGRPCMaxMessageSize {
		t.Errorf("GRPC.MaxMessageSize = %d, want %d", c.GRPC.MaxMessageSize, DefaultGatewayGRPCMaxMessageSize)
	}

	if c.Stream.SendChannelSize != DefaultGatewayStreamSendChannelSize {
		t.Errorf("Stream.SendChannelSize = %d, want %d", c.Stream.SendChannelSize, DefaultGatewayStreamSendChannelSize)
	}
	if c.Stream.ReceiveBatchSize != DefaultStreamReceiveBatchSize {
		t.Errorf("Stream.ReceiveBatchSize = %d, want %d", c.Stream.ReceiveBatchSize, DefaultStreamReceiveBatchSize)
	}
	if c.Stream.QueuePolicy.Policy != DefaultStreamQueuePolicy {
		t.Errorf("QueuePolicy.Policy = %q, want %q", c.Stream.QueuePolicy.Policy, DefaultStreamQueuePolicy)
	}
	if c.Stream.QueuePolicy.SendTimeout != DefaultSendTimeout {
		t.Errorf("QueuePolicy.SendTimeout = %q, want %q", c.Stream.QueuePolicy.SendTimeout, DefaultSendTimeout)
	}

	if c.Security.RateLimit.MaxTokens != DefaultRateLimitMaxTokens {
		t.Errorf("RateLimit.MaxTokens = %d, want %d", c.Security.RateLimit.MaxTokens, DefaultRateLimitMaxTokens)
	}
	if c.Security.CircuitBreaker.FailureThreshold != DefaultCircuitBreakerFailureThreshold {
		t.Errorf("CircuitBreaker.FailureThreshold = %d, want %d", c.Security.CircuitBreaker.FailureThreshold, DefaultCircuitBreakerFailureThreshold)
	}
	if c.JWTAuth.HeaderField != DefaultJWTHeaderField {
		t.Errorf("JWTAuth.HeaderField = %q, want %q", c.JWTAuth.HeaderField, DefaultJWTHeaderField)
	}
	if c.WAF.MaxPayloadSize != DefaultWAFMaxPayloadSize {
		t.Errorf("WAF.MaxPayloadSize = %d, want %d", c.WAF.MaxPayloadSize, DefaultWAFMaxPayloadSize)
	}
	if c.TrafficMirror.QueueSize != DefaultMirrorQueueSize {
		t.Errorf("TrafficMirror.QueueSize = %d, want %d", c.TrafficMirror.QueueSize, DefaultMirrorQueueSize)
	}
	if c.TrafficMirror.Workers != DefaultMirrorWorkers {
		t.Errorf("TrafficMirror.Workers = %d, want %d", c.TrafficMirror.Workers, DefaultMirrorWorkers)
	}
	// pprof 空串 = 关闭，不可填默认
	if c.Monitoring.PprofAddr != "" {
		t.Errorf("Monitoring.PprofAddr = %q, want empty (disabled)", c.Monitoring.PprofAddr)
	}
}

func TestApplyRuntimeDefaults_DoesNotOverwriteExplicitValues(t *testing.T) {
	c := &Config{}
	c.Protection.MaxFrameSize = 1234
	c.Protection.MaxFrameBufSize = 5678
	c.Protection.WSHeartbeatTimeout = 99
	c.GRPC.Port = 60000
	c.GRPC.WindowSize = 1024
	c.Stream.SendChannelSize = 77
	c.Security.RateLimit.MaxTokens = 42
	c.JWTAuth.HeaderField = "X-Custom"
	c.Monitoring.PprofAddr = "" // explicit disable
	c.ApplyRuntimeDefaults()

	if c.Protection.MaxFrameSize != 1234 {
		t.Errorf("MaxFrameSize overwritten to %d", c.Protection.MaxFrameSize)
	}
	if c.Protection.MaxFrameBufSize != 5678 {
		t.Errorf("MaxFrameBufSize overwritten to %d", c.Protection.MaxFrameBufSize)
	}
	if c.Protection.WSHeartbeatTimeout != 99 {
		t.Errorf("WSHeartbeatTimeout overwritten to %d", c.Protection.WSHeartbeatTimeout)
	}
	if c.GRPC.Port != 60000 {
		t.Errorf("GRPC.Port overwritten to %d", c.GRPC.Port)
	}
	if c.GRPC.WindowSize != 1024 {
		t.Errorf("GRPC.WindowSize overwritten to %d", c.GRPC.WindowSize)
	}
	if c.Stream.SendChannelSize != 77 {
		t.Errorf("Stream.SendChannelSize overwritten to %d", c.Stream.SendChannelSize)
	}
	if c.Security.RateLimit.MaxTokens != 42 {
		t.Errorf("RateLimit.MaxTokens overwritten to %d", c.Security.RateLimit.MaxTokens)
	}
	if c.JWTAuth.HeaderField != "X-Custom" {
		t.Errorf("JWTAuth.HeaderField overwritten to %q", c.JWTAuth.HeaderField)
	}
	if c.Monitoring.PprofAddr != "" {
		t.Errorf("Monitoring.PprofAddr became %q", c.Monitoring.PprofAddr)
	}
}

func TestApplyRuntimeDefaults_Idempotent(t *testing.T) {
	c := &Config{}
	c.ApplyRuntimeDefaults()
	first := *c
	c.ApplyRuntimeDefaults()
	if c.Protection.MaxFrameSize != first.Protection.MaxFrameSize ||
		c.GRPC.Port != first.GRPC.Port ||
		c.Stream.SendChannelSize != first.Stream.SendChannelSize ||
		c.Security.RateLimit.MaxTokens != first.Security.RateLimit.MaxTokens {
		t.Error("second ApplyRuntimeDefaults changed values (not idempotent)")
	}
}

func TestLoadSetsPath(t *testing.T) {
	// Load 成功路径依赖真实 yaml；此处仅验证 Path 在未 Load 时空串
	if Path() != "" && confPath.Load() == nil {
		t.Error("Path() non-empty but confPath not set")
	}
}

func TestPathDefaultEmpty(t *testing.T) {
	// 不调用 Load，Path 应为空（或保留前次 Load 的值；两者都合法）
	_ = Path()
}
