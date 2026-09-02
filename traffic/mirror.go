package traffic

import (
	"sync"
	"sync/atomic"

	"github.com/streasure/util/tlog"
	"github.com/streasure/sgate/types"
	"github.com/streasure/sgate/util"
	"github.com/streasure/sgate/internal/config"
)

// TrafficMirror 流量镜像器
// 把生产流量按比例复制一份到测试环境，不影响主流程
type TrafficMirror struct {
	mu         sync.RWMutex
	percent    int    // 镜像比例 0-100
	targetAddr string // 镜像目标地址（逻辑服务）
	queue      chan *types.FilterContext
	enabled    atomic.Int32
	dropped    atomic.Int64
	forwarded  atomic.Int64
}

// NewTrafficMirror 创建流量镜像器
func NewTrafficMirror(cfg config.TrafficMirrorConfig) *TrafficMirror {
	tm := &TrafficMirror{
		percent:    cfg.Percent,
		targetAddr: cfg.TargetAddr,
		queue:      make(chan *types.FilterContext, util.MaxInt(cfg.QueueSize, 1024)),
	}
	if cfg.Enabled {
		tm.enabled.Store(1)
	}
	// 启动 worker 异步发送镜像流量（避免阻塞主流程）
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		go tm.worker()
	}
	return tm
}

// MirrorFilter 流量镜像过滤器
type MirrorFilter struct {
	TM *TrafficMirror
}

func (mf *MirrorFilter) Name() string       { return "traffic-mirror" }
func (mf *MirrorFilter) Phase() types.FilterPhase { return types.PhasePostAuth }
func (mf *MirrorFilter) Priority() int      { return 300 }

func (mf *MirrorFilter) Process(fc *types.FilterContext) (bool, error) {
	if mf.TM == nil || mf.TM.enabled.Load() == 0 {
		return true, nil
	}
	mf.TM.Mirror(fc)
	return true, nil
}

// Mirror 异步投递镜像任务
func (tm *TrafficMirror) Mirror(fc *types.FilterContext) {
	if tm.percent <= 0 {
		return
	}
	// 按 connectionID 哈希采样（实际可换成更精确的随机）
	h := util.SimpleHash(fc.ConnectionID) % 100
	if int(h) >= tm.percent {
		return
	}
	// 浅拷贝上下文，避免共享可变状态
	clone := &types.FilterContext{
		ConnectionID: fc.ConnectionID,
		RemoteIP:     fc.RemoteIP,
		Route:        fc.Route,
		Cmd:          fc.Cmd,
		Data:         append([]byte(nil), fc.Data...),
		UserUUID:     fc.UserUUID,
		Metadata:     util.CopyMap(fc.Metadata),
	}
	clone.Mirrored = true
	select {
	case tm.queue <- clone:
		tm.forwarded.Add(1)
	default:
		tm.dropped.Add(1)
	}
}

func (tm *TrafficMirror) worker() {
	for fc := range tm.queue {
		// 此处接入镜像目标（实现简化：仅日志记录）
		// 实际生产可调用 mirror 专用 LogicClient
		tlog.Debug("traffic mirror",
			"route", fc.Route,
			"conn", fc.ConnectionID,
			"target", tm.targetAddr)
	}
}

func (tm *TrafficMirror) UpdatePercent(p int) {
	tm.mu.Lock()
	tm.percent = p
	tm.mu.Unlock()
}

func (tm *TrafficMirror) Stats() (forwarded, dropped int64) {
	return tm.forwarded.Load(), tm.dropped.Load()
}

func init() {
	types.RegisterFilter("traffic-mirror", func(cfg map[string]interface{}) (types.Filter, error) {
		c := config.TrafficMirrorConfig{
			Percent:    util.GetInt(cfg, "percent"),
			TargetAddr: util.GetString(cfg, "targetAddr"),
			QueueSize:  util.GetInt(cfg, "queueSize"),
			Workers:    util.GetInt(cfg, "workers"),
		}
		tm := NewTrafficMirror(c)
		return &MirrorFilter{TM: tm}, nil
	})
}
