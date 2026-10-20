package types

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockFilter 可配置的测试过滤器，记录调用次数。
type mockFilter struct {
	name     string
	phase    FilterPhase
	priority int
	proc     func(fc *FilterContext) (bool, error)
	calls    int
}

func (m *mockFilter) Name() string       { return m.name }
func (m *mockFilter) Phase() FilterPhase { return m.phase }
func (m *mockFilter) Priority() int      { return m.priority }
func (m *mockFilter) Process(fc *FilterContext) (bool, error) {
	m.calls++
	if m.proc != nil {
		return m.proc(fc)
	}
	return true, nil
}

func TestNewFilterChain_Empty(t *testing.T) {
	fc := NewFilterChain()
	if got := fc.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestFilterChain_RegisterAndLoadByName(t *testing.T) {
	fc := NewFilterChain()
	fc.Register("mock", func(cfg map[string]any) (Filter, error) {
		return &mockFilter{name: "mock", phase: PhasePreAuth}, nil
	})

	if err := fc.LoadByName("mock", nil); err != nil {
		t.Fatalf("LoadByName(mock) error = %v", err)
	}
	if got := fc.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}

func TestFilterChain_LoadByName_Unknown(t *testing.T) {
	fc := NewFilterChain()
	err := fc.LoadByName("no-such-filter", nil)
	if err == nil {
		t.Fatal("LoadByName(unknown) error = nil, want ErrFilterNotFound")
	}
	var notFound ErrFilterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("error type = %T (%v), want ErrFilterNotFound", err, err)
	}
	if notFound.Name != "no-such-filter" {
		t.Errorf("ErrFilterNotFound.Name = %q, want %q", notFound.Name, "no-such-filter")
	}
	if !strings.Contains(err.Error(), "no-such-filter") {
		t.Errorf("Error() = %q, want it to contain filter name", err.Error())
	}
	if got := fc.Len(); got != 0 {
		t.Errorf("Len() = %d after failed load, want 0", got)
	}
}

func TestFilterChain_LoadByName_FactoryError(t *testing.T) {
	fc := NewFilterChain()
	wantErr := errors.New("bad config")
	fc.Register("boom", func(cfg map[string]any) (Filter, error) {
		return nil, wantErr
	})

	err := fc.LoadByName("boom", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("LoadByName(boom) error = %v, want %v", err, wantErr)
	}
	if got := fc.Len(); got != 0 {
		t.Errorf("Len() = %d after factory error, want 0", got)
	}
}

func TestFilterChain_AddFilter_SortsByPriority(t *testing.T) {
	fc := NewFilterChain()
	fc.AddFilter(&mockFilter{name: "c", phase: PhasePostAuth, priority: 300})
	fc.AddFilter(&mockFilter{name: "a", phase: PhasePreAuth, priority: 100})
	fc.AddFilter(&mockFilter{name: "b", phase: PhaseAuth, priority: 200})
	fc.AddFilter(&mockFilter{name: "a2", phase: PhasePreAuth, priority: 100}) // 同优先级保持插入序（stable sort）

	if got := fc.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	wantOrder := []string{"a", "a2", "b", "c"}
	for i, want := range wantOrder {
		if got := fc.filters[i].Name(); got != want {
			t.Errorf("filters[%d].Name() = %q, want %q", i, got, want)
		}
	}
}

func TestFilterChain_RunByPhase_OnlySamePhase(t *testing.T) {
	fc := NewFilterChain()
	pre := &mockFilter{name: "pre", phase: PhasePreAuth}
	post := &mockFilter{name: "post", phase: PhasePostAuth}
	fc.AddFilter(pre)
	fc.AddFilter(post)

	fcx := &FilterContext{Ctx: context.Background()}
	if ok := fc.RunByPhase(PhaseAuth, fcx); !ok {
		t.Fatal("RunByPhase(PhaseAuth) = false, want true")
	}
	if pre.calls != 0 || post.calls != 0 {
		t.Errorf("filters called on empty phase: pre=%d post=%d, want 0/0", pre.calls, post.calls)
	}

	if ok := fc.RunByPhase(PhasePostAuth, fcx); !ok {
		t.Fatal("RunByPhase(PhasePostAuth) = false, want true")
	}
	if post.calls != 1 {
		t.Errorf("post.calls = %d, want 1", post.calls)
	}
	if pre.calls != 0 {
		t.Errorf("pre.calls = %d, want 0 (different phase)", pre.calls)
	}
}

func TestFilterChain_RunByPhase_ShortCircuitOnFalse(t *testing.T) {
	fc := NewFilterChain()
	first := &mockFilter{name: "first", phase: PhaseForward, proc: func(fc *FilterContext) (bool, error) {
		fc.DropReason = "first rejected"
		return false, nil
	}}
	second := &mockFilter{name: "second", phase: PhaseForward}
	fc.AddFilter(first)
	fc.AddFilter(second)

	fcx := &FilterContext{Ctx: context.Background()}
	if ok := fc.RunByPhase(PhaseForward, fcx); ok {
		t.Fatal("RunByPhase = true, want false (filter returned continue=false)")
	}
	if !fcx.Abort {
		t.Error("fcx.Abort = false, want true")
	}
	if fcx.DropReason != "first rejected" {
		t.Errorf("DropReason = %q, want %q", fcx.DropReason, "first rejected")
	}
	if second.calls != 0 {
		t.Errorf("second.calls = %d, want 0 (chain short-circuited)", second.calls)
	}
}

func TestFilterChain_RunByPhase_FailClosedOnError(t *testing.T) {
	fc := NewFilterChain()
	failing := &mockFilter{name: "failing", phase: PhaseAuth, proc: func(fc *FilterContext) (bool, error) {
		return true, errors.New("verify failed")
	}}
	after := &mockFilter{name: "after", phase: PhaseAuth}
	fc.AddFilter(failing)
	fc.AddFilter(after)

	fcx := &FilterContext{Ctx: context.Background()}
	if ok := fc.RunByPhase(PhaseAuth, fcx); ok {
		t.Fatal("RunByPhase = true, want false (filter returned error, fail-closed)")
	}
	if !fcx.Abort {
		t.Error("fcx.Abort = false, want true")
	}
	if want := "failing: verify failed"; fcx.DropReason != want {
		t.Errorf("DropReason = %q, want %q", fcx.DropReason, want)
	}
	if after.calls != 0 {
		t.Errorf("after.calls = %d, want 0 (chain aborted on error)", after.calls)
	}
}

func TestFilterChain_RunByPhase_ErrorKeepsExistingDropReason(t *testing.T) {
	fc := NewFilterChain()
	failing := &mockFilter{name: "failing", phase: PhaseAuth, proc: func(fc *FilterContext) (bool, error) {
		return true, errors.New("boom")
	}}
	fc.AddFilter(failing)

	fcx := &FilterContext{Ctx: context.Background(), DropReason: "earlier reason"}
	if ok := fc.RunByPhase(PhaseAuth, fcx); ok {
		t.Fatal("RunByPhase = true, want false")
	}
	if fcx.DropReason != "earlier reason" {
		t.Errorf("DropReason = %q, want preserved %q", fcx.DropReason, "earlier reason")
	}
}

func TestFilterChain_RunByPhase_DisabledSkipsFilters(t *testing.T) {
	fc := NewFilterChain()
	f := &mockFilter{name: "m", phase: PhasePreAuth}
	fc.AddFilter(f)
	fc.enabled.Store(0) // 包内直接操作：无导出禁用入口

	fcx := &FilterContext{Ctx: context.Background()}
	if ok := fc.RunByPhase(PhasePreAuth, fcx); !ok {
		t.Fatal("RunByPhase on disabled chain = false, want true")
	}
	if f.calls != 0 {
		t.Errorf("filter calls = %d on disabled chain, want 0", f.calls)
	}
}

func TestInitGetFilterChain_Global(t *testing.T) {
	chain := InitFilterChain()
	if chain == nil {
		t.Fatal("InitFilterChain() = nil")
	}
	if got := GetFilterChain(); got != chain {
		t.Errorf("GetFilterChain() = %p, want %p (same as InitFilterChain)", got, chain)
	}
	if got := chain.Len(); got != 0 {
		t.Errorf("fresh global chain Len() = %d, want 0", got)
	}
}

func TestRegisterFilter_GlobalRegistryVisibleToNewChains(t *testing.T) {
	RegisterFilter("test-global-mock", func(cfg map[string]any) (Filter, error) {
		return &mockFilter{name: "test-global-mock", phase: PhasePostAuth, priority: 10}, nil
	})
	fc := NewFilterChain() // 构造时快照全局注册表
	if err := fc.LoadByName("test-global-mock", nil); err != nil {
		t.Fatalf("LoadByName(test-global-mock) error = %v", err)
	}
	if got := fc.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
	if got := fc.filters[0].Priority(); got != 10 {
		t.Errorf("loaded filter Priority() = %d, want 10", got)
	}
}
