//go:build ebpf

// +build ebpf

// 启用：go build -tags ebpf
// 前置条件：
//   1. Linux 内核 >= 5.7（BTF 支持）
//   2. 安装 clang/llvm（编译 BPF C 程序）
//   3. 安装 libbpf-dev
//   4. CGO_ENABLED=1
//   5. go.mod 增加：require github.com/cilium/ebpf v0.12.0
//
// 部署模式：
//   - DaemonSet 部署 sgate-bpf-loader 容器（privileged，加载 BPF 程序）
//   - sgate 主进程通过 cilium/ebpf 用户态库读写 BPF map
//   - BPF map 跨进程共享：sgate 写黑名单 → XDP 程序实时读取丢弃
//
// XDP 程序示例（C，编译为 .o 加载）：
//   SEC("xdp") int sgate_xdp(struct xdp_md *ctx) {
//     // 解析 IP 头
//     // 查 ip_blacklist map
//     // 命中则 XDP_DROP
//     return XDP_PASS;
//   }
package gateway

import (
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	tlog "github.com/streasure/treasure-slog"
)

// CiliumEBPFHook cilium/ebpf 实现
type CiliumEBPFHook struct {
	mu          sync.Mutex
	collection  *ebpf.Collection
	xdpLink     link.Link
	tcLink      link.Link
	blacklistMap *ebpf.Map
	rateLimitMap *ebpf.Map
	tcpRetransmitMap *ebpf.Map
}

// NewCiliumEBPFHook 创建 hook（需先加载预编译的 BPF .o 文件）
func NewCiliumEBPFHook(bpfObjPath string) (*CiliumEBPFHook, error) {
	// 实际实现需调用 ebpf.LoadCollectionSpec + ebpf.NewCollection
	// 这里仅提供框架，完整实现需结合 BPF C 程序设计
	h := &CiliumEBPFHook{}
	tlog.Info("cilium/ebpf hook initialized", "obj", bpfObjPath)
	return h, nil
}

func (h *CiliumEBPFHook) Type() string { return "cilium-ebpf" }

func (h *CiliumEBPFHook) AddBlacklistIP(ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.blacklistMap == nil {
		return fmt.Errorf("blacklist map not loaded")
	}
	// 实际：解析 IP → 4 字节 key，写入 map
	return h.blacklistMap.Update(uint32IPKey(ip), uint8(1), ebpf.UpdateAny)
}

func (h *CiliumEBPFHook) RemoveBlacklistIP(ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.blacklistMap == nil {
		return fmt.Errorf("blacklist map not loaded")
	}
	return h.blacklistMap.Delete(uint32IPKey(ip))
}

func (h *CiliumEBPFHook) AddRateLimit(ip string, pps int) error {
	// 实际：写入 rate_limit_map[ip] = pps
	return nil
}

func (h *CiliumEBPFHook) GetTCPRetransmits() (uint64, error) {
	// 实际：读 tcp_retransmit_map 全局计数
	return 0, nil
}

func (h *CiliumEBPFHook) GetConnStats() (uint64, uint64, error) {
	// 实际：读 sock_set_state map
	return 0, 0, nil
}

// uint32IPKey 把 "1.2.3.4" 转 4 字节 key（兼容 BPF map）
func uint32IPKey(ip string) uint32 {
	// 简化实现：完整版用 netip.Addr
	return 0
}

func init() {
	// 实际启用需通过环境变量指定 BPF .o 路径
	SetKernelHook(&CiliumEBPFHook{})
	tlog.Info("eBPF runtime initialized (cilium/ebpf)")
}
