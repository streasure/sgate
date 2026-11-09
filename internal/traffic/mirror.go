package traffic

import (
	"context"
	"sync"
	"sync/atomic"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/gatewayutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uhash"
)

// TrafficMirror 流量镜像器
// 把生产流量按比例复制一份到测试环境，不影响主流程
type TrafficMirror struct {
	ctx        context.Context
	mu         sync.RWMutex
	percent    int // 镜像比例 0-100
	sender     *mirrorSender
	queue      chan *types.FilterContext
	stopCh     chan struct{}
	stopOnce   sync.Once
	workersWG  sync.WaitGroup
	enabled    atomic.Int32
	dropped    atomic.Int64
	forwarded  atomic.Int64
	sendFailed atomic.Int64
}

// NewTrafficMirror 创建流量镜像器
func NewTrafficMirror(cfg config.TrafficMirrorConfig) *TrafficMirror {
	tm := &TrafficMirror{
		ctx:     context.Background(),
		percent: cfg.Percent,
		queue:   make(chan *types.FilterContext, max(cfg.QueueSize, 1024)),
		stopCh:  make(chan struct{}),
	}
	if cfg.Enabled {
		tm.enabled.Store(1)
		// 镜像目标缺失/非法时保持可运行（主流程不受影响），但所有采样都会计入 sendFailed
		switch {
		case cfg.TargetAddr == "":
			tlog.Warn(tm.ctx, "traffic mirror enabled without targetAddr, sampled messages will be dropped")
		default:
			tm.sender = newMirrorSender(cfg.TargetAddr)
			if tm.sender == nil {
				tlog.Warn(tm.ctx, "traffic mirror invalid targetAddr, sampled messages will be dropped target=%s", cfg.TargetAddr)
			}
		}
	}
	// 启动 worker 异步发送镜像流量（避免阻塞主流程）
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	for range workers {
		tm.workersWG.Go(tm.worker)
	}
	return tm
}

// MirrorFilter 流量镜像过滤器
type MirrorFilter struct {
	TM *TrafficMirror
}

func (mf *MirrorFilter) Name() string             { return "traffic-mirror" }
func (mf *MirrorFilter) Phase() types.FilterPhase { return types.PhasePostAuth }
func (mf *MirrorFilter) Priority() int            { return 300 }

func (mf *MirrorFilter) Process(fc *types.FilterContext) (bool, error) {
	if mf.TM == nil || mf.TM.enabled.Load() == 0 {
		return true, nil
	}
	mf.TM.Mirror(fc)
	return true, nil
}

// Mirror 异步投递镜像任务
func (tm *TrafficMirror) Mirror(fc *types.FilterContext) {
	tm.mu.RLock()
	percent := tm.percent
	tm.mu.RUnlock()
	if percent <= 0 {
		return
	}
	// 按 connectionID 哈希采样（实际可换成更精确的随机）
	h := uhash.FNV1a32(fc.ConnectionID) % 100
	if int(h) >= percent {
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
		Metadata:     gatewayutil.CopyMap(fc.Metadata),
	}
	select {
	case tm.queue <- clone:
		tm.forwarded.Add(1)
	default:
		tm.dropped.Add(1)
	}
}

func (tm *TrafficMirror) worker() {
	for {
		select {
		case <-tm.stopCh:
			return
		case fc := <-tm.queue:
			tm.forward(fc)
		}
	}
}

// forward 把采样入队的上下文转成 StreamData 推给镜像目标。
// 镜像只复制客户端上行：SessionId/Cmd/Data/UserKey/ClientIp 即目标侧重放所需全部字段；
// Route 是本侧路由键，StreamData 无对应字段（目标侧按 cmd 分发），不透传。
// 发送失败计入 sendFailed（与队列满丢弃 dropped 分开计），不回压主流程。
func (tm *TrafficMirror) forward(fc *types.FilterContext) {
	if tm.sender == nil {
		tm.sendFailed.Add(1)
		return
	}
	err := tm.sender.Send(&protoGw.StreamData{
		SessionId: fc.ConnectionID,
		UserKey:   fc.UserUUID,
		Cmd:       fc.Cmd,
		Data:      fc.Data,
		ClientIp:  fc.RemoteIP,
	})
	if err != nil {
		tm.sendFailed.Add(1)
	}
}

func (tm *TrafficMirror) Stop() {
	tm.stopOnce.Do(func() { close(tm.stopCh) })
	tm.workersWG.Wait()
	if tm.sender != nil {
		tm.sender.Close()
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

// SendFailed 发送失败数：目标不可达或处于退避窗口而未投递的消息。
// 与队列满丢弃（dropped）分开计，便于区分「目标故障」与「消费不过来」。
func (tm *TrafficMirror) SendFailed() int64 {
	return tm.sendFailed.Load()
}

func init() {
	types.RegisterFilter("traffic-mirror", func(cfg map[string]any) (types.Filter, error) {
		c := config.TrafficMirrorConfig{
			Percent:    gatewayutil.GetInt(cfg, "percent"),
			TargetAddr: gatewayutil.GetString(cfg, "targetAddr"),
			QueueSize:  gatewayutil.GetInt(cfg, "queueSize"),
			Workers:    gatewayutil.GetInt(cfg, "workers"),
		}
		tm := NewTrafficMirror(c)
		return &MirrorFilter{TM: tm}, nil
	})
}
