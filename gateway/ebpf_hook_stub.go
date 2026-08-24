//go:build !ebpf

// 默认 stub：未启用 eBPF 内核态加速
// 启用：go build -tags ebpf
//   - 引入 cilium/ebpf（CGO，仅 Linux + 内核 BTF）
//   - 实现：ebpf_hook_cilium.go
//   - 还需预编译 BPF C 程序（XDP/TC/kprobe）
package gateway

import "errors"

type ebpfStub struct{}

func (ebpfStub) Type() string                                   { return "stub" }
func (ebpfStub) AddBlacklistIP(string) error                   { return ErrEBPFNotEnabled }
func (ebpfStub) RemoveBlacklistIP(string) error                { return ErrEBPFNotEnabled }
func (ebpfStub) AddRateLimit(string, int) error                { return ErrEBPFNotEnabled }
func (ebpfStub) GetTCPRetransmits() (uint64, error)            { return 0, ErrEBPFNotEnabled }
func (ebpfStub) GetConnStats() (uint64, uint64, error)         { return 0, 0, ErrEBPFNotEnabled }

// ErrEBPFNotEnabled eBPF 未启用错误
var ErrEBPFNotEnabled = errors.New("eBPF not enabled (build with -tags ebpf)")

func init() {
	SetKernelHook(ebpfStub{})
}
