package gateway

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

type OverloadProtector struct {
	memoryThreshold float64
	cpuThreshold    float64
	dropOnOverload  bool
	overloadFlag    atomic.Int32
	checkInterval   time.Duration
	stopChan        chan struct{}
	proc            *process.Process
	memPercent      float64
	cpuPercent      float64
	heapThreshold   uint64
	totalDropped    atomic.Int64
	lastLogTime     atomic.Int64
}

func NewOverloadProtector(cfg config.ProtectionConfig) *OverloadProtector {
	memThreshold := cfg.MemoryThreshold
	if memThreshold <= 0 {
		memThreshold = 90.0
	}
	cpuThreshold := cfg.CPUThreshold
	if cpuThreshold <= 0 {
		cpuThreshold = 90.0
	}

	var heapThreshold uint64
	vm, err := mem.VirtualMemory()
	if err == nil && vm.Total > 0 {
		heapThreshold = uint64(float64(vm.Total) * memThreshold / 100.0)
	} else {
		heapThreshold = 4 * 1024 * 1024 * 1024
	}

	proc, _ := process.NewProcess(int32(os.Getpid()))
	if proc != nil {
		proc.Percent(500 * time.Millisecond)
	}

	checkInterval := 200 * time.Millisecond
	if cfg.CheckIntervalMs > 0 {
		checkInterval = time.Duration(cfg.CheckIntervalMs) * time.Millisecond
	}

	return &OverloadProtector{
		memoryThreshold: memThreshold,
		cpuThreshold:    cpuThreshold,
		dropOnOverload:  cfg.DropOnOverload,
		checkInterval:   checkInterval,
		stopChan:        make(chan struct{}),
		proc:            proc,
		heapThreshold:   heapThreshold,
	}
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

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.Alloc > op.heapThreshold {
		overloaded = true
	}

	if !overloaded && op.proc != nil {
		cpuPct, err := op.proc.Percent(0)
		if err == nil {
			cpuPct /= float64(runtime.NumCPU())
			if cpuPct > op.cpuThreshold {
				overloaded = true
			}
		}
		op.cpuPercent = cpuPct
	}

	if !overloaded {
		vm, err := mem.VirtualMemory()
		if err == nil && vm.UsedPercent > op.memoryThreshold {
			overloaded = true
		}
		op.memPercent = vm.UsedPercent
	}

	if overloaded && op.dropOnOverload {
		op.overloadFlag.Store(1)
		now := time.Now().UnixMilli()
		lastLog := op.lastLogTime.Load()
		if now-lastLog > 5000 {
			op.lastLogTime.Store(now)
			tlog.Warn("overload detected, dropping messages",
				"cpu", op.cpuPercent,
				"mem", op.memPercent,
				"heapAlloc", m.Alloc/1024/1024,
				"heapThreshold", op.heapThreshold/1024/1024,
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
	if _, err := mem.VirtualMemory(); err != nil {
		tlog.Warn("gopsutil memory monitor unavailable on this platform, using Go runtime stats only", "error", err)
	}
	if _, err := process.NewProcess(int32(os.Getpid())); err != nil {
		tlog.Warn("gopsutil process monitor unavailable on this platform, CPU monitoring disabled", "error", err)
	}
}
