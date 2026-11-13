package component

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/streasure/sgate/internal/config"
)

// mustLoadConfig 写最小合法 yaml 并 config.Load，返回 cfg 供组件构造器显式传入。
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

func TestComponents_NameAndOrder(t *testing.T) {
	cfg := mustLoadConfig(t, "")
	res := NewResources()
	tests := []struct {
		name  string
		order int
		comp  interface {
			Name() string
			Order() int
		}
	}{
		{"security", 100, NewSecurityComponent(cfg, res)},
		{"observability", 200, NewObservabilityComponent(cfg, res)},
		{"traffic", 300, NewTrafficComponent(cfg, res)},
		{"cluster", 400, NewClusterComponent(cfg, res)},
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

func TestConstructors_CopyConfig(t *testing.T) {
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
	res := NewResources()
	sec := NewSecurityComponent(cfg, res)
	if !reflect.DeepEqual(sec.cfg, cfg.Security) || !reflect.DeepEqual(sec.waf, cfg.WAF) || !reflect.DeepEqual(sec.jwt, cfg.JWTAuth) {
		t.Error("SecurityComponent did not copy config sections")
	}
	obs := NewObservabilityComponent(cfg, res)
	if !reflect.DeepEqual(obs.otelCfg, cfg.OTelTracer) || obs.pprofAddr != "127.0.0.1:6061" || !obs.disableTracer {
		t.Errorf("ObservabilityComponent config copy mismatch: otel=%+v pprof=%q disableTracer=%v",
			obs.otelCfg, obs.pprofAddr, obs.disableTracer)
	}
	tr := NewTrafficComponent(cfg, res)
	if !reflect.DeepEqual(tr.canaryCfg, cfg.Canary) || !reflect.DeepEqual(tr.mirrorCfg, cfg.TrafficMirror) ||
		!reflect.DeepEqual(tr.degradationCfg, cfg.Degradation) || !reflect.DeepEqual(tr.wasmCfg, cfg.WasmRuntime) {
		t.Error("TrafficComponent did not copy config sections")
	}
	cl := NewClusterComponent(cfg, res)
	if cl.grpcPort != 6000 {
		t.Errorf("ClusterComponent.grpcPort = %d, want 6000", cl.grpcPort)
	}
	if cl.cfg.ServerID != cfg.ServerID || cl.cfg.Zone != cfg.Zone {
		t.Error("ClusterComponent did not copy identity config")
	}
}

// TestResources_NewEmpty 新建资源容器：滤链就绪且为空，其余资源在组件
// Init 前为 nil（替代旧的全局量 getter nil 守卫测试）。
func TestResources_NewEmpty(t *testing.T) {
	res := NewResources()
	if res.FC == nil {
		t.Fatal("NewResources().FC = nil, want fresh filter chain")
	}
	if got := res.FC.Len(); got != 0 {
		t.Errorf("fresh filter chain Len() = %d, want 0", got)
	}
	fields := []struct {
		name  string
		isNil bool
	}{
		{"WhitelistBlacklist", res.WhitelistBlacklist == nil},
		{"WAF", res.WAF == nil},
		{"RateLimiter", res.RateLimiter == nil},
		{"JWTAuth", res.JWTAuth == nil},
		{"CircuitBreakerMgr", res.CircuitBreakerMgr == nil},
		{"Tracer", res.Tracer == nil},
		{"OTelTracer", res.OTelTracer == nil},
		{"LatencyTracker", res.LatencyTracker == nil},
		{"CanaryFilter", res.CanaryFilter == nil},
		{"TrafficMirror", res.TrafficMirror == nil},
		{"Degradation", res.Degradation == nil},
		{"ConfigCenter", res.ConfigCenter == nil},
		{"AlertWebhook", res.AlertWebhook == nil},
	}
	for _, f := range fields {
		if !f.isNil {
			t.Errorf("%s = non-nil before component Init, want nil", f.name)
		}
	}
}

func TestSecurityComponent_Init_Disabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, "")

	c := NewSecurityComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if res.WhitelistBlacklist != nil || res.WAF != nil || res.RateLimiter != nil ||
		res.JWTAuth != nil || res.CircuitBreakerMgr != nil {
		t.Error("disabled security component published non-nil resources")
	}
	if got := res.FC.Len(); got != 0 {
		t.Errorf("filter chain Len() = %d after disabled Init, want 0", got)
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestSecurityComponent_Init_Enabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, `
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

	c := NewSecurityComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	wbl := res.WhitelistBlacklist
	if wbl == nil {
		t.Fatal("res.WhitelistBlacklist = nil after enabled Init")
	}
	if !wbl.IsInWhitelist("10.0.0.1") {
		t.Error("configured whitelist IP not whitelisted")
	}
	if !wbl.IsInBlacklist("192.168.1.9") {
		t.Error("configured blacklist IP not blacklisted")
	}
	if res.WAF == nil || res.RateLimiter == nil || res.CircuitBreakerMgr == nil {
		t.Error("expected non-nil WAF/RateLimiter/CircuitBreakerMgr after enabled Init")
	}
	if res.JWTAuth != nil {
		t.Error("res.JWTAuth non-nil though jwtAuth disabled")
	}
}

func TestSecurityComponent_Init_JWT(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, "")

	// 组件级 fail-closed：Validate 已拦空 secret，此处直改热更路径的配置字段，
	// 验证 Init 仍二次兜底拒绝（空 HS256 密钥可伪造任意令牌）。
	c := NewSecurityComponent(cfg, res)
	c.jwt = config.JWTAuthConfig{Enabled: true, Secret: ""}
	if err := c.Init(); err == nil {
		t.Fatal("Init() with jwtAuth.enabled and empty secret error = nil, want fail-closed error")
	}

	cfg2 := mustLoadConfig(t, `
jwtAuth:
  enabled: true
  secret: "s3cret"
  issuer: "test-issuer"
`)
	c = NewSecurityComponent(cfg2, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() with jwt secret error = %v", err)
	}
	defer c.Destroy()
	if res.JWTAuth == nil {
		t.Error("res.JWTAuth = nil after enabled Init")
	}
	if got := res.FC.Len(); got != 1 {
		t.Errorf("filter chain Len() = %d, want 1 (jwt filter)", got)
	}
}

func TestObservabilityComponent_Init(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, `
monitoring:
  disableTracer: true
`)

	c := NewObservabilityComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if res.Tracer != nil {
		t.Error("res.Tracer non-nil though monitoring.disableTracer=true")
	}
	if res.LatencyTracker == nil {
		t.Error("res.LatencyTracker = nil after Init")
	}
	if res.OTelTracer != nil {
		t.Error("res.OTelTracer non-nil though otel disabled")
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestTrafficComponent_Init_Disabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, "")

	c := NewTrafficComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if res.CanaryFilter != nil || res.TrafficMirror != nil || res.Degradation != nil {
		t.Error("disabled traffic component published non-nil resources")
	}
	if got := res.FC.Len(); got != 0 {
		t.Errorf("filter chain Len() = %d, want 0", got)
	}
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}
}

func TestTrafficComponent_Init_AllEnabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, `
canary:
  enabled: true
  percent: 100
trafficMirror:
  enabled: true
  percent: 100
  targetAddr: "127.0.0.1:50099"
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

	c := NewTrafficComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()
	if err := c.Start(); err != nil {
		t.Errorf("Start() error = %v", err)
	}

	if res.CanaryFilter == nil || res.TrafficMirror == nil || res.Degradation == nil {
		t.Fatal("expected non-nil canary/mirror/degradation after enabled Init")
	}
	if got := res.FC.Len(); got != 3 {
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
			res := NewResources()
			cfg := mustLoadConfig(t, "wasmRuntime:\n  enabled: true\n  modules:\n"+tt.modules)

			c := NewTrafficComponent(cfg, res)
			if err := c.Init(); err == nil {
				t.Fatal("Init() with invalid wasm modules error = nil, want fail-fast error")
			}
			c.Destroy() // 未创建 mirror，Destroy 必须安全
		})
	}
}

func TestClusterComponent_Init_Disabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, "")

	c := NewClusterComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if res.ConfigCenter != nil || res.AlertWebhook != nil {
		t.Error("disabled cluster component published non-nil init resources")
	}
}

func TestClusterComponent_Init_AlertEnabled(t *testing.T) {
	res := NewResources()
	cfg := mustLoadConfig(t, `
alert:
  enabled: true
  rateLimit: 60
  webhooks:
    - name: "ops"
      url: "http://127.0.0.1:1/hook"
      type: generic
`)

	c := NewClusterComponent(cfg, res)
	if err := c.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer c.Destroy()

	if res.AlertWebhook == nil {
		t.Error("res.AlertWebhook = nil after alert.enabled Init")
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
