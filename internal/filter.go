package gateway

import (
	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/types"
)

// buildFilterContext 从原始请求构造过滤器上下文
func (g *Gateway) buildFilterContext(c gnet.Conn, data []byte, connectionID string, cmd int32) *types.FilterContext {
	fc := &types.FilterContext{
		Ctx:          g.ctx,
		ConnectionID: connectionID,
		RemoteIP:     getRemoteIP(c),
		Cmd:          cmd,
		Data:         data,
		Metadata:     make(map[string]string),
	}
	return fc
}

func (g *Gateway) applyForwardFilters(c gnet.Conn, data []byte, connectionID string, cmd int32) (*protoGw.StreamData, bool) {
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
			return nil, false
		}
	}
	// 镜像副作用标记
	if fcx.Mirrored && g.trafficMirror != nil {
		g.trafficMirror.Mirror(fcx)
	}
	// 构造转发消息（允许过滤器修改 metadata）
	msg := &protoGw.StreamData{
		SessionId: connectionID,
		Data:      data,
	}
	if fcx.UserUUID != "" {
		msg.UserKey = fcx.UserUUID
	}
	return msg, true
}
