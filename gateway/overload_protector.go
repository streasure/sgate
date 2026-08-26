package gateway

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v3/process"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// OverloadProtector 基于进程 CPU 使用率判断过载。
//
// 设计决策（只看 CPU，不看内存）：
//   - GOMEMLIMIT（Go 1.19+）已在 runtime 层硬性限制 Go 堆上限，超限时自动触发 GC，
//     应用层不需要重复检查内存——重复检查会在高 inflow 场景下误丢弃正常流量
//     （千万级 QPS 时几 GB inflight 数据是正常的，不代表过载）
//   - 系统内存使用率（mem.VirtualMemory）受背景进程干扰，Windows 上常 95%+，误判严重
//   - 进程 RSS 包含 native 库/虚拟内存映射等非 Go 堆内存，会超过 GOMEMLIMIT，误判
//   - CPU 使用率是真正的过载信号：当 sgate 进程 CPU 占满所有核心时，处理能力达上限
//
// CPU 计算方式：使用 process.Times() 手动计算增量，而非 gopsutil 的 Percent(0)。
// Percent(0) 在 Windows 上可能返回错误值（如 9393%），因为首次调用的基线不稳定。
// 手动计算：delta_cpu / delta_wall / NumCPU * 100，结果稳定且跨平台一致。
type OverloadProtector struct {
	cpuThreshold   float64
	dropOnOverload bool
	overloadFlag    atomic.Int32
	checkInterval   time.Duration
	stopChan        chan struct{}
	proc            *process.Process
	memPercent      float64 // 保留用于监控展示（heap 占 GOMEMLIMIT 百分比）
	cpuPercent      float64
	heapThreshold   uint64 // 仅用于 memPercent 展示，不参与过载判断
	totalDropped    atomic.Int64
	lastLogTime     atomic.Int64
	// CPU 增量计算基线
	lastCPUTime    float64 // 上次 process.Times() 的 User+System 总和（秒）
	lastCheckTime time.Time
}

// parseGOMEMLIMIT 读取 GOMEMLIMIT 环境变量（如 "4GiB"/"4096MiB"/"4294967296"）。
// Go 1.19+ runtime 通过该环境变量设置软内存上限。
func parseGOMEMLIMIT() uint64 {
	v := os.Getenv("GOMEMLIMIT")
	if v == "" {
		return 0
	}
	v = strings.TrimSpace(v)
	// 支持 B/KiB/MiB/GiB/TiB 后缀（Go runtime 格式）
	multiplier := uint64(1)
	numPart := v
	switch {
	case strings.HasSuffix(v, "GiB"):
		multiplier = 1024 * 1024 * 1024
		numPart = strings.TrimSuffix(v, "GiB")
	case strings.HasSuffix(v, "MiB"):
		multiplier = 1024 * 1024
		numPart = strings.TrimSuffix(v, "MiB")
	case strings.HasSuffix(v, "KiB"):
		multiplier = 1024
		numPart = strings.TrimSuffix(v, "KiB")
	case strings.HasSuffix(v, "TiB"):
		multiplier = 1024 * 1024 * 1024 * 1024
		numPart = strings.TrimSuffix(v, "TiB")
	case strings.HasSuffix(v, "G"):
		multiplier = 1024 * 1024 * 1024
		numPart = strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		multiplier = 1024 * 1024
		numPart = strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		multiplier = 1024
		numPart = strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0
	}
	return uint64(n * float64(multiplier))
}

func NewOverloadProtector(cfg config.ProtectionConfig) *OverloadProtector {
	cpuThreshold := cfg.CPUThreshold
	if cpuThreshold <= 0 {
		cpuThreshold = 90.0
	}

	// memLimit：GOMEMLIMIT 环境变量值（字节），0 表示未设置
	memLimit := parseGOMEMLIMIT()

	// heapThreshold：heap.Alloc 阈值。
	// 若设了 GOMEMLIMIT，取其 90%（留 10% 余量给 GC 回收）。
	// 否则默认 4GiB 兜底。
	var heapThreshold uint64 = 4 * 1024 * 1024 * 1024
	if memLimit > 0 {
		heapThreshold = uint64(float64(memLimit) * 0.9)
	}

	proc, _ := process.NewProcess(int32(os.Getpid()))

	checkInterval := 200 * time.Millisecond
	if cfg.CheckIntervalMs > 0 {
		checkInterval = time.Duration(cfg.CheckIntervalMs) * time.Millisecond
	}

	op := &OverloadProtector{
		cpuThreshold:   cpuThreshold,
		dropOnOverload: cfg.DropOnOverload,
		checkInterval:   checkInterval,
		stopChan:        make(chan struct{}),
		proc:            proc,
		heapThreshold:   heapThreshold,
	}

	// 初始化 CPU 基线：读取当前 process.Times()，供后续增量计算
	if proc != nil {
		if times, err := proc.Times(); err == nil {
			op.lastCPUTime = times.User + times.System
		}
	}
	op.lastCheckTime = time.Now()

	return op
}

func (op *OverloadProtector) Start() {
	go func() {
		ticker := time.NewTicker(op.checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-op.stopChan:
				return
			case <-ticker.C:
				op.check()
			}
		}
	}()
}

func (op *OverloadProtector) check() {
	overloaded := false

	// 仅更新 memPercent 用于监控展示（heap 占 GOMEMLIMIT 百分比），不参与过载判断
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if op.heapThreshold > 0 {
		op.memPercent = float64(m.Alloc) / float64(op.heapThreshold) * 100.0
	}

	// CPU 检查：使用 process.Times() 手动计算增量，避免 Percent(0) 在 Windows 上的 bug
	// 计算：delta_cpu_time / delta_wall_time / NumCPU * 100
	// 例如：12 核机器，200ms 内 CPU 时间增加 2.4s → 2.4/0.2/12*100 = 100%
	if op.proc != nil {
		times, err := op.proc.Times()
		if err == nil {
			curCPUTime := times.User + times.System
			deltaCPU := curCPUTime - op.lastCPUTime
			op.lastCPUTime = curCPUTime

			now := time.Now()
			deltaWall := now.Sub(op.lastCheckTime).Seconds()
			op.lastCheckTime = now

			if deltaWall > 0 {
				cpuPct := (deltaCPU / deltaWall) / float64(runtime.NumCPU()) * 100.0
				// 限制在合理范围 [0, 100*NumCPU]
				maxCPU := float64(runtime.NumCPU()) * 100.0
				if cpuPct > maxCPU {
					cpuPct = maxCPU
				}
				if cpuPct > op.cpuThreshold {
					overloaded = true
				}
				op.cpuPercent = cpuPct
			}
		}
	}

	if overloaded && op.dropOnOverload {
		op.overloadFlag.Store(1)
		now := time.Now().UnixMilli()
		lastLog := op.lastLogTime.Load()
		if now-lastLog > 5000 {
			op.lastLogTime.Store(now)
			tlog.Warn("overload detected, dropping messages",
				"cpu", op.cpuPercent,
				"heapPercent", op.memPercent,
				"heapAllocMB", m.Alloc/1024/1024,
				"totalDropped", op.totalDropped.Load(),
			)
		}
	} else {
		op.overloadFlag.Store(0)
	}
}

func (op *OverloadProtector) IsOverloaded() bool {
	return op.overloadFlag.Load() == 1
}

func (op *OverloadProtector) RecordDrop(n int64) {
	op.totalDropped.Add(n)
}

func (op *OverloadProtector) Stats() (cpuPercent float64, memPercent float64, overloaded bool, dropped int64) {
	return op.cpuPercent, op.memPercent, op.IsOverloaded(), op.totalDropped.Load()
}

func (op *OverloadProtector) Stop() {
	close(op.stopChan)
}

func init() {
	if _, err := process.NewProcess(int32(os.Getpid())); err != nil {
		tlog.Warn("gopsutil process monitor unavailable on this platform, CPU/RSS monitoring disabled", "error", err)
	}
}
