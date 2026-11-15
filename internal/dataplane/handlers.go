package dataplane

import (
	"fmt"
	"slices"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/security"
)

func (g *Engine) isLogicConnected() bool {
	if g.logicPool != nil && g.logicPool.IsConnected() {
		return true
	}
	if g.logicLink != nil && g.logicLink.IsConnected() {
		return true
	}
	return false
}

func (g *Engine) isPreAuthCommand(cmd int32) bool {
	// 逻辑登录命令是稳定的协议边界。将其作为内置回退保留，
	// 以便旧的动态配置在命令范围迁移后不会锁定所有新连接的会话。
	if cmd == protoGw.CmdLogicLoginReq {
		return true
	}
	return slices.Contains(g.GetProtection().PreAuthCommands, cmd)
}

func (g *Engine) GetLogicClient(serverID string) LogicClientProvider {
	if g.logicPool == nil {
		return nil
	}
	return g.logicPool.GetClient(serverID)
}

// HandleTCPFrame 处理一条完整 TCP 逻辑帧（transport.Runtime 实现，事件循环内调用）。
// 连接标识由 transport 侧从 ConnContext 解析后传入（调用点保证来自 feedFrames 回调）。
func (g *Engine) HandleTCPFrame(c gnet.Conn, connectionID string, data []byte) (action gnet.Action) {
	if len(data) == 0 {
		return
	}

	g.messagesReceived.Add(1)

	message, ok := protoGw.DecodeClientMessage(data)
	if !ok {
		return gnet.Close
	}
	// 入站流量刷新活跃时间（否则仅靠 Send 采样会被 connIdleTimeout 误杀）
	if connObj := g.connectionManager.GetConnection(connectionID); connObj != nil {
		connObj.TouchActive()
	}
	cmd := message.Cmd
	if cmd == protoGw.CmdLoginGate {
		// LoginGate 消息由 finishLoginGate 异步持有，不归还解码池。
		return g.handleLoginGate(c, connectionID, message)
	}
	if cmd == protoGw.CmdLogoutGate {
		action := g.handleLogoutGate(c, connectionID, message)
		protoGw.PutClientMessage(message)
		return action
	}

	// 异步路径：投递到 worker pool，event loop 立即返回
	if g.pipelineWorkerPool != nil {
		// 深拷贝 data，因为 FrameBuf 会被 event loop 复用（缓冲池化复用）
		dataCopy := getMsgBuf(len(data))
		copy(dataCopy, data)
		task := pipelineTaskPool.Get().(*pipelineTaskData)
		*task = pipelineTaskData{
			conn:         c,
			data:         dataCopy,
			message:      message,
			connectionID: connectionID,
			wsConn:       nil,
		}
		if !g.pipelineWorkerPool.Submit(task) {
			protoGw.PutClientMessage(task.message)
			putMsgBuf(task.data)
			pipelineTaskPool.Put(task)
		}
		return
	}

	// 同步路径：直接在 event loop 中处理
	result := g.pipeline.Process(c, data, message, connectionID)
	protoGw.PutClientMessage(message)
	if result.Error != nil {
		if result.ProtoMsg != nil {
			protoGw.PutStreamData(result.ProtoMsg)
		}
		_ = g.sendFrameSync(connectionID, protoGw.NewErrorFrame("error", result.Error.Error(), "", ""))
		return result.Action
	}
	return result.Action
}

// getOrCreateBreaker 获取或创建指定 route 的熔断器。
// 未启用（breakerParams 为 nil）或管理器缺失时返回 nil（调用方需判空）。
// 参数来自构造/热更时的快照，热路径不做配置解析。
func (g *Engine) getOrCreateBreaker(route string) *security.CircuitBreaker {
	if g.circuitBreakerMgr == nil {
		return nil
	}
	p := g.breakerParams.Load()
	if p == nil {
		return nil
	}
	return g.circuitBreakerMgr.GetCircuitBreaker(route, p.failureThreshold, p.successThreshold, p.timeout)
}

// ============================================================================
// 统一写出口（协议无关）
// ============================================================================

// sendFrameSync 在事件循环内同步写出原始帧（ConnectionID 索引连接）。
// 仅允许在 gnet 事件循环内调用（OnTraffic 等）；跨协程用 sendFrameAsync。
func (g *Engine) sendFrameSync(connectionID string, data []byte) error {
	conn := g.connectionManager.GetConnection(connectionID)
	if conn == nil {
		return fmt.Errorf("connection not found")
	}
	return conn.SendSync(data)
}

// sendMsgSync 在事件循环内同步写出 StreamData 消息（序列化 + 封帧）。
func (g *Engine) sendMsgSync(connectionID string, msg *protoGw.StreamData) error {
	data, err := protoGw.MarshalClientMessage(msg)
	if err != nil {
		return err
	}
	return g.sendFrameSync(connectionID, data)
}

// sendFrameAsync 跨协程异步写出原始帧（任意协程可调用）。
func (g *Engine) sendFrameAsync(connectionID string, data []byte) error {
	conn := g.connectionManager.GetConnection(connectionID)
	if conn == nil {
		return fmt.Errorf("connection not found")
	}
	return conn.Send(data)
}
