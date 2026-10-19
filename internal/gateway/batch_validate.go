package gateway

import (
	"context"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"

	protoLogin "github.com/streasure/protocol/loginserver"
	"github.com/streasure/util/tlog"
)

// startBatchValidateSweep 启动 BatchValidate 周期安全网（封禁收敛 P2）。
//
// 背景：mid-login 竞态（Login 成功→过门之间封禁）时踢人 no-op，玩家持有 ban 前
// token 可过门上线；会话缓存 TTL=24h 超长在线踢不中。sgate 登录成功后留存
// accountId+loginKey，本任务周期调 loginserver BatchValidateLoginTokens 重校验
// 全部在线会话，失效/被封禁的本地踢下线，彻底收口两类窗口。
//
// 约束：
//   - 仅 loginValidation.enabled=true 且 batchInterval>0 时运行（热读配置）
//   - 单次批量 ≤ batchSize（默认 200，loginserver 硬上限 500）
//   - 校验请求不带 sgate 上下文（loginserver 天然跳过会话写入）
//   - 单条 RPC 失败按 invalid 处理（fail-closed，下轮再确认）
//   - 踢人复用 kickConn：KICK_NOTIFY + 逻辑服下线 + 断连
func (g *Gateway) startBatchValidateSweep() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				tlog.Error(g.ctx, "batchValidate sweep panic recovered error=%v", r)
			}
		}()
		for {
			interval := g.batchValidateInterval()
			if interval <= 0 {
				// 关闭：短轮询等待热更新开启（避免 stopChan 竞态漏关）
				select {
				case <-g.stopChan:
					return
				case <-time.After(time.Second):
				}
				continue
			}
			select {
			case <-g.stopChan:
				return
			case <-time.After(interval):
			}
			g.runBatchValidateSweep()
		}
	}()
}

// batchValidateInterval 热读 batchInterval；enabled=false 或非法/零值返回 0（暂停）。
func (g *Gateway) batchValidateInterval() time.Duration {
	if !g.loginValidationEnabled() {
		return 0
	}
	raw := ""
	if cfg := g.cfg.Load(); cfg != nil {
		raw = cfg.LoginValidation.BatchInterval
	} else if cfg := config.Get(); cfg != nil {
		raw = cfg.LoginValidation.BatchInterval
	}
	return parseBatchInterval(raw)
}

// batchValidateSize 热读 batchSize；0/非法回退默认 200。
func (g *Gateway) batchValidateSize() int {
	if cfg := g.cfg.Load(); cfg != nil && cfg.LoginValidation.BatchSize > 0 {
		return cfg.LoginValidation.BatchSize
	}
	if cfg := config.Get(); cfg != nil && cfg.LoginValidation.BatchSize > 0 {
		return cfg.LoginValidation.BatchSize
	}
	return config.DefaultLoginValidationBatchSize
}

// batchSweepItem 收集到的待校验会话。
type batchSweepItem struct {
	accountID string
	loginKey  string
	conn      *connection.Connection
}

// runBatchValidateSweep 执行一轮：收集在线会话 → 分片批量校验 → 踢失效。
func (g *Gateway) runBatchValidateSweep() {
	defer func() {
		if r := recover(); r != nil {
			tlog.Error(g.ctx, "batch validate sweep panic recovered error=%v", r)
		}
	}()

	g.loginServerMu.RLock()
	client := g.loginServerClient
	g.loginServerMu.RUnlock()
	if client == nil {
		return
	}

	size := g.batchValidateSize()
	items := make([]batchSweepItem, 0, size)
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		aid := conn.GetAccountID()
		key := conn.GetLoginKey()
		if aid == "" || key == "" {
			return true
		}
		items = append(items, batchSweepItem{accountID: aid, loginKey: key, conn: conn})
		return true
	})
	if len(items) == 0 {
		return
	}

	var checked, kicked, rpcFailed int
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		batch := items[start:end]

		req := &protoLogin.BatchValidateLoginTokensReq{
			Items: make([]*protoLogin.ValidateLoginTokenReq, len(batch)),
		}
		for i, it := range batch {
			req.Items[i] = &protoLogin.ValidateLoginTokenReq{
				AccountId:  it.accountID,
				LoginToken: it.loginKey,
				// 不带 sgate 上下文：loginserver 跳过会话写入，纯校验
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ack, err := client.BatchValidateLoginTokens(ctx, req)
		cancel()
		if err != nil {
			rpcFailed += len(batch)
			tlog.Warn(g.ctx, "batch validate RPC failed batch=%d/%d size=%d error=%v",
				start/size+1, (len(items)+size-1)/size, len(batch), err)
			continue
		}
		valids := ack.GetValid()
		if len(valids) != len(batch) {
			rpcFailed += len(batch)
			tlog.Warn(g.ctx, "batch validate ack length mismatch want=%d got=%d",
				len(batch), len(valids))
			continue
		}
		for i, valid := range valids {
			checked++
			if valid {
				continue
			}
			// 二次确认连接仍在线且未换 token（批量校验期间可能已重登/断开）
			cur := batch[i].conn
			if cur.GetAccountID() != batch[i].accountID || cur.GetLoginKey() != batch[i].loginKey {
				continue
			}
			g.kickConn(cur, kickNotifyParams{
				reason:  "ban",
				code:    403,
				message: "session revoked",
			})
			kicked++
			tlog.Info(g.ctx, "batch validate kick accountId=%s conn=%s reason=ban",
				batch[i].accountID, cur.ID())
		}
	}

	if checked > 0 || kicked > 0 || rpcFailed > 0 {
		tlog.Info(g.ctx, "batch validate sweep done online=%d checked=%d kicked=%d rpcFailed=%d",
			len(items), checked, kicked, rpcFailed)
	}
}

// parseBatchInterval 解析周期字符串；空/0/非法返回 0（关闭）。
func parseBatchInterval(raw string) time.Duration {
	if raw == "" || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
