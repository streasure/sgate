package traffic

import (
	"context"
	"strconv"
	"testing"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/uhash"
)

// findConnIDWithHash 在 "conn-N" 序列里找一个 FNV1a32%100 落在 [0, below) 的连接 ID。
func findConnIDWithHash(t *testing.T, below int) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := "conn-" + strconv.Itoa(i)
		if int(uhash.FNV1a32(id)%100) < below {
			return id
		}
	}
	t.Fatalf("no connection id with hash%%100 < %d found", below)
	return ""
}

// findConnIDWithoutHash 找一个 FNV1a32%100 >= atLeast 的连接 ID。
func findConnIDWithoutHash(t *testing.T, atLeast int) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := "conn-" + strconv.Itoa(i)
		if int(uhash.FNV1a32(id)%100) >= atLeast {
			return id
		}
	}
	t.Fatalf("no connection id with hash%%100 >= %d found", atLeast)
	return ""
}

func newFC(connID, user string) *types.FilterContext {
	return &types.FilterContext{
		Ctx:          context.Background(),
		ConnectionID: connID,
		UserUUID:     user,
		Metadata:     map[string]string{},
	}
}

func TestCanaryFilter_Metadata(t *testing.T) {
	f := NewCanaryFilter(config.CanaryConfig{})
	if f.Name() != "canary" {
		t.Errorf("Name() = %q, want canary", f.Name())
	}
	if f.Phase() != types.PhasePostAuth {
		t.Errorf("Phase() = %v, want PhasePostAuth", f.Phase())
	}
	if f.Priority() != 200 {
		t.Errorf("Priority() = %d, want 200", f.Priority())
	}
}

func TestCanaryFilter_Process_PercentBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		percent int
		wantHit bool // 对任意 connectionID 的命中预期
	}{
		{name: "percent 0 never hits", percent: 0, wantHit: false},
		{name: "percent 100 always hits", percent: 100, wantHit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewCanaryFilter(config.CanaryConfig{Percent: tt.percent})
			for _, connID := range []string{"conn-0", "conn-1", "abc", "zzz-999"} {
				fc := newFC(connID, "")
				ok, err := f.Process(fc)
				if err != nil {
					t.Fatalf("Process(%s) error = %v", connID, err)
				}
				if !ok {
					t.Fatalf("Process(%s) = false, want true (canary never aborts)", connID)
				}
				got := fc.Metadata["canary"] == "true"
				if got != tt.wantHit {
					t.Errorf("Process(%s) hit = %v, want %v (percent=%d)", connID, got, tt.wantHit, tt.percent)
				}
			}
		})
	}
}

func TestCanaryFilter_Process_PercentDeterministicSelection(t *testing.T) {
	const percent = 50
	f := NewCanaryFilter(config.CanaryConfig{Percent: percent})

	hitID := findConnIDWithHash(t, percent)
	missID := findConnIDWithoutHash(t, percent)

	// 命中侧：同一连接多次处理结果稳定且命中
	for i := 0; i < 3; i++ {
		fc := newFC(hitID, "")
		if _, err := f.Process(fc); err != nil {
			t.Fatalf("Process error = %v", err)
		}
		if fc.Metadata["canary"] != "true" {
			t.Fatalf("connection %s (hash=%d) not hit on iteration %d", hitID, uhash.FNV1a32(hitID)%100, i)
		}
	}
	// 未命中侧：多次处理均不命中
	for i := 0; i < 3; i++ {
		fc := newFC(missID, "")
		if _, err := f.Process(fc); err != nil {
			t.Fatalf("Process error = %v", err)
		}
		if fc.Metadata["canary"] == "true" {
			t.Fatalf("connection %s (hash=%d) unexpectedly hit on iteration %d", missID, uhash.FNV1a32(missID)%100, i)
		}
	}
	if got := f.GetHitCount(); got != 3 {
		t.Errorf("GetHitCount() = %d, want 3", got)
	}
}

func TestCanaryFilter_Process_UserID(t *testing.T) {
	f := NewCanaryFilter(config.CanaryConfig{UserIDs: []string{"u-1", "u-2"}})

	fc := newFC("any-conn", "u-2")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] != "true" {
		t.Error("listed user not hit")
	}

	fc = newFC("any-conn", "u-other")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] == "true" {
		t.Error("unlisted user unexpectedly hit")
	}
}

func TestCanaryFilter_Process_HeaderMatch(t *testing.T) {
	f := NewCanaryFilter(config.CanaryConfig{
		Headers: map[string]string{"x-canary": "on", "x-env": "gray"},
	})

	fc := newFC("conn-x", "")
	fc.Metadata["x-env"] = "gray"
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] != "true" {
		t.Error("header match not hit")
	}

	fc = newFC("conn-x", "")
	fc.Metadata["x-env"] = "prod"
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] == "true" {
		t.Error("non-matching header values unexpectedly hit")
	}
}

func TestCanaryFilter_Process_TargetRouteRewrite(t *testing.T) {
	f := NewCanaryFilter(config.CanaryConfig{UserIDs: []string{"u-hit"}, TargetRoute: "route-gray"})
	fc := newFC("conn", "u-hit")
	fc.Route = "route-main"
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Route != "route-gray" {
		t.Errorf("Route = %q, want %q", fc.Route, "route-gray")
	}

	// 未命中时路由不变
	fc = newFC("conn", "u-miss")
	fc.Route = "route-main"
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Route != "route-main" {
		t.Errorf("Route = %q, want unchanged %q", fc.Route, "route-main")
	}
}

func TestCanaryFilter_UserBeatsPercent(t *testing.T) {
	// 用户命中时即使 percent=0 也应灰度
	f := NewCanaryFilter(config.CanaryConfig{Percent: 0, UserIDs: []string{"u-1"}})
	fc := newFC("conn-0", "u-1")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] != "true" {
		t.Error("user-based canary ignored percent=0")
	}
}

func TestCanaryFilter_UpdateConfig(t *testing.T) {
	f := NewCanaryFilter(config.CanaryConfig{Percent: 100})
	hitID := "conn-anywhere"

	fc := newFC(hitID, "")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] != "true" {
		t.Fatal("percent=100 should hit")
	}

	// 热更到 percent=0 + 指定用户
	f.UpdateConfig(config.CanaryConfig{Percent: 0, UserIDs: []string{"u-new"}, TargetRoute: "gray"})

	fc = newFC(hitID, "")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] == "true" {
		t.Error("after UpdateConfig(percent=0) generic connection should not hit")
	}

	fc = newFC(hitID, "u-new")
	if _, err := f.Process(fc); err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if fc.Metadata["canary"] != "true" {
		t.Error("after UpdateConfig user list should hit")
	}
	if fc.Route != "gray" {
		t.Errorf("Route = %q, want %q after UpdateConfig", fc.Route, "gray")
	}
	if got := f.GetHitCount(); got != 2 {
		t.Errorf("GetHitCount() = %d, want 2", got)
	}
}

func TestCanaryFilter_NilMetadataHitPanicsGuard(t *testing.T) {
	// 命中路径要求 fc.Metadata 非 nil（写入 canary 标记）；
	// 未命中路径不触碰 Metadata，nil map 也应安全。
	f := NewCanaryFilter(config.CanaryConfig{Percent: 0})
	fc := &types.FilterContext{Ctx: context.Background(), ConnectionID: "conn-0"}
	ok, err := f.Process(fc)
	if err != nil || !ok {
		t.Fatalf("Process(nil metadata, no hit) = (%v, %v), want (true, nil)", ok, err)
	}
}
