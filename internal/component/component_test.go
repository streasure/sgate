package component

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	clusterPkg "github.com/streasure/sgate/internal/cluster"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/traffic"
	"github.com/streasure/sgate/internal/types"
)

// mustLoadConfig 写最小合法 yaml 并 config.Load（组件构造器要求 config.Get() 非空）。
// extraYAML 追加在最小骨架之后，供各用例覆盖特性开关。
func mustLoadConfig(t *testing.T, extraYAML string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "httpPort: 8080\nbelong: b\nserverId: s\nserverType: Gateway\nzone: z\n" +
		"transports:\n  - port: 1\n" + extraYAML
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// resetComponentResources 清空全局资源，防跨用例污染。
func resetComponentResources(t *testing.T) {
	t.Helper()
	clear := func() {
		globalWhitelistBlacklist = nil
		globalWAF = nil
		globalRateLimiter = nil
		globalJWTAuth = nil
		globalCircuitBreakerMgr = nil
		globalTracer = nil
		globalOTelTracer = nil
		globalLatencyTracker = nil
		globalCanaryFilter = nil
		globalTrafficMirror = nil
		globalDegradation = nil
		globalConfigCenter = nil
		globalAlertWebhook = nil
	}
	clear()
	t.Cleanup(clear)
}

func TestComponents_NameAndOrder(t *testing.T) {
	mustLoadConfig(t, "")
	tests := []struct {
		name  string
		order int
		comp  interface {
			Name() string
			Order() int
		}
	}{
		{"security", 100, NewSecurityComponent()},
		{"observability", 200, NewObservabilityComponent()},
		{"traffic", 300, NewTrafficComponent()},
		{"cluster", 400, NewClusterComponent()},
	}
	for _, tt := range tests {
		if got := tt.comp.Name(); got != tt.name {
			t.Errorf("Name() = %q, want %q", got, tt.name)
		}
		if got := tt.comp.Order(); got != tt.order {
			t.Errorf("%s Order() = %d, want %d", tt.name, got, tt.order)
		}
	}
}

func TestConstructors_CopyConfigFromGlobal(t *testing.T) {
	cfg := mustLoadConfig(t, `
canary:
  enabled: true
  percent: 42
grpc:
  port: 6000
monitoring:
  disableTracer: true
  pprofAddr: "127.0.0.1:6061"
`)
	sec := NewSecurityComponent()
	if !reflect.DeepEqual(sec.cfg, cfg.Security) || !reflect.DeepEqual(sec.waf, cfg.WAF) || !reflect.DeepEqual(sec.jwt, cfg.JWTAuth) {
		t.Error("SecurityComponent did not copy config sections")
	}
	obs := NewObservabilityComponent()
	if !reflect.DeepEqual(obs.otelCfg, cfg.OTelTracer) || obs.pprofAddr != "127.0.0.1:6061" || !obs.disableTracer {
		t.Errorf("ObservabilityComponent config copy mismatch: otel=%+v pprof=%q disableTracer=%v",
			obs.otelCfg, obs.pprofAddr, obs.disableTracer)
	}
	tr := NewTrafficComponent()
	if !reflect.DeepEqual(tr.canaryCfg, cfg.Canary) || !reflect.DeepEqual(tr.mirrorCfg, cfg.TrafficMirror) ||
		!reflect.DeepEqual(tr.degradationCfg, cfg.Degradation) || !reflect.DeepEqual(tr.wasmCfg, cfg.WasmRuntime) {
		t.Error("TrafficComponent did not copy config sections")
	}
	cl := NewClusterComponent()
	if cl.grpcPort != 6000 {
		t.Errorf("ClusterComponent.grpcPort = %d, want 6000", cl.grpcPort)
	}
	if cl.cfg.ServerID != cfg.ServerID || cl.cfg.Zone != cfg.Zone {
		t.Error("ClusterComponent did not copy identity config")
	}
}

func TestResources_GettersNilBeforeInit(t *testing.T) {
	resetComponentResources(t)
	getters := []struct {
		name  string
		isNil func() bool
	}{
		{"WhitelistBlacklist", func() bool { return WhitelistBlacklist() == nil }},
		{"WAF", func() bool { return WAF() == nil }},
		{"RateLimiter", func() bool { return RateLimiter() == nil }},
		{"JWTAuth", func() bool { return JWTAuth() == nil }},
		{"CircuitBreakerMgr", func() bool { return CircuitBreakerMgr() == nil }},
		{"Tracer", func() bool { return Tracer() == nil }},
		{"OTelTracer", func() bool { return OTelTracer() == nil }},
		{"LatencyTracker", func() bool { return LatencyTracker() == nil }},
		{"CanaryFilter", func() bool { return CanaryFilter() == nil }},
		{"TrafficMirror", func() bool { return TrafficMirror() == nil }},
		{"Degradation", func() bool { return Degradation() == nil }},
		{"ConfigCenter", func() bool { return ConfigCenter() == nil }},
		{"AlertWebhook", func() bool { return AlertWebhook() == nil }},
	}
	for _, g := range getters {
		if !g.isNil() {
			t.Errorf("%s() = non-nil before Init, want nil", g.name)
		}
	}
}

// stubConfigCenter 仅用于资源发布回环测试（不连 etcd）。
type stubConfigCenter struct{}

func (stubConfigCenter) Watch(_ context.Context) (<-chan []byte, error) { return nil, nil }
func (stubConfigCenter) Pull(_ context.Context) ([]byte, error)         { return nil, nil }
func (stubConfigCenter) Type() string                                   { return "stub" }
func (stubConfigCenter) Stop()                                          {}

func TestResources_SettersGettersRoundTrip(t *testing.T) {
	resetComponentResources(t)

	wb := security.NewWhitelistBlacklist()
	waf := security.NewWAF(config.WAFConfig{})
	rl := security.NewRateLimiter(10, time.Second)
	defer rl.Stop()
	jwt := security.NewJWTAuthFilter(config.JWTAuthConfig{})
	cb := security.NewCircuitBreakerManager()
	setSecurityResources(wb, waf, rl, jwt, cb)
	if WhitelistBlacklist() != wb || WAF() != waf || RateLimiter() != rl || JWTAuth() != jwt || CircuitBreakerMgr() != cb {
		t.Error("security getters do not return the pointers passed to setSecurityResources")
	}

	tr := &obs.Tracer{}
	otel := &obs.OTelTracer{}
	lt := &obs.LatencyTracker{}
	setObservabilityResources(tr, otel, lt)
	if Tracer() != tr || OTelTracer() != otel || LatencyTracker() != lt {
		t.Error("observability getters do not return the pointers passed to setObservabilityResources")
	}

	canary := traffic.NewCanaryFilter(config.CanaryConfig{})
	mirror := traffic.NewTrafficMirror(config.TrafficMirrorConfig{})
	defer mirror.Stop()
	degrade := traffic.NewDegradationManager(nil)
	setTrafficResources(canary, mirror, degrade)
	if CanaryFilter() != canary || TrafficMirror() != mirror || Degradation() != degrade {
		t.Error("traffic getters do not return the pointers passed to setTrafficResources")
	}

	cc := clusterPkg.ConfigCenter(stubConfigCenter{})
	hook := clusterPkg.NewAlertWebhook(config.AlertWebhookConfig{})
	setClusterInitResources(cc, hook)
	if ConfigCenter() != cc || AlertWebhook() != hook {
		t.Error("cluster init getters do not match setClusterInitResources arguments")
	}
}

func TestSecurityComponent_Init_Disabled(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, "")

	c := NewSecurityComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if WhitelistBlacklist() != nil || WAF() != nil || RateLimiter() != nil ||
		JWTAuth() != nil || CircuitBreakerMgr() != nil {
		t.Error("disabled security component published non-nil resources")
	}
	if got := types.GetFilterChain().Len(); got != 0 {
		t.Errorf("filter chain Len() = %d after disabled Init, want 0", got)
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestSecurityComponent_Init_Enabled(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, `
security:
  enabled: true
  whitelist: ["10.0.0.1"]
  blacklist: ["192.168.1.9"]
  rateLimit:
    enabled: true
    maxTokens: 100
  circuitBreaker:
    enabled: true
waf:
  enabled: true
`)

	c := NewSecurityComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	wbl := WhitelistBlacklist()
	if wbl == nil {
		t.Fatal("WhitelistBlacklist() = nil after enabled Init")
	}
	if !wbl.IsInWhitelist("10.0.0.1") {
		t.Error("configured whitelist IP not whitelisted")
	}
	if !wbl.IsInBlacklist("192.168.1.9") {
		t.Error("configured blacklist IP not blacklisted")
	}
	if WAF() == nil || RateLimiter() == nil || CircuitBreakerMgr() == nil {
		t.Error("expected non-nil WAF/RateLimiter/CircuitBreakerMgr after enabled Init")
	}
	if JWTAuth() != nil {
		t.Error("JWTAuth() non-nil though jwtAuth disabled")
	}
}

func TestSecurityComponent_Init_JWT(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, "")

	// 组件级 fail-closed：Validate 已拦空 secret，此处直改热更路径的配置字段，
	// 验证 Init 仍二次兜底拒绝（空 HS256 密钥可伪造任意令牌）。
	c := NewSecurityComponent()
	c.jwt = config.JWTAuthConfig{Enabled: true, Secret: ""}
	if err := c.Init(); err == nil {
		t.Fatal("Init() with jwtAuth.enabled and empty secret error = nil, want fail-closed error")
	}

	mustLoadConfig(t, `
jwtAuth:
  enabled: true
  secret: "s3cret"
  issuer: "test-issuer"
`)
	c = NewSecurityComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() with jwt secret error = %v", err)
	}
	defer c.Destroy()
	if JWTAuth() == nil {
		t.Error("JWTAuth() = nil after enabled Init")
	}
	if got := types.GetFilterChain().Len(); got != 1 {
		t.Errorf("filter chain Len() = %d, want 1 (jwt filter)", got)
	}
}

func TestObservabilityComponent_Init(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, `
monitoring:
  disableTracer: true
`)

	c := NewObservabilityComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if Tracer() != nil {
		t.Error("Tracer() non-nil though monitoring.disableTracer=true")
	}
	if LatencyTracker() == nil {
		t.Error("LatencyTracker() = nil after Init")
	}
	if OTelTracer() != nil {
		t.Error("OTelTracer() non-nil though otel disabled")
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestTrafficComponent_Init_Disabled(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, "")

	c := NewTrafficComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if CanaryFilter() != nil || TrafficMirror() != nil || Degradation() != nil {
		t.Error("disabled traffic component published non-nil resources")
	}
	if got := types.GetFilterChain().Len(); got != 0 {
		t.Errorf("filter chain Len() = %d, want 0", got)
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestTrafficComponent_Init_AllEnabled(t *testing.T) {
	resetComponentResources(t)
	types.InitFilterChain()
	mustLoadConfig(t, `
canary:
  enabled: true
  percent: 100
trafficMirror:
  enabled: true
  percent: 100
  queueSize: 64
  workers: 1
degradation:
  enabled: true
  rules:
    - route: "login"
      errorThreshold: 0.5
      windowSize: 4
      fallbackData: "FB"
`)

	c := NewTrafficComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}

	if CanaryFilter() == nil || TrafficMirror() == nil || Degradation() == nil {
		t.Fatal("expected non-nil canary/mirror/degradation after enabled Init")
	}
	if got := types.GetFilterChain().Len(); got != 3 {
		t.Errorf("filter chain Len() = %d, want 3 (canary+mirror+degradation)", got)
	}
}

func TestTrafficComponent_Init_WasmModuleValidation(t *testing.T) {
	tests := []struct {
		name    string
		modules string
	}{
		{
			name:    "missing name and path",
			modules: "    - name: \"\"\n      path: \"\"\n",
		},
		{
			name:    "module file does not exist",
			modules: "    - name: \"m\"\n      path: \"no/such/module.wasm\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetComponentResources(t)
			types.InitFilterChain()
			mustLoadConfig(t, "wasmRuntime:\n  enabled: true\n  modules:\n"+tt.modules)

			c := NewTrafficComponent()
			if err := c.Init(); err == nil {
				t.Fatal("Init() with invalid wasm modules error = nil, want fail-fast error")
			}
			c.Destroy() // 未创建 mirror，Destroy 必须安全
		})
	}
}

func TestClusterComponent_Init_Disabled(t *testing.T) {
	resetComponentResources(t)
	mustLoadConfig(t, "")

	c := NewClusterComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if ConfigCenter() != nil || AlertWebhook() != nil {
		t.Error("disabled cluster component published non-nil init resources")
	}
}

func TestClusterComponent_Init_AlertEnabled(t *testing.T) {
	resetComponentResources(t)
	mustLoadConfig(t, `
alert:
  enabled: true
  rateLimit: 60
  webhooks:
    - name: "ops"
      url: "http://127.0.0.1:1/hook"
      type: generic
`)

	c := NewClusterComponent()
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if AlertWebhook() == nil {
		t.Error("AlertWebhook() = nil after alert.enabled Init")
	}
}

func TestBuildRegisterAddress(t *testing.T) {
	cfg := config.Config{
		Transports: []config.Transport{
			{Port: 48080},
			{Port: 48081, Type: "websocket"},
			{Port: 48082}, // 第二个非 ws 端口应被忽略（TCP 只取首个）
		},
	}
	raw := buildRegisterAddress(cfg, 50051)

	var addr struct {
		IP        string `json:"ip"`
		GRPC      int    `json:"grpc"`
		TCP       string `json:"tcp"`
		WebSocket string `json:"websocket"`
	}
	if err := json.Unmarshal([]byte(raw), &addr); err != nil {
		t.Fatalf("buildRegisterAddress output is not valid JSON: %v (%s)", err, raw)
	}
	if addr.GRPC != 50051 {
		t.Errorf("grpc = %d, want 50051", addr.GRPC)
	}
	if addr.IP == "" {
		t.Error("ip is empty")
	}
	if want := addr.IP + ":48080"; addr.TCP != want {
		t.Errorf("tcp = %q, want %q (first non-ws transport)", addr.TCP, want)
	}
	if want := addr.IP + ":48081"; addr.WebSocket != want {
		t.Errorf("websocket = %q, want %q", addr.WebSocket, want)
	}
}
