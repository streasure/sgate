package gateway

import (
	"strconv"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/types"
)

// buildFilterContext 从原始请求构造过滤器上下文。
// 填充连接已绑定的 UserUUID，使 Auth 阶段能区分「已登录帧」与「未登录帧」。
func (g *Gateway) buildFilterContext(c gnet.Conn, data []byte, connectionID string, cmd int32) *types.FilterContext {
	fc := &types.FilterContext{
		Ctx:          g.ctx,
		ConnectionID: connectionID,
		RemoteIP:     getRemoteIP(c),
		Cmd:          cmd,
		Data:         data,
		Metadata:     make(map[string]string),
	}
	if conn := g.connectionManager.GetConnection(connectionID); conn != nil && conn.IsAuthenticated() {
		fc.UserUUID = conn.GetUserUUID()
	}
	// Route 供 skipRoutes 匹配；协议帧无路由名，用 cmd 十进制串（与 skipRoutes 配置一致）
	fc.Route = strconv.Itoa(int(cmd))
	return fc
}

func (g *Gateway) applyForwardFilters(c gnet.Conn, data []byte, connectionID string, cmd int32, seqID int64) (*protoGw.StreamData, bool) {
	if g.filterChain == nil {
		return nil, true
	}
	fcx := g.buildFilterContext(c, data, connectionID, cmd)
	for phase := types.PhasePreAuth; phase <= types.PhaseForward; phase++ {
		if !g.filterChain.RunByPhase(phase, fcx) {
			g.messagesDroppedFilterChain.Add(1)
			return nil, false
		}
		if fcx.Abort {
			g.messagesDroppedFilterChain.Add(1)
			return nil, false
		}
	}
	// 镜像副作用标记
	if fcx.Mirrored && g.trafficMirror != nil {
		g.trafficMirror.Mirror(fcx)
	}
	// 持久化 JWT jti/exp 到连接，供 logout/封禁撤销
	if jti := fcx.Metadata["jwt.jti"]; jti != "" {
		if connObj := g.connectionManager.GetConnection(connectionID); connObj != nil {
			connObj.SetJWTJti(jti)
			if expStr := fcx.Metadata["jwt.exp"]; expStr != "" {
				if exp, err := strconv.ParseInt(expStr, 10, 64); err == nil {
					connObj.SetJWTExp(exp)
				}
			}
		}
	}
	// 过滤器可能改写 Data（如降级兜底），优先使用过滤后的数据
	payload := fcx.Data
	if len(payload) == 0 {
		payload = data
	}
	// 构造转发消息（允许过滤器修改 metadata）
	msg := &protoGw.StreamData{
		SessionId: connectionID,
		Data:      payload,
		Cmd:       cmd,
		SeqId:     seqID,
	}
	if fcx.UserUUID != "" {
		msg.UserKey = fcx.UserUUID
	}
	return msg, true
}
