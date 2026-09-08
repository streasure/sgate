// 默认 stub：未启用 eBPF 内核态加速
package traffic

import "errors"

type ebpfStub struct{}

func (ebpfStub) Type() string                          { return "stub" }
func (ebpfStub) AddBlacklistIP(string) error           { return ErrEBPFNotEnabled }
func (ebpfStub) RemoveBlacklistIP(string) error        { return ErrEBPFNotEnabled }
func (ebpfStub) AddRateLimit(string, int) error        { return ErrEBPFNotEnabled }
func (ebpfStub) GetTCPRetransmits() (uint64, error)    { return 0, ErrEBPFNotEnabled }
func (ebpfStub) GetConnStats() (uint64, uint64, error) { return 0, 0, ErrEBPFNotEnabled }

// ErrEBPFNotEnabled eBPF 未启用错误
var ErrEBPFNotEnabled = errors.New("eBPF acceleration is not available in the standard build")

func init() {
	SetKernelHook(ebpfStub{})
}
