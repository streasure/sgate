package gateway

import (
	"strconv"
	"sync"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/backend"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/netutil"
)

// FilterContext 与 Metadata 池化：热路径每消息一次上下文 + 一个 map。
// 归还前必须与过滤器链完全脱钩——in-repo 过滤器仅在 Process 内引用 fc，
// 镜像链路（TrafficMirror.Mirror）会深拷贝后入队；新增过滤器不得在
// Process 返回后继续持有 fc 或其字段。
var (
	filterContextPool = sync.Pool{New: func() any { return &types.FilterContext{} }}
	filterMetaPool    = sync.Pool{New: func() any { return make(map[string]string, 2) }}
)

// getFilterContext 从池中取出并重置过滤器上下文。
func getFilterContext() *types.FilterContext {
	fc := filterContextPool.Get().(*types.FilterContext)
	*fc = types.FilterContext{Metadata: filterMetaPool.Get().(map[string]string)}
	return fc
}

// putFilterContext 归还过滤器上下文（含 Metadata 清空入池）。
func putFilterContext(fc *types.FilterContext) {
	if fc == nil {
		return
	}
	if fc.Metadata != nil {
		clear(fc.Metadata)
		filterMetaPool.Put(fc.Metadata)
	}
	*fc = types.FilterContext{}
	filterContextPool.Put(fc)
}

// buildFilterContext 从原始请求构造过滤器上下文。
// 填充连接已绑定的 UserUUID，使 Auth 阶段能区分「已登录帧」与「未登录帧」。
// connObj/routeKey 由 pipeline 阶段2/阶段3 查得后传入：热路径每消息一次连接
// 查找与一次路由键计算，此处复用避免重复开销（同 ID 必得同一连接对象）。
// connObj 为 nil 时回退 RemoteAddr 解析（仅测试等直调场景）。
func (g *Gateway) buildFilterContext(c gnet.Conn, connObj *connection.Connection, routeKey string, data []byte, connectionID string, cmd int32) *types.FilterContext {
	fc := getFilterContext()
	fc.Ctx = g.ctx
	fc.ConnectionID = connectionID
	if connObj != nil {
		fc.RemoteIP = connObj.RemoteHost()
	} else {
		fc.RemoteIP = netutil.AddrHost(c.RemoteAddr())
	}
	fc.Cmd = cmd
	fc.Data = data
	fc.Route = routeKey
	if connObj != nil && connObj.IsAuthenticated() {
		fc.UserUUID = connObj.GetUserUUID()
	}
	return fc
}

func (g *Gateway) applyForwardFilters(c gnet.Conn, connObj *connection.Connection, routeKey string, data []byte, connectionID string, cmd int32, seqID int64) (*protoGw.StreamData, bool) {
	// 空链（未注册任何过滤器）轻量路径：跳过 FilterContext 构造、Phase 遍历与
	// 对象池全套开销，直接构造转发消息。Data 用入站池化缓冲拷贝而非引用——
	// 与非空链路径最终经 pipeline 阶段5.5 切断别名的结果一致，但省去每消息
	// 一次 make+copy 分配。UserKey/ClientIp 由 pipeline 阶段5 统一补齐。
	if g.filterChain == nil || g.filterChain.Len() == 0 {
		msg := backend.GetStreamData()
		msg.SessionId = connectionID
		msg.Data = append(msg.Data[:0], data...)
		msg.Cmd = cmd
		msg.SeqId = seqID
		return msg, true
	}
	fcx := g.buildFilterContext(c, connObj, routeKey, data, connectionID, cmd)
	for phase := types.PhasePreAuth; phase <= types.PhaseForward; phase++ {
		if !g.filterChain.RunByPhase(phase, fcx) {
			g.messagesDroppedFilterChain.Add(1)
			putFilterContext(fcx)
			return nil, false
		}
		if fcx.Abort {
			g.messagesDroppedFilterChain.Add(1)
			putFilterContext(fcx)
			return nil, false
		}
	}
	// 镜像副作用标记
	if fcx.Mirrored && g.trafficMirror != nil {
		g.trafficMirror.Mirror(fcx)
	}
	// 持久化 JWT jti/exp 到连接，供 logout/封禁撤销
	if jti := fcx.Metadata["jwt.jti"]; jti != "" {
		if connObj != nil {
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
	msg := backend.GetStreamData()
	msg.SessionId = connectionID
	msg.Data = payload
	msg.Cmd = cmd
	msg.SeqId = seqID
	if fcx.UserUUID != "" {
		msg.UserKey = fcx.UserUUID
	}
	// msg.Data 可能引用 fc.Data 的底层数组：归还仅清空结构体字段，
	// 不触碰字节缓冲，msg 生命周期独立（alias-break 仍由 pipeline 阶段5.5 兜底）。
	putFilterContext(fcx)
	return msg, true
}
