// wazero WASM 运行时实现：纯 Go，无 CGO，跨平台
// 依赖：go get github.com/tetratelabs/wazero
//
// 加载示例：
//
//	wasm-runtime:
//	  modules:
//	    - name: "auth-plugin"
//	      path: "./plugins/auth.wasm"
//	      function: "process"
//
// 插件 WAT 示例（process(data_ptr, data_len) -> (out_ptr, out_len, status)）：
//
//	(module
//	  (memory (export "mem") 1)
//	  (func (export "process") (param i32 i32) (result i32 i32 i32) ...)
//	)
package traffic

import (
	"context"
	"fmt"
	"os"
	"sync"

	tlog "github.com/streasure/treasure-slog"
	waz "github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// WazeroRuntime 基于 wazero 的 WASM 运行时实现
type WazeroRuntime struct {
	ctx     context.Context
	runtime waz.Runtime
	modules sync.Map // name -> *moduleEntry
}

type moduleEntry struct {
	module  waz.CompiledModule
	memory  api.Memory
	process api.Function
}

// NewWazeroRuntime 创建运行时
func NewWazeroRuntime() *WazeroRuntime {
	ctx := context.Background()
	r := waz.NewRuntime(ctx)
	return &WazeroRuntime{ctx: ctx, runtime: r}
}

func (w *WazeroRuntime) Type() string { return "wazero" }

// LoadModule 加载 .wasm 字节码
func (w *WazeroRuntime) LoadModule(name string, bytes []byte) error {
	// 从文件读取
	if len(bytes) == 0 {
		return fmt.Errorf("empty wasm bytes for module %s", name)
	}
	compiled, err := w.runtime.CompileModule(w.ctx, bytes)
	if err != nil {
		return fmt.Errorf("compile module %s: %w", name, err)
	}
	// 实例化
	mod, err := w.runtime.InstantiateModule(w.ctx, compiled, waz.NewModuleConfig())
	if err != nil {
		return fmt.Errorf("instantiate module %s: %w", name, err)
	}
	mem := mod.Memory()
	proc := mod.ExportedFunction("process")
	if proc == nil {
		return fmt.Errorf("module %s missing 'process' export", name)
	}
	w.modules.Store(name, &moduleEntry{module: compiled, memory: mem, process: proc})
	tlog.Info("wasm module loaded", "name", name, "type", "wazero")
	return nil
}

// LoadModuleFromFile 从文件加载
func (w *WazeroRuntime) LoadModuleFromFile(name, path string) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return w.LoadModule(name, bytes)
}

// Invoke 调用 process(data_ptr, data_len) -> (out_ptr, out_len, status)
func (w *WazeroRuntime) Invoke(name, funcName string, input []byte) ([]byte, int, error) {
	v, ok := w.modules.Load(name)
	if !ok {
		return nil, -1, fmt.Errorf("module not loaded: %s", name)
	}
	entry := v.(*moduleEntry)
	// 把 input 写入 WASM 内存（offset=0；wazero 1.8 Write 返回 bool）
	if !entry.memory.Write(0, input) {
		return nil, -1, fmt.Errorf("wasm memory write failed (len=%d)", len(input))
	}
	// 调用 process(0, len) — 偏移固定 0，长度 len(input)
	results, err := entry.process.Call(w.ctx, uint64(0), uint64(len(input)))
	if err != nil {
		return nil, -1, fmt.Errorf("wasm invoke: %w", err)
	}
	if len(results) < 3 {
		return nil, -1, fmt.Errorf("wasm process returned %d values, expected 3", len(results))
	}
	outPtr := int32(results[0])
	outLen := int32(results[1])
	status := int(results[2])
	if outLen <= 0 {
		return nil, status, nil
	}
	out, ok := entry.memory.Read(uint32(outPtr), uint32(outLen))
	if !ok {
		return nil, -1, fmt.Errorf("wasm memory read out failed")
	}
	return append([]byte(nil), out...), status, nil
}

// UnloadModule 卸载模块
func (w *WazeroRuntime) UnloadModule(name string) error {
	if v, ok := w.modules.LoadAndDelete(name); ok {
		entry := v.(*moduleEntry)
		return entry.module.Close(w.ctx)
	}
	return fmt.Errorf("module not found: %s", name)
}

func init() {
	SetWasmRuntime(NewWazeroRuntime())
	tlog.Info("wasm runtime initialized (wazero)")
}
