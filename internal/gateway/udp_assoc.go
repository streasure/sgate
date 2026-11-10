package gateway

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/netutil"
	"github.com/streasure/util/tlog"
)

// UDP 关联通道：WS 会话 + UDP 关联（设计见 docs/udp-ws-design.md §5.1）。
//
// 关联生命周期严格挂在会话上——会话是根，WS 断则关联一起销毁。收包热路径
// （lookup/touch）走 sync.Map 无锁读；结构性变更（create/delete/token）持 mu，
// 两者不互斥进同一把锁，保证多核 UDP 收包不被关联建立/销毁串行化。

// udpPendingToken 登录成功时下发的一次性关联 token（尚未被 ASSOCIATE 消费）。
type udpPendingToken struct {
	connID   string
	expireAt int64 // UnixMilli，超时未消费由 janitor 清理
}

// udpAssociation 一个会话的 UDP 关联。
type udpAssociation struct {
	connID     string    // 关联的会话 connectionID（会话根）
	addrKey    string    // 客户端公网地址 ip:port（byAddr 索引键）
	publicAddr net.Addr  // 回程目标（ASSOCIATE 源地址防御拷贝，阶段 3 下行 SendTo 用）
	ip         string    // 客户端公网 IP（per-IP 上限计数）
	udpConn    gnet.Conn // ASSOCIATE 包的 conn：fd/pc 属监听 socket，长期有效，
	// 后续用 SendTo(frame, addr) 向任意地址发包（gnet 每数据报新建 conn，
	// release 不关 fd/pc、不动 isDatagram，见 gnet v2.9.7 connection_unix/windows）

	token     string       // 已消费的一次性 token（审计/防重放记录）
	seqBase   int64        // 关联时分配的上行 seq 基线（回 Ack，客户端起始序号）
	stateVer  atomic.Int64 // 下行状态版本（单调递增，阶段 3 生效）
	lastSeen  atomic.Int64 // 最后收包时间（UnixMilli，空闲淘汰依据）
	createdAt int64        // 创建时间（UnixMilli）

	// 上行 seq 滑动窗口去重（§4.3，阶段 2）。位图位 i 对应 seq = seqMax - i，
	// 窗口大小 = 字长 × 64（首包时按 config.seqWindow 分配，向上取整 64 倍数）。
	// 正常情况下同源地址数据报在同一 event loop 串行处理，锁不竞争；
	// seqMu 防御 reuseport 分发异常/未来多 loop 读写。
	seqMu    sync.Mutex
	seqMax   int64    // 窗口内最大已见 seq
	seqWords []uint64 // nil = 尚未初始化（首包直接放行并定基）
}

// udpAssocTable 关联表：两个收包索引 + 结构性辅助表（mu 保护）。
type udpAssocTable struct {
	mu       sync.Mutex // 只保护结构性变更，不进收包热路径
	byConnID sync.Map   // connID → *udpAssociation（回程/清理）
	byAddr   sync.Map   // addrKey(ip:port) → connID（收包反查）

	perIP         map[string]int              // ip → 关联数（mu）
	tokens        map[string]*udpPendingToken // token → 待消费（mu，一次性）
	tokenByConnID map[string]string           // connID → token（mu，重登录撤销旧 token）
	count         int                         // 关联总数（mu）
}

func newUDPAssocTable() *udpAssocTable {
	return &udpAssocTable{
		perIP:         make(map[string]int),
		tokens:        make(map[string]*udpPendingToken),
		tokenByConnID: make(map[string]string),
	}
}

// issueToken 生成并登记一次性关联 token。同会话重复登录会撤销旧 token（旧 token
// 不得复活）。token 是 16 字节 crypto/rand 的 hex（128 位不可猜）。
func (t *udpAssocTable) issueToken(connID string, ttl time.Duration) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate udp token: %w", err)
	}
	token := hex.EncodeToString(b[:])

	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.tokenByConnID[connID]; ok {
		delete(t.tokens, old)
	}
	t.tokens[token] = &udpPendingToken{connID: connID, expireAt: time.Now().Add(ttl).UnixMilli()}
	t.tokenByConnID[connID] = token
	return token, nil
}

// consumeToken 一次性消费：先删后验，重复 ASSOCIATE 同 token 拿不到（防重放）；
// 过期 token 同样拒绝（消费即失效，不留残值）。
func (t *udpAssocTable) consumeToken(token string, now int64) (string, bool) {
	if token == "" {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.tokens[token]
	if !ok {
		return "", false // 未知 / 已被消费
	}
	delete(t.tokens, token)
	if t.tokenByConnID[p.connID] == token {
		delete(t.tokenByConnID, p.connID)
	}
	if now > p.expireAt {
		return "", false
	}
	return p.connID, true
}

// create 建立关联：per-IP 与总量上限检查 → 幂等替换同会话旧关联 → 双索引登记。
// 超限返回 nil（调用方静默丢，不回错误帧）。上限由调用方从实例 cfg 热读传入
// （不读全局 config.Get：热更走 gw.cfg，且测试可注入）。
func (t *udpAssocTable) create(connID string, addr net.Addr, udpConn gnet.Conn, token string, now int64, maxPerIP, maxAssoc int) *udpAssociation {
	ip := netutil.AddrHost(addr)
	addrKey := addr.String()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.perIP[ip] >= maxPerIP {
		return nil
	}
	if t.count >= maxAssoc {
		return nil
	}
	// 幂等替换：同会话重复 ASSOCIATE 先拆旧索引，避免 byAddr 残留旧地址
	if old, ok := t.byConnID.Load(connID); ok {
		t.removeLocked(connID, old.(*udpAssociation))
	}
	a := &udpAssociation{
		connID:     connID,
		addrKey:    addrKey,
		publicAddr: cloneUDPAddr(addr),
		ip:         ip,
		udpConn:    udpConn,
		token:      token,
		seqBase:    randomSeqBase(),
		createdAt:  now,
	}
	a.lastSeen.Store(now)
	t.byConnID.Store(connID, a)
	t.byAddr.Store(addrKey, connID)
	t.perIP[ip]++
	t.count++
	return a
}

// removeLocked 拆除一个已确认存在的关联（调用方须持 t.mu）。
func (t *udpAssocTable) removeLocked(connID string, a *udpAssociation) {
	t.byConnID.Delete(connID)
	t.byAddr.Delete(a.addrKey)
	if t.perIP[a.ip] > 1 {
		t.perIP[a.ip]--
	} else {
		delete(t.perIP, a.ip)
	}
	t.count--
}

// deleteByConnID 按会话删除关联（OnClose/RemoveConnection 兜底，幂等）。
func (t *udpAssocTable) deleteByConnID(connID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	old, ok := t.byConnID.Load(connID)
	if !ok {
		return false
	}
	t.removeLocked(connID, old.(*udpAssociation))
	return true
}

// lookupByAddr 收包热路径反查：无锁读，未关联返回 false（调用方静默丢）。
// 并发删除窗口内可能返回已拆索引的关联——发送会失败并被丢弃，无副作用。
func (t *udpAssocTable) lookupByAddr(addrKey string) (*udpAssociation, bool) {
	connID, ok := t.byAddr.Load(addrKey)
	if !ok {
		return nil, false
	}
	v, ok := t.byConnID.Load(connID)
	if !ok {
		return nil, false
	}
	return v.(*udpAssociation), true
}

// lookupByConnID 按会话反查关联（下行 SendUDP 用，无锁读）。
// 未关联返回 false，调用方回落 WS。
func (t *udpAssocTable) lookupByConnID(connID string) (*udpAssociation, bool) {
	v, ok := t.byConnID.Load(connID)
	if !ok {
		return nil, false
	}
	return v.(*udpAssociation), true
}

// sweep 淘汰空闲关联（NAT 过期后关联无意义）与过期 token，返回删除的关联数。
// onIdleRemoved 对每个被空闲淘汰的关联回调（持有表锁内调用，回调只做无锁读——
// §8.4 埋点：淘汰时 WS 会话仍存活 = stale 关联缺口签名；nil 表示不关心）。
func (t *udpAssocTable) sweep(now, idleMillis int64, onIdleRemoved func(connID string)) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	removed := 0
	t.byConnID.Range(func(key, value any) bool {
		a := value.(*udpAssociation)
		if now-a.lastSeen.Load() > idleMillis {
			connID := key.(string)
			if onIdleRemoved != nil {
				onIdleRemoved(connID)
			}
			t.removeLocked(connID, a)
			removed++
		}
		return true
	})
	for tok, p := range t.tokens {
		if now > p.expireAt {
			delete(t.tokens, tok)
			if t.tokenByConnID[p.connID] == tok {
				delete(t.tokenByConnID, p.connID)
			}
		}
	}
	return removed
}

// snapshot 返回当前关联数/待消费 token 数（/stats 与测试观测）。
func (t *udpAssocTable) snapshot() (assoc, tokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count, len(t.tokens)
}

// cloneUDPAddr 防御拷贝：gnet release 会把 remoteAddr 的 Zone 归还对象池，
// 关联长期持有回程目标，不能与 gnet 内部别名共享底层。
func cloneUDPAddr(a net.Addr) net.Addr {
	if u, ok := a.(*net.UDPAddr); ok {
		ip := make(net.IP, len(u.IP))
		copy(ip, u.IP)
		return &net.UDPAddr{IP: ip, Port: u.Port, Zone: u.Zone}
	}
	return a
}

// seqDuplicate 上行 seq 滑动窗口去重：返回 true = 重复或过旧（应静默丢弃）。
// 位图位 i 对应 seq = seqMax - i，窗口 = 字长×64（首包按 config.seqWindow
// 向上取整 64 倍数分配一次，之后热更不重排窗口）。
// seq <= 0 视为客户端未携带序号（向后兼容不设 seq 的客户端），不参与去重。
// 仅由收包路径调用；正常同源地址包在同一 event loop 串行，seqMu 防御并发。
func (a *udpAssociation) seqDuplicate(seq int64, window int) bool {
	if seq <= 0 {
		return false
	}
	a.seqMu.Lock()
	defer a.seqMu.Unlock()
	if a.seqWords == nil {
		if window < 64 {
			window = 64
		}
		a.seqWords = make([]uint64, (window+63)/64)
		a.seqMax = seq
		a.seqWords[0] = 1 // 本包已见（位 0 = seqMax）
		return false
	}
	bits := uint64(len(a.seqWords) * 64)
	if seq > a.seqMax {
		a.shlSeqWindow(uint(seq - a.seqMax))
		a.seqMax = seq
		a.seqWords[0] |= 1
		return false
	}
	gap := uint64(a.seqMax - seq)
	if gap >= bits {
		return true // 窗口外的旧包：无法判定，按过旧丢弃（乱序幅度超窗口视为无效）
	}
	if a.seqWords[gap/64]&(1<<(gap%64)) != 0 {
		return true // 已见（重放/重复）
	}
	a.seqWords[gap/64] |= 1 << (gap % 64)
	return false
}

// shlSeqWindow 位图整体前移 n 位（更高下标 = 更旧的 seq；滑出窗口的位丢弃）。
// 调用方须持 seqMu。
func (a *udpAssociation) shlSeqWindow(n uint) {
	words := len(a.seqWords)
	if n >= uint(words*64) {
		for i := range a.seqWords {
			a.seqWords[i] = 0
		}
		return
	}
	wordShift := int(n / 64)
	bitShift := n % 64
	for i := words - 1; i >= 0; i-- {
		var v uint64
		if src := i - wordShift; src >= 0 {
			v = a.seqWords[src] << bitShift
			if bitShift > 0 && src > 0 {
				v |= a.seqWords[src-1] >> (64 - bitShift)
			}
		}
		a.seqWords[i] = v
	}
}

// randomSeqBase 关联时分配的上行 seq 基线（crypto/rand 63 位，客户端从该值递增）。
func randomSeqBase() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0 // 极端情形退化为固定基线，不影响安全性（仅去重起点）
	}
	return int64(binary.BigEndian.Uint64(b[:]) &^ (1 << 63))
}

// udpEnabled 热读 UDP 开关（配置热更即时生效；默认 false = 预留通道现状）。
func (g *Gateway) udpEnabled() bool {
	cfg := g.cfg.Load()
	return cfg != nil && cfg.UDP.Enabled
}

// udpAssocLimits 热读关联上限（默认值兜底测试直造的零值配置）。
func (g *Gateway) udpAssocLimits() (maxPerIP, maxAssoc int) {
	maxPerIP, maxAssoc = config.DefaultUDPMaxPerIP, config.DefaultUDPMaxAssociations
	if cfg := g.cfg.Load(); cfg != nil {
		if cfg.UDP.MaxPerIP > 0 {
			maxPerIP = cfg.UDP.MaxPerIP
		}
		if cfg.UDP.MaxAssociations > 0 {
			maxAssoc = cfg.UDP.MaxAssociations
		}
	}
	return maxPerIP, maxAssoc
}

// udpSeqWindow 热读上行 seq 去重窗口大小。
func (g *Gateway) udpSeqWindow() int {
	if cfg := g.cfg.Load(); cfg != nil && cfg.UDP.SeqWindow > 0 {
		return cfg.UDP.SeqWindow
	}
	return config.DefaultUDPSeqWindow
}

// udpMaxDatagram 热读单数据报上限（MTU 安全值，设计 §5.6）。
func (g *Gateway) udpMaxDatagram() int {
	if cfg := g.cfg.Load(); cfg != nil && cfg.UDP.MaxDatagram > 0 {
		return cfg.UDP.MaxDatagram
	}
	return config.DefaultUDPMaxDatagram
}

// udpAssociateTTL 热读一次性 token 有效期。
func (g *Gateway) udpAssociateTTL() time.Duration {
	if cfg := g.cfg.Load(); cfg != nil && cfg.UDP.AssociateTimeout != "" {
		if d, err := time.ParseDuration(cfg.UDP.AssociateTimeout); err == nil && d > 0 {
			return d
		}
	}
	return config.DefaultUDPAssociateDuration
}

// udpListenPort 返回 type=udp 的 transport 端口；未配置返回 0（= 不下发 token）。
func udpListenPort(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	for _, t := range cfg.Transports {
		if strings.EqualFold(t.Type, "udp") {
			return t.Port
		}
	}
	return 0
}

// issueUDPToken 登录成功时生成一次性关联 token（WS 成功 ack 专用）。
// 未启用 / 无 UDP 监听时返回 ("", 0, false)，ack 不带 UDP 字段（向后兼容）。
func (g *Gateway) issueUDPToken(connID string) (string, int, bool) {
	if !g.udpEnabled() {
		return "", 0, false
	}
	cfg := g.cfg.Load()
	port := udpListenPort(cfg)
	if port <= 0 {
		return "", 0, false
	}
	token, err := g.udpAssoc.issueToken(connID, g.udpAssociateTTL())
	if err != nil {
		tlog.Warn(g.ctx, "issue udp token failed connectionID=%s error=%v", connID, err)
		return "", 0, false
	}
	return token, port, true
}

// releaseUDPAssoc 会话结束时拆除关联（幂等；OnClose 与 RemoveConnection 两路都调）。
func (g *Gateway) releaseUDPAssoc(connID string) {
	if connID == "" {
		return
	}
	if g.udpAssoc.deleteByConnID(connID) {
		tlog.Debug(g.ctx, "udp assoc released connectionID=%s", connID)
	}
}

// udpIdleDuration 热读空闲淘汰时长（janitor 清扫与下行新鲜度条件共用同一配置源）。
func (g *Gateway) udpIdleDuration() time.Duration {
	if cfg := g.cfg.Load(); cfg != nil && cfg.UDP.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.UDP.IdleTimeout); err == nil && d > 0 {
			return d
		}
	}
	return config.DefaultUDPIdleDuration
}

// udpAssocSweepOnce 执行一轮空闲清扫（janitor 与测试共用入口）。
// §8.4 埋点：关联网关会话仍存活的关联被空闲淘汰 = stale 关联缺口签名
// （NAT 重绑/UDP 静默但 WS 还活着），计数 udpAssocSweptWSSurviving。
func (g *Gateway) udpAssocSweepOnce(now int64, idle time.Duration) int {
	return g.udpAssoc.sweep(now, idle.Milliseconds(), func(connID string) {
		if g.connectionManager.GetConnection(connID) != nil {
			g.udpAssocSweptWSSurviving.Add(1)
		}
	})
}

// udpAssocJanitor 周期淘汰空闲关联与过期 token。
// 无条件启动：空表扫描开销可忽略，且不依赖 udp.enabled 状态（热更开启时
// 清扫已在运行）；单轮异常由 panicutil.GoLoop 退避重启。
func (g *Gateway) udpAssocJanitor() {
	for {
		idle := g.udpIdleDuration()
		interval := idle / 2
		if interval < time.Second {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-g.stopChan:
			timer.Stop()
			return
		case <-timer.C:
		}

		now := time.Now().UnixMilli()
		if n := g.udpAssocSweepOnce(now, idle); n > 0 {
			tlog.Info(g.ctx, "udp assoc idle sweep removed=%d idleTimeout=%s sweptWsSurviving=%d",
				n, idle, g.udpAssocSweptWSSurviving.Load())
		}
	}
}
