package traffic

import (
	"sync/atomic"

	"github.com/streasure/sgate/gateway/types"
)

// KernelHook eBPF 内核态下沉接口
//
// 目标：把高频低层规则匹配（IP 黑名单、限流计数、TCP 重传统计）下沉到
// XDP/TC 层，在包进入用户态前完成丢弃/统计，降低 user/kernel 上下文切换。
//
// 实现策略：
//   - 默认 stub（ebpf_hook_stub.go）：所有方法 NoOp，不引入任何依赖
//   - cilium/ebpf 实现（ebpf_hook_cilium.go，build tag `ebpf`）：
//     需要 CGO + 内核 BTF + clang 编译 BPF C 程序，仅 Linux 可用
//   - 启用：go build -tags ebpf
//
// 适配层网关：用户态逻辑仍由 SPI Filter 链处理，KernelHook 仅做加速/早期丢弃
type KernelHook interface {
	// Type 实现类型
	Type() string
	// AddBlacklistIP 在 XDP 层添加 IP 黑名单（包到 NIC 即丢，不进用户态）
	AddBlacklistIP(ip string) error
	// RemoveBlacklistIP 移除 XDP 黑名单
	RemoveBlacklistIP(ip string) error
	// AddRateLimit 在 TC ingress 层添加 per-IP 限速（BPF map 维护令牌）
	AddRateLimit(ip string, pps int) error
	// GetTCPRetransmits 获取 TCP 重传统计（kprobe tcp_retransmit_skb 采集）
	GetTCPRetransmits() (uint64, error)
	// GetConnStats 获取新建/活跃连接数（tracepoint sock/inet_sock_set_state）
	GetConnStats() (newConns, activeConns uint64, err error)
}

// ebpfHookSingleton 全局 eBPF hook 单例
var ebpfHookSingleton atomic.Pointer[KernelHook]

func SetKernelHook(h KernelHook) { ebpfHookSingleton.Store(&h) }

func GetKernelHook() KernelHook {
	p := ebpfHookSingleton.Load()
	if p == nil {
		return nil
	}
	return *p
}

// EBPFAcceleratorFilter 把 KernelHook 接入 SPI Filter 链
// 作用：在用户态 filter 之前先查询 XDP 是否已丢弃该 IP（统计/告警同步）
type EBPFAcceleratorFilter struct {
	hook KernelHook
}

func (f *EBPFAcceleratorFilter) Name() string       { return "ebpf-accelerator" }
func (f *EBPFAcceleratorFilter) Phase() types.FilterPhase { return types.PhasePreAuth }
func (f *EBPFAcceleratorFilter) Priority() int      { return 10 } // 最先执行

func (f *EBPFAcceleratorFilter) Process(fc *types.FilterContext) (bool, error) {
	if f.hook == nil {
		return true, nil
	}
	// XDP 已丢弃的包不会到达用户态；这里仅同步内核态统计
	// 若需在用户态二次确认，可调用 hook.IsBlacklisted(ip)
	// 当前实现：纯透传，统计由 metrics exporter 周期读取
	return true, nil
}

func init() {
	types.RegisterFilter("ebpf-accelerator", func(cfg map[string]interface{}) (types.Filter, error) {
		hook := GetKernelHook()
		return &EBPFAcceleratorFilter{hook: hook}, nil
	})
}
