package logic

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// ReliabilityConfig 可靠推送的容量与时序参数（yaml: reliability.*）。
// "端到端 ACK、不逐跳"的契约与 SeqId 语义进代码，行为参数进配置（决策⑦）。
// 零值字段由 withDefaults 回退，未配置即用默认。
type ReliabilityConfig struct {
	WindowSize     int           `yaml:"windowSize"`     // 每会话未确认窗口条数上限（<=0 默认 64）
	ScanInterval   time.Duration `yaml:"scanInterval"`   // 重传扫描周期（<=0 默认 200ms）
	InitialBackoff time.Duration `yaml:"initialBackoff"` // 首次重传退避（<=0 默认 300ms）
	MaxBackoff     time.Duration `yaml:"maxBackoff"`     // 退避指数上限（<=0 默认 5s）
	MaxRetries     int           `yaml:"maxRetries"`     // 单条消息最大重传次数，不含首发（<=0 默认 5）
}

// 可靠推送默认值唯一值源：零值回退共用，勿另写字面量。
const (
	defaultReliableWindowSize     = 64
	defaultReliableScanInterval   = 200 * time.Millisecond
	defaultReliableInitialBackoff = 300 * time.Millisecond
	defaultReliableMaxBackoff     = 5 * time.Second
	defaultReliableMaxRetries     = 5
)

// withDefaults 返回零值字段回退默认值后的副本。
func (c ReliabilityConfig) withDefaults() ReliabilityConfig {
	if c.WindowSize <= 0 {
		c.WindowSize = defaultReliableWindowSize
	}
	if c.ScanInterval <= 0 {
		c.ScanInterval = defaultReliableScanInterval
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = defaultReliableInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultReliableMaxBackoff
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = defaultReliableMaxRetries
	}
	return c
}

// ErrReliableWindowFull 会话未确认窗口已满的背压错误（决策⑥：关键消息绝不静默丢，
// 由业务决定等待/降级/放弃）。
var ErrReliableWindowFull = fmt.Errorf("logic: reliable window full")

// reliableEntry 未确认窗口条目：持有原始消息副本（重传原样复用）与重传调度状态。
type reliableEntry struct {
	msg       *protocol.StreamData // 下行消息（含 SessionId/Cmd/Data/SeqId），重传原样复用
	cmd       int32                // 业务命令码（日志用）
	attempts  int                  // 已发送次数（首发为 1）
	nextRetry time.Time            // 下次重传时间
}

// reliableWindow 单会话的未确认窗口：seq 单调分配 + 有界条目表。
// 按扫描组织而非每消息一个 timer（决策③：扫描式复用 janitor 模式）。
type reliableWindow struct {
	mu      sync.Mutex
	nextSeq int64 // 下一个待分配 seq（会话内单调，1 起）
	entries map[int64]*reliableEntry
}

// reliableManager 按会话维护窗口并驱动超时重传。
type reliableManager struct {
	ctx      context.Context
	mu       sync.Mutex
	sessions map[string]*reliableWindow // 会话 ID -> 窗口
	cfg      ReliabilityConfig
	// 至少发生过一次窗口满：避免"从未背压"的会话每轮扫描重复告警。
	droppedFull atomic.Bool
	// nowFn 便于测试注入时间。
	nowFn func() time.Time
}

func newReliableManager(ctx context.Context, cfg ReliabilityConfig) *reliableManager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &reliableManager{
		ctx:      ctx,
		sessions: make(map[string]*reliableWindow),
		cfg:      cfg.withDefaults(),
		nowFn:    time.Now,
	}
}

// windowFor 返回会话窗口，不存在则创建。
func (m *reliableManager) windowFor(sessionID string) *reliableWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.sessions[sessionID]
	if w == nil {
		w = &reliableWindow{entries: make(map[int64]*reliableEntry)}
		m.sessions[sessionID] = w
	}
	return w
}

// get 返回会话窗口（不创建），不存在返回 nil。
func (m *reliableManager) get(sessionID string) *reliableWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

// dropSession 会话终止（Offline / 断流清理）时删除窗口：决策④窗口随会话，
// 断线不跨会话续传，业务重连后走登录全量/增量同步拉取。
// 返回被清掉的未确认条目数（droppedOffline 指标）。
func (m *reliableManager) dropSession(sessionID string) int {
	m.mu.Lock()
	w := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if w == nil {
		return 0
	}
	w.mu.Lock()
	n := len(w.entries)
	w.entries = nil
	w.mu.Unlock()
	return n
}

// scanAll 一轮超时扫描。alive 判定会话是否仍在线：离线窗口整体回收（含
// Offline/断流与扫描竞态产生的孤儿窗口）。
// 返回 (到期条目按会话分组, 本轮放弃条目数, 本轮随离线窗口回收条目数)。
// 到期条目只"取出调度"，实际重发由调用方在锁外执行（conn.Send 可能阻塞，
// 不能拖住其它会话的扫描）。
func (m *reliableManager) scanAll(alive func(sessionID string) bool) (due map[string][]reliableEntry, gaveUp, reclaimed int) {
	now := m.nowFn()
	cfg := m.cfg
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultReliableMaxRetries
	}

	m.mu.Lock()
	windows := make(map[string]*reliableWindow, len(m.sessions))
	for sid, w := range m.sessions {
		windows[sid] = w
	}
	m.mu.Unlock()

	due = make(map[string][]reliableEntry, len(windows))
	for sid, w := range windows {
		if !alive(sid) {
			// 会话已不在（Offline/断流清理的竞态兜底）：窗口连同未确认条目回收。
			reclaimed += m.dropSession(sid)
			continue
		}
		w.mu.Lock()
		for seq, e := range w.entries {
			if now.Before(e.nextRetry) {
				continue // 尚未到期
			}
			if e.attempts-1 >= maxRetries {
				// 决策③：放弃 + 计数告警，业务侧经 PushMetrics 观测。
				delete(w.entries, seq)
				gaveUp++
				tlog.Warn(m.ctx, "logic.reliable give up sessionID=%s cmd=%d seq=%d attempts=%d",
					sid, e.cmd, seq, e.attempts)
				continue
			}
			e.attempts++
			e.nextRetry = now.Add(backoffFor(cfg, e.attempts))
			due[sid] = append(due[sid], *e)
		}
		w.mu.Unlock()
	}
	return due, gaveUp, reclaimed
}

// backoffFor 第 attempt 次发送后（首发 attempt=1）到下一次重传的退避时长：
// 指数增长 InitialBackoff * 2^(attempt-1)，上限 MaxBackoff。
// 即首发后 300ms、第 1 次重传后 600ms、第 2 次重传后 1.2s……
func backoffFor(cfg ReliabilityConfig, attempt int) time.Duration {
	d := cfg.InitialBackoff
	if d <= 0 {
		d = defaultReliableInitialBackoff
	}
	max := cfg.MaxBackoff
	if max <= 0 {
		max = defaultReliableMaxBackoff
	}
	for i := 1; i < attempt; i++ {
		if d >= max {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// WithReliableConfig 设置可靠推送参数（零值字段回退默认）。
// 与 ServiceOption 的 WithReliability（配置装配）区分：本项直接作用于 Server。
func WithReliableConfig(cfg ReliabilityConfig) ServerOption {
	return func(s *Server) { s.reliable.cfg = cfg.withDefaults() }
}

// PushReliable 向指定会话推送需端到端确认的关键消息。
//
// 与 PushToConnection 的区别：消息带会话内单调 SeqId 并进入未确认窗口，
// 客户端回 CmdMsgAck 后移出窗口；超时未确认由 janitor 指数退避重传。
// 窗口满返回 ErrReliableWindowFull（背压，绝不静默丢）；首发失败回滚条目
// 并返回错误，由业务决定重试（客户端去重窗口兜住重复投递）。
// 会话 Offline/断流即清窗：本机制只保证会话内 at-least-once，跨会话靠重连拉取。
func (s *Server) PushReliable(sessionID string, targetCmd int32, data []byte) error {
	value, ok := s.sessions.Load(sessionID)
	if !ok {
		s.metrics.TotalFailed.Add(1)
		return fmt.Errorf("logic: session %q not found", sessionID)
	}
	conn := value.(*streamConn)
	s.startReliableJanitor()

	w := s.reliable.windowFor(sessionID)
	now := time.Now()
	w.mu.Lock()
	if len(w.entries) >= s.reliable.cfg.WindowSize {
		w.mu.Unlock()
		s.metrics.TotalFailed.Add(1)
		// 首次背压记一条日志，其后只计数（每条一条会刷屏）。
		if !s.reliable.droppedFull.Swap(true) {
			tlog.Warn(s.ctx, "logic.reliable window full, backpressure sessionID=%s size=%d",
				sessionID, s.reliable.cfg.WindowSize)
		}
		return ErrReliableWindowFull
	}
	w.nextSeq++
	seq := w.nextSeq
	msg := &protocol.StreamData{SessionId: sessionID, Cmd: targetCmd, Data: data, SeqId: seq}
	// 先置 attempts/nextRetry 再入窗：避免 janitor 把"尚未首发"的条目当到期重传。
	// 首发（attempt=1）后等 InitialBackoff 再重传。
	e := &reliableEntry{
		msg: msg, cmd: targetCmd, attempts: 1,
		nextRetry: now.Add(backoffFor(s.reliable.cfg, 1)),
	}
	w.entries[seq] = e
	w.mu.Unlock()

	if err := conn.Send(msg); err != nil {
		// 首发失败：回滚条目释放窗口位，错误交还业务（绝不静默丢）。
		w.mu.Lock()
		delete(w.entries, seq)
		w.mu.Unlock()
		s.metrics.TotalFailed.Add(1)
		return err
	}
	s.metrics.TotalPushed.Add(1)
	return nil
}

// handleMsgAck 处理客户端端到端确认：批量移出未确认窗口。
// 重复/迟到 ACK（条目已不在）为 no-op，天然幂等；窗口已随会话回收时同样吞掉。
func (s *Server) handleMsgAck(msg *protocol.StreamData) {
	var ack protoLogic.MsgAck
	if err := proto.Unmarshal(msg.Data, &ack); err != nil {
		tlog.Warn(s.ctx, "logic.reliable bad ack sessionID=%s error=%v", msg.SessionId, err)
		return
	}
	s.reliable.mu.Lock()
	w := s.reliable.sessions[msg.SessionId]
	s.reliable.mu.Unlock()
	if w == nil || len(ack.SeqIds) == 0 {
		return
	}
	acked := int64(0)
	w.mu.Lock()
	for _, seq := range ack.SeqIds {
		if _, ok := w.entries[seq]; ok {
			delete(w.entries, seq)
			acked++
		}
	}
	w.mu.Unlock()
	if acked > 0 {
		s.metrics.Acked.Add(acked)
	}
}

// clearReliable 会话终止时清窗（Offline 与断流清理共用）。
// 决策④：窗口随会话，断线不跨会话续传。
func (s *Server) clearReliable(sessionID string) {
	if n := s.reliable.dropSession(sessionID); n > 0 {
		s.metrics.DroppedOffline.Add(int64(n))
	}
}

// startReliableJanitor 惰性启动重传扫描协程（首个 PushReliable 时启动，
// 未使用可靠推送的进程零开销）。Stop 后再调用会立即退出。
func (s *Server) startReliableJanitor() {
	s.reliableOnce.Do(func() {
		interval := s.reliable.cfg.ScanInterval
		panicutil.GoLoop(s.ctx, "logic.reliableJanitor", func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-s.reliableStop:
					return
				case <-ticker.C:
					s.reliableTick()
				}
			}
		})
	})
}

// reliableTick 单轮重传扫描：回收离线窗口、放弃超限条目、重发到期条目。
func (s *Server) reliableTick() {
	alive := func(sessionID string) bool {
		_, ok := s.sessions.Load(sessionID)
		return ok
	}
	due, gaveUp, reclaimed := s.reliable.scanAll(alive)
	if gaveUp > 0 {
		s.metrics.GiveUps.Add(int64(gaveUp))
	}
	if reclaimed > 0 {
		s.metrics.DroppedOffline.Add(int64(reclaimed))
	}
	for sessionID, entries := range due {
		value, ok := s.sessions.Load(sessionID)
		if !ok {
			continue // 扫描与断流竞态：条目留在窗口，下轮 alive 判定回收
		}
		conn := value.(*streamConn)
		for _, e := range entries {
			if err := conn.Send(e.msg); err != nil {
				// 发送失败不重置调度：attempts 已计入，超限自然放弃。
				tlog.Warn(s.ctx, "logic.reliable resend failed sessionID=%s cmd=%d seq=%d attempts=%d error=%v",
					sessionID, e.cmd, e.msg.SeqId, e.attempts, err)
				continue
			}
			s.metrics.Retransmits.Add(1)
		}
	}
}
