package traffic

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/streasure/sgate/internal/types"
)

// wasmFromHex 解析十六进制为 wasm 二进制（测试内嵌最小模块，避免依赖 wat2wasm）。
func wasmFromHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// wasmOKModule 最小合法模块：导出 process(i32,i32)->(i32,i32,i32)，返回 (0,0,0)。
var wasmOKModule = `
0061736d 01000000
0109 01 60 02 7f 7f 03 7f 7f 7f
0302 01 00
0503 01 00 01
070b 01 07 70726f63657373 00 00
0a0a 01 08 00 4100 4100 4100 0b`

// wasmTrapModule 导出 process 但函数体 unreachable：调用必然 trap。
var wasmTrapModule = `
0061736d 01000000
0109 01 60 02 7f 7f 03 7f 7f 7f
0302 01 00
0503 01 00 01
070b 01 07 70726f63657373 00 00
0a05 01 03 00 00 0b`

// wasmNoExportModule 合法但无任何导出。
var wasmNoExportModule = `
0061736d 01000000
0109 01 60 02 7f 7f 03 7f 7f 7f
0302 01 00
0503 01 00 01
0a0a 01 08 00 4100 4100 4100 0b`

// stubWasmRuntime 可脚本化的 WasmRuntime 桩。
type stubWasmRuntime struct {
	invokeFn func(moduleName, funcName string, input []byte) ([]byte, int, error)
	loaded   map[string][]byte
}

func (s *stubWasmRuntime) LoadModule(name string, b []byte) error {
	if s.loaded == nil {
		s.loaded = map[string][]byte{}
	}
	s.loaded[name] = b
	return nil
}
func (s *stubWasmRuntime) Invoke(moduleName, funcName string, input []byte) ([]byte, int, error) {
	if s.invokeFn != nil {
		return s.invokeFn(moduleName, funcName, input)
	}
	return nil, 0, nil
}
func (s *stubWasmRuntime) UnloadModule(name string) error {
	if _, ok := s.loaded[name]; !ok {
		return errors.New("module not loaded: " + name)
	}
	delete(s.loaded, name)
	return nil
}
func (s *stubWasmRuntime) Type() string { return "stub" }

func TestGetWasmRuntime_WazeroInjected(t *testing.T) {
	rt := GetWasmRuntime()
	if rt == nil {
		t.Fatal("GetWasmRuntime() = nil, want wazero runtime (init always injects)")
	}
	if rt.Type() != "wazero" {
		t.Errorf("runtime Type() = %q, want wazero", rt.Type())
	}
}

func TestSetGetWasmRuntime_SwapAndRestore(t *testing.T) {
	old := GetWasmRuntime()
	defer SetWasmRuntime(old)

	stub := &stubWasmRuntime{}
	SetWasmRuntime(stub)
	if got := GetWasmRuntime(); got != stub {
		t.Errorf("GetWasmRuntime() = %v, want injected stub", got)
	}

	// 注入 nil 后视为未启用
	SetWasmRuntime(nil)
	if got := GetWasmRuntime(); got != nil {
		t.Errorf("GetWasmRuntime() = %v after SetWasmRuntime(nil), want nil", got)
	}
	_, err := NewWasmFilter("m", "process")
	if !errors.Is(err, ErrWasmRuntimeNotEnabled) {
		t.Errorf("NewWasmFilter without runtime error = %v, want ErrWasmRuntimeNotEnabled", err)
	}
}

func TestNewWasmFilter(t *testing.T) {
	old := GetWasmRuntime()
	defer SetWasmRuntime(old)
	SetWasmRuntime(&stubWasmRuntime{})

	w, err := NewWasmFilter("mod", "process")
	if err != nil {
		t.Fatalf("NewWasmFilter error = %v", err)
	}
	if w.Name() != "wasm-filter" {
		t.Errorf("Name() = %q, want wasm-filter", w.Name())
	}
	if w.Phase() != types.PhasePostAuth {
		t.Errorf("Phase() = %v, want PhasePostAuth", w.Phase())
	}
	if w.Priority() != 250 {
		t.Errorf("Priority() = %d, want 250", w.Priority())
	}
}

func TestWasmFilter_Process_Branches(t *testing.T) {
	tests := []struct {
		name         string
		invoke       func(input []byte) ([]byte, int, error)
		wantOK       bool
		wantContains string // DropReason 子串；空表示不校验
		wantRoute    string
		wantNote     string
	}{
		{
			name:         "invoke error aborts",
			invoke:       func([]byte) ([]byte, int, error) { return nil, 0, errors.New("invoke boom") },
			wantOK:       false,
			wantContains: "wasm invoke failed: invoke boom",
		},
		{
			name:         "nonzero status aborts",
			invoke:       func([]byte) ([]byte, int, error) { return []byte("x"), 3, nil },
			wantOK:       false,
			wantContains: "wasm plugin rejected",
		},
		{
			name:   "zero status empty output passes",
			invoke: func([]byte) ([]byte, int, error) { return nil, 0, nil },
			wantOK: true,
		},
		{
			name: "zero status with route rewrite",
			invoke: func([]byte) ([]byte, int, error) {
				// tag 'r' 长度 5 "route" + tag 'm' 长度 2 "hi"
				out := []byte{'r', 0, 5}
				out = append(out, "route"...)
				out = append(out, 'm', 0, 2)
				out = append(out, "hi"...)
				return out, 0, nil
			},
			wantOK:    true,
			wantRoute: "route",
			wantNote:  "hi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubWasmRuntime{invokeFn: func(m, f string, input []byte) ([]byte, int, error) {
				if m != "mod" || f != "process" {
					t.Errorf("Invoke(%q, %q), want (mod, process)", m, f)
				}
				return tt.invoke(input)
			}}
			old := GetWasmRuntime()
			defer SetWasmRuntime(old)
			SetWasmRuntime(stub)

			w, err := NewWasmFilter("mod", "process")
			if err != nil {
				t.Fatalf("NewWasmFilter error = %v", err)
			}
			fc := &types.FilterContext{
				Ctx:          t.Context(),
				ConnectionID: "c1",
				UserUUID:     "u1",
				Route:        "orig",
				Data:         []byte{9, 9},
				Metadata:     map[string]string{},
			}
			ok, err := w.Process(fc)
			if err != nil {
				t.Fatalf("Process error = %v", err)
			}
			if ok != tt.wantOK {
				t.Errorf("Process = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantContains != "" && !strings.Contains(fc.DropReason, tt.wantContains) {
				t.Errorf("DropReason = %q, want contains %q", fc.DropReason, tt.wantContains)
			}
			if tt.wantRoute != "" && fc.Route != tt.wantRoute {
				t.Errorf("Route = %q, want %q (plugin rewrite)", fc.Route, tt.wantRoute)
			}
			if tt.wantNote != "" && fc.Metadata["wasm.note"] != tt.wantNote {
				t.Errorf("Metadata[wasm.note] = %q, want %q", fc.Metadata["wasm.note"], tt.wantNote)
			}
		})
	}
}

func TestWasmFilter_Process_CapturesInput(t *testing.T) {
	var captured []byte
	stub := &stubWasmRuntime{invokeFn: func(m, f string, input []byte) ([]byte, int, error) {
		captured = append([]byte(nil), input...)
		return nil, 0, nil
	}}
	old := GetWasmRuntime()
	defer SetWasmRuntime(old)
	SetWasmRuntime(stub)

	w, _ := NewWasmFilter("mod", "process")
	fc := &types.FilterContext{
		Ctx:          t.Context(),
		ConnectionID: "c1",
		UserUUID:     "u1",
		RemoteIP:     "10.0.0.1",
		Route:        "login",
		Data:         []byte{1, 2, 3},
		Metadata:     map[string]string{},
	}
	if ok, err := w.Process(fc); err != nil || !ok {
		t.Fatalf("Process = (%v, %v)", ok, err)
	}

	// 手工解析 TLV 输入：r/c/u/i 为 2 字节长度前缀，d 为 4 字节长度前缀
	parse := func(b []byte) map[byte]string {
		out := map[byte]string{}
		pos := 0
		for pos < len(b) {
			tag := b[pos]
			pos++
			if tag == 'd' {
				if pos+4 > len(b) {
					t.Fatalf("truncated data length at %d", pos)
				}
				l := int(b[pos])<<24 | int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
				pos += 4
				out[tag] = string(b[pos : pos+l])
				pos += l
				continue
			}
			if pos+2 > len(b) {
				t.Fatalf("truncated length at %d", pos)
			}
			l := int(b[pos])<<8 | int(b[pos+1])
			pos += 2
			out[tag] = string(b[pos : pos+l])
			pos += l
		}
		return out
	}
	got := parse(captured)
	if got['r'] != "login" || got['c'] != "c1" || got['u'] != "u1" || got['i'] != "10.0.0.1" || got['d'] != "\x01\x02\x03" {
		t.Errorf("TLV input mismatch: %q", got)
	}
}

func TestApplyWasmOutput(t *testing.T) {
	tests := []struct {
		name     string
		out      []byte
		wantR    string
		wantU    string
		wantNote string
	}{
		{name: "empty output noop", out: nil},
		{name: "short output noop", out: []byte{1, 2}},
		{name: "route rewrite", out: append([]byte{'r', 0, 3}, "abc"...), wantR: "abc"},
		{name: "user rewrite", out: append([]byte{'u', 0, 2}, "u9"...), wantU: "u9"},
		{name: "metadata note", out: append([]byte{'m', 0, 2}, "n1"...), wantNote: "n1"},
		{
			name: "multiple fields",
			out: func() []byte {
				b := []byte{'r', 0, 1, 'x'}
				b = append(b, 'u', 0, 1, 'y')
				b = append(b, 'm', 0, 1, 'z')
				return b
			}(),
			wantR: "x", wantU: "y", wantNote: "z",
		},
		{name: "unknown tag skipped, later fields applied", out: append([]byte{'z', 0, 1, 'q'}, 'r', 0, 1, 'k'), wantR: "k"},
		{name: "truncated length header", out: []byte{'r', 0}},
		{name: "value overflows buffer", out: []byte{'r', 0, 99, 'a'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &types.FilterContext{Route: "orig", UserUUID: "origU", Metadata: map[string]string{}}
			applyWasmOutput(fc, tt.out) // 不得 panic
			if tt.wantR != "" && fc.Route != tt.wantR {
				t.Errorf("Route = %q, want %q", fc.Route, tt.wantR)
			}
			if tt.wantU != "" && fc.UserUUID != tt.wantU {
				t.Errorf("UserUUID = %q, want %q", fc.UserUUID, tt.wantU)
			}
			if tt.wantNote != "" && fc.Metadata["wasm.note"] != tt.wantNote {
				t.Errorf("Metadata[wasm.note] = %q, want %q", fc.Metadata["wasm.note"], tt.wantNote)
			}
			if tt.wantR == "" && tt.wantU == "" && tt.wantNote == "" {
				if fc.Route != "orig" || fc.UserUUID != "origU" || len(fc.Metadata) != 0 {
					t.Errorf("context mutated by noop output: %+v", fc)
				}
			}
		})
	}
}

func TestWazeroRuntime_LoadModuleErrors(t *testing.T) {
	rt := NewWazeroRuntime()

	if err := rt.LoadModule("empty", nil); err == nil || !strings.Contains(err.Error(), "empty wasm bytes") {
		t.Errorf("LoadModule(nil bytes) error = %v, want empty wasm bytes", err)
	}
	if err := rt.LoadModule("bad", []byte{0xde, 0xad}); err == nil || !strings.Contains(err.Error(), "compile module") {
		t.Errorf("LoadModule(invalid) error = %v, want compile module failure", err)
	}
	if err := rt.LoadModule("noexport", wasmFromHex(t, wasmNoExportModule)); err == nil ||
		!strings.Contains(err.Error(), "missing 'process' export") {
		t.Errorf("LoadModule(no process export) error = %v, want missing export", err)
	}
	if err := rt.LoadModuleFromFile("x", filepath.Join(t.TempDir(), "missing.wasm")); err == nil {
		t.Error("LoadModuleFromFile(missing path) error = nil, want os error")
	}
}

func TestWazeroRuntime_LoadInvokeUnload(t *testing.T) {
	rt := NewWazeroRuntime()

	if _, _, err := rt.Invoke("ghost", "process", nil); err == nil || !strings.Contains(err.Error(), "module not loaded") {
		t.Errorf("Invoke(unloaded) error = %v, want module not loaded", err)
	}

	if err := rt.LoadModule("ok", wasmFromHex(t, wasmOKModule)); err != nil {
		t.Fatalf("LoadModule(ok) error = %v", err)
	}

	out, status, err := rt.Invoke("ok", "process", []byte("hello"))
	if err != nil {
		t.Fatalf("Invoke(ok) error = %v", err)
	}
	if status != 0 {
		t.Errorf("status = %d, want 0", status)
	}
	if len(out) != 0 {
		t.Errorf("out = %v, want empty (module returns outLen=0)", out)
	}

	// 超出模块内存（1 页 = 64KB）的写入失败
	big := make([]byte, 70*1024)
	if _, _, err := rt.Invoke("ok", "process", big); err == nil || !strings.Contains(err.Error(), "memory write failed") {
		t.Errorf("Invoke(oversized input) error = %v, want memory write failure", err)
	}

	if err := rt.UnloadModule("ok"); err != nil {
		t.Fatalf("UnloadModule(ok) error = %v", err)
	}
	if err := rt.UnloadModule("ok"); err == nil || !strings.Contains(err.Error(), "module not found") {
		t.Errorf("UnloadModule(twice) error = %v, want module not found", err)
	}
}

func TestWazeroRuntime_InvokeTrap(t *testing.T) {
	rt := NewWazeroRuntime()
	if err := rt.LoadModule("trap", wasmFromHex(t, wasmTrapModule)); err != nil {
		t.Fatalf("LoadModule(trap) error = %v", err)
	}
	_, status, err := rt.Invoke("trap", "process", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "wasm invoke") {
		t.Errorf("Invoke(trap) error = %v, want wasm invoke failure", err)
	}
	if status != -1 {
		t.Errorf("status = %d, want -1 on trap", status)
	}
}

func TestWazeroRuntime_LoadModuleFromFile(t *testing.T) {
	rt := NewWazeroRuntime()
	path := filepath.Join(t.TempDir(), "ok.wasm")
	if err := os.WriteFile(path, wasmFromHex(t, wasmOKModule), 0o600); err != nil {
		t.Fatalf("write wasm fixture: %v", err)
	}
	if err := rt.LoadModuleFromFile("filemod", path); err != nil {
		t.Fatalf("LoadModuleFromFile error = %v", err)
	}
	if _, _, err := rt.Invoke("filemod", "process", nil); err != nil {
		t.Errorf("Invoke after file load error = %v", err)
	}
	if err := rt.UnloadModule("filemod"); err != nil {
		t.Errorf("UnloadModule error = %v", err)
	}
}

func TestWasmFilter_WithRealWazeroRuntime(t *testing.T) {
	rt := GetWasmRuntime()
	if rt == nil {
		t.Skip("wasm runtime not available")
	}
	if err := rt.LoadModule("t-real", wasmFromHex(t, wasmOKModule)); err != nil {
		t.Fatalf("LoadModule error = %v", err)
	}
	defer func() { _ = rt.UnloadModule("t-real") }()

	w, err := NewWasmFilter("t-real", "process")
	if err != nil {
		t.Fatalf("NewWasmFilter error = %v", err)
	}
	fc := &types.FilterContext{Ctx: t.Context(), ConnectionID: "c", Metadata: map[string]string{}}
	ok, err := w.Process(fc)
	if err != nil {
		t.Fatalf("Process error = %v", err)
	}
	if !ok {
		t.Error("Process = false, want true (status 0)")
	}
	if fc.DropReason != "" {
		t.Errorf("DropReason = %q, want empty", fc.DropReason)
	}
}
