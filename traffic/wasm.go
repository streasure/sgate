package traffic

import (
	"fmt"
	"sync/atomic"

	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/util"
)

// WasmRuntime WebAssembly 插件运行时抽象
// 用于在沙箱中加载/执行动态插件，提供接近原生的性能 + 安全隔离
//
// 实现：
//   - 默认 stub（wasm_runtime_stub.go）：不引入第三方依赖，直接拒绝加载
//   - wazero 实现（wasm_runtime_wazero.go，build tag `wasm`）：纯 Go WASM 运行时
//
// 启用：go build -tags wasm
type WasmRuntime interface {
	// LoadModule 加载 .wasm 字节码到运行时
	// moduleName 模块名，bytes wasm 二进制
	LoadModule(moduleName string, bytes []byte) error
	// Invoke 调用模块内指定函数，传递输入字节，返回输出字节
	// 函数签名约定：(data_ptr, data_len) -> (out_ptr, out_len, status_code)
	Invoke(moduleName, funcName string, input []byte) ([]byte, int, error)
	// UnloadModule 卸载模块
	UnloadModule(moduleName string) error
	// Type 返回运行时类型
	Type() string
}

// wasmRuntimeSingleton 全局运行时实例（build tag 控制实际实现）
var wasmRuntimeSingleton atomic.Pointer[WasmRuntime]

// SetWasmRuntime 注入运行时实现（一般由 init() 调用）
func SetWasmRuntime(rt WasmRuntime) {
	wasmRuntimeSingleton.Store(&rt)
}

// GetWasmRuntime 获取当前运行时（nil 表示未启用）
func GetWasmRuntime() WasmRuntime {
	p := wasmRuntimeSingleton.Load()
	if p == nil {
		return nil
	}
	return *p
}

// WasmFilter WebAssembly 插件过滤器
// SPI 注册名: "wasm-filter"
// 通过 wasm-filter.yaml 配置 module 路径 + 调用入口
type WasmFilter struct {
	moduleName string
	funcName   string
	runtime    WasmRuntime
}

// NewWasmFilter 创建 WASM 过滤器（前提：运行时已注入）
func NewWasmFilter(moduleName, funcName string) (*WasmFilter, error) {
	rt := GetWasmRuntime()
	if rt == nil {
		return nil, ErrWasmRuntimeNotEnabled
	}
	return &WasmFilter{
		moduleName: moduleName,
		funcName:   funcName,
		runtime:    rt,
	}, nil
}

func (w *WasmFilter) Name() string       { return "wasm-filter" }
func (w *WasmFilter) Phase() types.FilterPhase { return types.PhasePostAuth }
func (w *WasmFilter) Priority() int      { return 250 }

// Process 把请求交给 WASM 插件处理
// 返回码：0 = 放行；非 0 = 中止
func (w *WasmFilter) Process(fc *types.FilterContext) (bool, error) {
	// 构造输入：route|conn|user|data
	input := buildWasmInput(fc)
	out, code, err := w.runtime.Invoke(w.moduleName, w.funcName, input)
	if err != nil {
		fc.DropReason = "wasm invoke failed: " + err.Error()
		return false, nil
	}
	if code != 0 {
		fc.DropReason = "wasm plugin rejected"
		_ = out
		return false, nil
	}
	// 允许插件回写 data（如改写路由）
	if len(out) > 0 {
		applyWasmOutput(fc, out)
	}
	return true, nil
}

func buildWasmInput(fc *types.FilterContext) []byte {
	// 简单 TLV 编码：route + conn + user + data
	// 实际 wasm 插件按相同协议解析
	var buf []byte
	writeWasmField := func(tag byte, s string) {
		buf = append(buf, tag)
		buf = append(buf, byte(len(s)>>8), byte(len(s)))
		buf = append(buf, s...)
	}
	writeWasmField('r', fc.Route)
	writeWasmField('c', fc.ConnectionID)
	writeWasmField('u', fc.UserUUID)
	writeWasmField('i', fc.RemoteIP)
	// data 字段直接追加（避免双重长度前缀）
	buf = append(buf, 'd')
	dl := len(fc.Data)
	buf = append(buf, byte(dl>>24), byte(dl>>16), byte(dl>>8), byte(dl))
	buf = append(buf, fc.Data...)
	return buf
}

func applyWasmOutput(fc *types.FilterContext, out []byte) {
	// 简单解析：第一个字节为 tag，后续为值
	if len(out) < 3 {
		return
	}
	pos := 0
	for pos < len(out) {
		tag := out[pos]
		pos++
		if pos+2 > len(out) {
			return
		}
		l := int(out[pos])<<8 | int(out[pos+1])
		pos += 2
		if pos+l > len(out) {
			return
		}
		val := string(out[pos : pos+l])
		pos += l
		switch tag {
		case 'r':
			fc.Route = val
		case 'u':
			fc.UserUUID = val
		case 'm':
			fc.Metadata["wasm.note"] = val
		}
	}
}

// ErrWasmRuntimeNotEnabled WASM 运行时未启用
var ErrWasmRuntimeNotEnabled = fmt.Errorf("wasm runtime not enabled (build with -tags wasm)")

func init() {
	types.RegisterFilter("wasm-filter", func(cfg map[string]interface{}) (types.Filter, error) {
		return NewWasmFilter(util.GetString(cfg, "module"), util.GetString(cfg, "function"))
	})
}
