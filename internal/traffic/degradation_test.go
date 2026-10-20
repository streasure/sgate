package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
)

func TestDegradationManager_Metadata(t *testing.T) {
	m := NewDegradationManager(nil)
	if m.Name() != "degradation" {
		t.Errorf("Name() = %q, want degradation", m.Name())
	}
	if m.Phase() != types.PhaseForward {
		t.Errorf("Phase() = %v, want PhaseForward", m.Phase())
	}
	if m.Priority() != 500 {
		t.Errorf("Priority() = %d, want 500", m.Priority())
	}
}

func TestDegradationRule_Defaults(t *testing.T) {
	// 零值补齐：windowSize=0 / threshold<=0 时补默认，防止 RecordResult 除零
	r := newDegradationRule(config.DegradationRuleConfig{Route: "r1"})
	if r.windowSize != 100 {
		t.Errorf("windowSize = %d, want default 100", r.windowSize)
	}
	if r.errorThreshold != 0.5 {
		t.Errorf("errorThreshold = %v, want default 0.5", r.errorThreshold)
	}
	if len(r.recentErrors) != r.windowSize {
		t.Errorf("len(recentErrors) = %d, want %d", len(r.recentErrors), r.windowSize)
	}
	if r.coolDown != 30*time.Second {
		t.Errorf("coolDown = %v, want default 30s", r.coolDown)
	}

	// 显式值不被覆盖
	r = newDegradationRule(config.DegradationRuleConfig{
		Route:          "r2",
		ErrorThreshold: 0.25,
		WindowSize:     4,
		FallbackData:   "fb",
		CoolDown:       "5s",
	})
	if r.windowSize != 4 || r.errorThreshold != 0.25 || string(r.fallbackData) != "fb" || r.coolDown != 5*time.Second {
		t.Errorf("explicit rule values not preserved: %+v", r)
	}
}

func TestDegradationManager_Process_UnknownRoutePassthrough(t *testing.T) {
	m := NewDegradationManager(nil)
	fc := &types.FilterContext{Ctx: context.Background(), Route: "nope", Metadata: map[string]string{}}
	ok, err := m.Process(fc)
	if err != nil || !ok {
		t.Fatalf("Process(unknown route) = (%v, %v), want (true, nil)", ok, err)
	}
	if fc.Data != nil {
		t.Errorf("Data = %q, want untouched nil", fc.Data)
	}
}

func TestDegradationManager_TriggerAndFallback(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "login",
		ErrorThreshold: 0.5,
		WindowSize:     4,
		FallbackData:   "FALLBACK",
		CoolDown:       "1h",
	}})

	// 未触发前：透传
	fc := &types.FilterContext{Ctx: context.Background(), Route: "login", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, err := m.Process(fc); err != nil || !ok {
		t.Fatalf("Process before trigger = (%v, %v)", ok, err)
	}
	if string(fc.Data) != "orig" {
		t.Errorf("Data = %q, want untouched %q", fc.Data, "orig")
	}

	// 窗口填满且错误率达阈值：触发降级
	for i := 0; i < 3; i++ {
		m.RecordResult("login", true)
	}
	m.RecordResult("login", false) // 3/4 = 0.75 >= 0.5
	if got := m.GetTriggeredCount(); got != 1 {
		t.Fatalf("GetTriggeredCount() = %d, want 1", got)
	}

	// 冷却期内：替换为兜底数据
	fc = &types.FilterContext{Ctx: context.Background(), Route: "login", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, err := m.Process(fc); err != nil || !ok {
		t.Fatalf("Process degraded = (%v, %v)", ok, err)
	}
	if string(fc.Data) != "FALLBACK" {
		t.Errorf("Data = %q, want FALLBACK", fc.Data)
	}
	if fc.Metadata["degraded"] != "true" {
		t.Error("Metadata[degraded] not set")
	}

	// 持续错误不会重复累加触发计数（CompareAndSwap 保护）
	for i := 0; i < 8; i++ {
		m.RecordResult("login", true)
	}
	if got := m.GetTriggeredCount(); got != 1 {
		t.Errorf("GetTriggeredCount() = %d, want still 1", got)
	}
}

func TestDegradationManager_NoTriggerBelowThreshold(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "api",
		ErrorThreshold: 0.5,
		WindowSize:     4,
		FallbackData:   "FB",
	}})
	// 1/4 = 0.25 < 0.5：窗口满也不触发
	m.RecordResult("api", true)
	for i := 0; i < 3; i++ {
		m.RecordResult("api", false)
	}
	if got := m.GetTriggeredCount(); got != 0 {
		t.Errorf("GetTriggeredCount() = %d, want 0", got)
	}
	fc := &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false, want true")
	}
	if string(fc.Data) != "orig" {
		t.Errorf("Data = %q, want untouched", fc.Data)
	}
}

func TestDegradationManager_SlidingWindowEvictsOldErrors(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "api",
		ErrorThreshold: 0.5,
		WindowSize:     4,
		FallbackData:   "FB",
		CoolDown:       "1ms",
	}})
	// 触发一次
	for i := 0; i < 4; i++ {
		m.RecordResult("api", true)
	}
	if m.GetTriggeredCount() != 1 {
		t.Fatalf("expected trigger after 4 errors")
	}

	// 冷却过期后 Process 恢复
	r := m.rules["api"]
	r.lastDegrade.Store(time.Now().Add(-time.Hour).Unix())
	fc := &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false, want true")
	}
	if r.degraded.Load() != 0 {
		t.Fatal("expected degraded reset after cooldown")
	}
	if string(fc.Data) != "orig" {
		t.Errorf("Data = %q, want untouched after recovery", fc.Data)
	}

	// 滑动窗口：4 个成功把旧错误挤出，错误率归零
	for i := 0; i < 4; i++ {
		m.RecordResult("api", false)
	}
	if r.errorCount != 0 {
		t.Errorf("errorCount = %d, want 0 after successes evicted errors", r.errorCount)
	}
}

func TestDegradationManager_CooldownExpiryRecovers(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "api",
		ErrorThreshold: 0.5,
		WindowSize:     2,
		FallbackData:   "FB",
		CoolDown:       "1h",
	}})
	m.RecordResult("api", true)
	m.RecordResult("api", true)
	r := m.rules["api"]
	if r.degraded.Load() != 1 {
		t.Fatal("expected degraded=1")
	}

	// 冷却未过：仍是兜底
	fc := &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false, want true")
	}
	if string(fc.Data) != "FB" {
		t.Errorf("Data = %q, want FB during cooldown", fc.Data)
	}

	// 冷却到期：首次 Process 恢复，不再替换数据
	r.lastDegrade.Store(time.Now().Add(-2 * time.Hour).Unix())
	fc = &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false, want true")
	}
	if r.degraded.Load() != 0 {
		t.Error("degraded not reset after cooldown")
	}
	if string(fc.Data) != "orig" {
		t.Errorf("Data = %q, want untouched after cooldown", fc.Data)
	}
}

func TestDegradationManager_DisableEnable(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "api",
		ErrorThreshold: 0.5,
		WindowSize:     2,
		FallbackData:   "FB",
	}})
	m.RecordResult("api", true)
	m.RecordResult("api", true)

	m.Disable()
	fc := &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, err := m.Process(fc); err != nil || !ok {
		t.Fatalf("Process disabled = (%v, %v), want (true, nil)", ok, err)
	}
	if string(fc.Data) != "orig" {
		t.Errorf("Data = %q on disabled manager, want untouched", fc.Data)
	}

	m.Enable()
	fc = &types.FilterContext{Ctx: context.Background(), Route: "api", Data: []byte("orig"), Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false after Enable")
	}
	if string(fc.Data) != "FB" {
		t.Errorf("Data = %q, want FB after re-enable", fc.Data)
	}
}

func TestDegradationManager_RecordResultUnknownRouteNoop(t *testing.T) {
	m := NewDegradationManager(nil)
	// 未知路由与空规则均不应 panic
	m.RecordResult("ghost", true)
	if got := m.GetTriggeredCount(); got != 0 {
		t.Errorf("GetTriggeredCount() = %d, want 0", got)
	}
}

func TestDegradationManager_AddRuleZeroValuesNoDivByZero(t *testing.T) {
	m := NewDegradationManager(nil)
	// 零值 AddRule：补齐默认窗口/阈值后可安全记录（无除零 panic）
	m.AddRule(config.DegradationRuleConfig{Route: "hot"})
	for i := 0; i < 100; i++ {
		m.RecordResult("hot", true)
	}
	if got := m.GetTriggeredCount(); got != 1 {
		t.Errorf("GetTriggeredCount() = %d, want 1 (100%% errors over default window)", got)
	}

	fc := &types.FilterContext{Ctx: context.Background(), Route: "hot", Metadata: map[string]string{}}
	if ok, _ := m.Process(fc); !ok {
		t.Fatal("Process = false, want true")
	}
	if fc.Metadata["degraded"] != "true" {
		t.Error("route added via AddRule not degraded")
	}
}

func TestDegradationManager_AddRuleOverridesExisting(t *testing.T) {
	m := NewDegradationManager([]config.DegradationRuleConfig{{
		Route:          "api",
		ErrorThreshold: 0.5,
		WindowSize:     4,
		FallbackData:   "OLD",
	}})
	m.AddRule(config.DegradationRuleConfig{Route: "api", WindowSize: 2, FallbackData: "NEW", ErrorThreshold: 0.9})
	r := m.rules["api"]
	if string(r.fallbackData) != "NEW" || r.windowSize != 2 || r.errorThreshold != 0.9 {
		t.Errorf("AddRule did not override: fallback=%q window=%d threshold=%v",
			r.fallbackData, r.windowSize, r.errorThreshold)
	}
}
