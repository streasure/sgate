package logic

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// streamConn 表示与网关的 gRPC 流连接
type streamConn struct {
	stream     protocol.GatewayStream_OnDataServer // gRPC 流对象
	sendCh     chan *protocol.StreamData            // 发送通道
	done       chan struct{}                        // 流结束信号
	closed     atomic.Bool                         // 连接是否已关闭
	closeOnce  sync.Once                           // 确保只关闭一次
	gatewayID  string                              // 网关标识
	sessionMu  sync.Mutex                          // 会话列表互斥锁
	sessionIDs map[string]struct{}                 // 关联的会话 ID 集合
}

// newStreamConn 创建新的流连接，启动发送协程
func newStreamConn(stream protocol.GatewayStream_OnDataServer, size int, gatewayID string) *streamConn {
	if size <= 0 {
		size = 1024
	}
	c := &streamConn{
		stream: stream, sendCh: make(chan *protocol.StreamData, size), done: make(chan struct{}),
		gatewayID: gatewayID, sessionIDs: make(map[string]struct{}),
	}
	go func() {
		defer close(c.done)
		for msg := range c.sendCh {
			if err := c.stream.Send(msg); err != nil {
				return
			}
		}
	}()
	return c
}

// Send 向流连接发送消息，连接已关闭时返回错误
func (c *streamConn) Send(msg *protocol.StreamData) error {
	select {
	case c.sendCh <- msg:
		return nil
	case <-c.done:
		return fmt.Errorf("logic: gateway stream closed")
	}
}

// bindSession 将会话 ID 绑定到此连接
func (c *streamConn) bindSession(sessionID string) {
	c.sessionMu.Lock()
	c.sessionIDs[sessionID] = struct{}{}
	c.sessionMu.Unlock()
}

// Close 关闭流连接，确保只执行一次
func (c *streamConn) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.sendCh)
	})
}

// pushGroup 推送组，维护组内成员会话
type pushGroup struct {
	members map[string]struct{} // 组成员会话 ID 集合
}

// PushMetrics 推送操作统计指标，用于监控
type PushMetrics struct {
	TotalPushed     atomic.Int64 // 总推送成功数
	TotalFailed     atomic.Int64 // 总推送失败数
	GroupPushed     atomic.Int64 // 组推送成功数
	GroupFailed     atomic.Int64 // 组推送失败数
	BroadcastSent   atomic.Int64 // 广播成功数
	BroadcastFailed atomic.Int64 // 广播失败数
	RetryAttempts   atomic.Int64 // 重试次数
	ScheduledPushes atomic.Int64 // 定时推送次数
}

// GetSnapshot 获取当前监控指标的快照副本
func (m *PushMetrics) GetSnapshot() map[string]int64 {
	return map[string]int64{
		"totalPushed":     m.TotalPushed.Load(),
		"totalFailed":     m.TotalFailed.Load(),
		"groupPushed":     m.GroupPushed.Load(),
		"groupFailed":     m.GroupFailed.Load(),
		"broadcastSent":   m.BroadcastSent.Load(),
		"broadcastFailed": m.BroadcastFailed.Load(),
		"retryAttempts":   m.RetryAttempts.Load(),
		"scheduledPushes": m.ScheduledPushes.Load(),
	}
}

// Server 逻辑层服务端，管理网关流连接、会话、分组和消息分发
type Server struct {
	protocol.UnimplementedGatewayStreamServer
	handlers sync.Map // 命令码 -> *protoEntry，已注册的协议处理器

	streams  sync.Map // 流 ID -> *streamConn，所有活跃的网关流连接
	sessions sync.Map // 会话 ID -> *streamConn，会话到流的映射

	userSessions sync.Map // 用户 UUID -> 会话 ID，用户到会话的映射
	sessionUsers sync.Map // 会话 ID -> 用户 UUID，会话到用户的映射

	groupMu       sync.RWMutex                     // 分组操作互斥锁
	groups        map[string]*pushGroup            // 组 ID -> 推送组
	sessionGroups map[string]map[string]struct{}   // 会话 ID -> 所属组 ID 集合

	serverID     string               // 逻辑服务端标识
	streamSeq    atomic.Uint64        // 流连接序号生成器
	streamChSize int                  // 流发送通道大小
	stopOnce     sync.Once            // 确保只停止一次
	metrics      PushMetrics          // 推送监控指标
}

// ServerOption 服务端配置选项函数
type ServerOption func(*Server)

// WithServerID 设置服务端 ID
func WithServerID(serverID string) ServerOption { return func(s *Server) { s.serverID = serverID } }
// WithStreamChSize 设置流发送通道大小
func WithStreamChSize(n int) ServerOption       { return func(s *Server) { s.streamChSize = n } }

// NewServer 创建逻辑层服务端实例
func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		groups:        make(map[string]*pushGroup),
		sessionGroups: make(map[string]map[string]struct{}),
		streamChSize:  1024,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) GetServerID() string { return s.serverID }

// OnData 处理来自网关的流数据，按命令码分发到注册的处理器
func (s *Server) OnData(stream protocol.GatewayStream_OnDataServer) error {
	streamID := fmt.Sprintf("stream_%s_%d", s.serverID, s.streamSeq.Add(1))
	gatewayID := streamID
	if values := metadata.ValueFromIncomingContext(stream.Context(), "sgate-gateway-id"); len(values) > 0 && values[0] != "" {
		gatewayID = values[0]
	}
	conn := newStreamConn(stream, s.streamChSize, gatewayID)
	s.streams.Store(streamID, conn)
	defer func() {
		s.streams.Delete(streamID)
		conn.Close()
		<-conn.done
		conn.sessionMu.Lock()
		for sessionID := range conn.sessionIDs {
			s.sessions.CompareAndDelete(sessionID, conn)
			if userUUID, ok := s.sessionUsers.LoadAndDelete(sessionID); ok {
				s.userSessions.CompareAndDelete(userUUID.(string), sessionID)
			}
		}
		conn.sessionMu.Unlock()
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if msg.SessionId != "" {
			s.sessions.Store(msg.SessionId, conn)
			conn.bindSession(msg.SessionId)
			if msg.UserKey != "" {
				s.RegisterUser(msg.UserKey, msg.SessionId)
			}
		}
		s.dispatchMessage(msg, func(response *protocol.StreamData) {
			if err := conn.Send(response); err != nil {
				tlog.Warn(context.Background(), "failed to queue response cmd=%d sessionID=%s error=%v", response.Cmd, response.SessionId, err)
			}
		})
	}
}

// SendMessage 处理单条消息的同步分发
func (s *Server) SendMessage(_ context.Context, msg *protocol.StreamData) (*protocol.StreamData, error) {
	var response *protocol.StreamData
	s.dispatchMessage(msg, func(resp *protocol.StreamData) { response = resp })
	return response, nil
}

// PushToConnection 向指定会话推送业务消息
func (s *Server) PushToConnection(sessionID string, targetCmd int32, data []byte) error {
	value, ok := s.sessions.Load(sessionID)
	if !ok {
		s.metrics.TotalFailed.Add(1)
		return fmt.Errorf("logic: session %q not found", sessionID)
	}
	err := value.(*streamConn).Send(&protocol.StreamData{SessionId: sessionID, Cmd: targetCmd, Data: data})
	if err != nil {
		s.metrics.TotalFailed.Add(1)
	} else {
		s.metrics.TotalPushed.Add(1)
	}
	return err
}

// RegisterUser 注册用户与会话的双向映射
func (s *Server) RegisterUser(userUUID, sessionID string) {
	if userUUID == "" || sessionID == "" {
		return
	}
	if old, ok := s.userSessions.Swap(userUUID, sessionID); ok && old.(string) != sessionID {
		s.sessionUsers.Delete(old.(string))
	}
	if old, ok := s.sessionUsers.Swap(sessionID, userUUID); ok && old.(string) != userUUID {
		s.userSessions.Delete(old.(string))
	}
}

// UnregisterUser 注销用户与会话的映射关系
func (s *Server) UnregisterUser(userUUID string) {
	if sessionID, ok := s.userSessions.LoadAndDelete(userUUID); ok {
		s.sessionUsers.CompareAndDelete(sessionID.(string), userUUID)
	}
}

// GetConnectionIDByUser 根据用户 UUID 获取会话 ID
func (s *Server) GetConnectionIDByUser(userUUID string) (string, bool) {
	value, ok := s.userSessions.Load(userUUID)
	if !ok {
		return "", false
	}
	return value.(string), true
}

// JoinGroup 将会话加入指定组，返回组当前成员数
func (s *Server) JoinGroup(groupID, sessionID string) int {
	if groupID == "" || sessionID == "" {
		return 0
	}
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	group := s.groups[groupID]
	if group == nil {
		group = &pushGroup{members: make(map[string]struct{})}
		s.groups[groupID] = group
	}
	group.members[sessionID] = struct{}{}
	if s.sessionGroups[sessionID] == nil {
		s.sessionGroups[sessionID] = make(map[string]struct{})
	}
	s.sessionGroups[sessionID][groupID] = struct{}{}
	return len(group.members)
}

// LeaveGroup 将会话移出指定组，返回组剩余成员数
func (s *Server) LeaveGroup(groupID, sessionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	group := s.groups[groupID]
	if group == nil {
		return 0
	}
	delete(group.members, sessionID)
	if len(group.members) == 0 {
		delete(s.groups, groupID)
	}
	if memberships := s.sessionGroups[sessionID]; memberships != nil {
		delete(memberships, groupID)
		if len(memberships) == 0 {
			delete(s.sessionGroups, sessionID)
		}
	}
	return len(group.members)
}

// GetGroupMembers 获取指定组的所有成员会话 ID 列表
func (s *Server) GetGroupMembers(groupID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()
	group := s.groups[groupID]
	if group == nil {
		return nil
	}
	members := make([]string, 0, len(group.members))
	for sessionID := range group.members {
		members = append(members, sessionID)
	}
	return members
}

// GetGroupCount 获取指定组的成员数量
func (s *Server) GetGroupCount(groupID string) int { return len(s.GetGroupMembers(groupID)) }

// leaveAllGroups 将会话从所有组中移除
func (s *Server) leaveAllGroups(sessionID string) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	for groupID := range s.sessionGroups[sessionID] {
		group := s.groups[groupID]
		delete(group.members, sessionID)
		if len(group.members) == 0 {
			delete(s.groups, groupID)
		}
	}
	delete(s.sessionGroups, sessionID)
}

// Offline 清除与客户端关联的用户、组和会话状态
func (s *Server) Offline(sessionID, userUUID string) {
	if value, ok := s.sessions.Load(sessionID); ok {
		s.sessions.CompareAndDelete(sessionID, value)
	}
	s.leaveAllGroups(sessionID)
	if userUUID != "" {
		s.UnregisterUser(userUUID)
		return
	}
	if value, ok := s.sessionUsers.LoadAndDelete(sessionID); ok {
		s.userSessions.CompareAndDelete(value.(string), sessionID)
	}
}

// sendRawControl 向所有网关连接发送控制消息
func (s *Server) sendRawControl(cmd int32, data []byte) int {
	count := 0
	sent := make(map[string]struct{})
	s.streams.Range(func(_, value any) bool {
		conn := value.(*streamConn)
		if _, ok := sent[conn.gatewayID]; ok {
			return true
		}
		// 先记录 gatewayID 防止同一网关多流重复发送
		sent[conn.gatewayID] = struct{}{}
		if conn.Send(&protocol.StreamData{Cmd: cmd, Data: data}) == nil {
			count++
		}
		return true
	})
	return count
}

// SendToUser 根据用户 UUID 查找连接并直接发送消息
func (s *Server) SendToUser(userUUID string, targetCmd int32, data []byte) int {
	if sessionID, ok := s.GetConnectionIDByUser(userUUID); ok {
		if s.PushToConnection(sessionID, targetCmd, data) == nil {
			return 1
		}
	}
	return 0
}

// Kick 发送踢下线通知
func (s *Server) Kick(sessionID string, args ...any) int {
	var targetCmd int32
	var data []byte
	if len(args) == 2 {
		if cmd, ok := args[0].(int32); ok {
			targetCmd = cmd
		}
		if d, ok := args[1].([]byte); ok {
			data = d
		}
	}
	if targetCmd == 0 {
		targetCmd = 1100012 // 用户下线通知命令。
	}
	return s.sendRawControl(targetCmd, data)
}

// SendToGroup 向组内所有成员发送消息
func (s *Server) SendToGroup(groupID string, targetCmd int32, data []byte) int {
	members := s.GetGroupMembers(groupID)
	sent := 0
	for _, sessionID := range members {
		if s.PushToConnection(sessionID, targetCmd, data) == nil {
			sent++
			s.metrics.GroupPushed.Add(1)
		} else {
			s.metrics.GroupFailed.Add(1)
		}
	}
	return sent
}

// Broadcast 向所有活跃会话广播消息
func (s *Server) Broadcast(targetCmd int32, data []byte) int {
	sent := 0
	s.sessions.Range(func(key, _ any) bool {
		if s.PushToConnection(key.(string), targetCmd, data) == nil {
			sent++
			s.metrics.BroadcastSent.Add(1)
		} else {
			s.metrics.BroadcastFailed.Add(1)
		}
		return true
	})
	return sent
}

// mustMarshal 将 protobuf 消息序列化为字节数组，失败时返回 nil
func mustMarshal(message proto.Message) []byte {
	data, err := proto.Marshal(message)
	if err != nil {
		return nil
	}
	return data
}

// RegisterGatewayStreamServer 将逻辑服务端注册为网关流服务实现
func (s *Server) RegisterGatewayStreamServer(grpcServer *grpc.Server) {
	protocol.RegisterGatewayStreamServer(grpcServer, s)
}

// GetConnectionCount 获取当前活跃连接数
func (s *Server) GetConnectionCount() int {
	count := 0
	s.sessions.Range(func(_, _ any) bool { count++; return true })
	return count
}

// SendToGroupWithRetry 向组内所有成员发送消息，失败时重试
// 返回成功发送数量和仍失败的会话 ID 列表
func (s *Server) SendToGroupWithRetry(groupID string, targetCmd int32, data []byte, maxRetries int) (int, []string) {
	members := s.GetGroupMembers(groupID)
	failedSessions := make([]string, 0)
	for _, sessionID := range members {
		if err := s.PushToConnection(sessionID, targetCmd, data); err != nil {
			failedSessions = append(failedSessions, sessionID)
		}
	}
	for retry := 0; retry < maxRetries && len(failedSessions) > 0; retry++ {
		time.Sleep(10 * time.Millisecond)
		s.metrics.RetryAttempts.Add(1)
		var stillFailed []string
		for _, sessionID := range failedSessions {
			if err := s.PushToConnection(sessionID, targetCmd, data); err != nil {
				stillFailed = append(stillFailed, sessionID)
			}
		}
		failedSessions = stillFailed
	}
	return len(members) - len(failedSessions), failedSessions
}

// JoinGroupForUser 将用户加入组（通过用户 UUID），用户必须已通过 RegisterUser 注册会话
func (s *Server) JoinGroupForUser(userUUID, groupID string) error {
	sessionID, ok := s.GetConnectionIDByUser(userUUID)
	if !ok {
		return fmt.Errorf("logic: user %q not found", userUUID)
	}
	s.JoinGroup(groupID, sessionID)
	return nil
}

// LeaveGroupForUser 将用户从组中移除（通过用户 UUID）
func (s *Server) LeaveGroupForUser(userUUID, groupID string) error {
	sessionID, ok := s.GetConnectionIDByUser(userUUID)
	if !ok {
		return fmt.Errorf("logic: user %q not found", userUUID)
	}
	s.LeaveGroup(groupID, sessionID)
	return nil
}

// ScheduledPush 定时推送任务，可定期向组推送消息
type ScheduledPush struct {
	GroupID  string        // 目标组 ID
	Cmd      int32         // 命令码
	Data     []byte        // 消息数据
	Interval time.Duration // 推送间隔
	MaxCount int           // 最大推送次数（0 = 无限）
	stopCh   chan struct{} // 停止信号
	server   *Server      // 服务端引用
	done     chan struct{} // 任务结束信号
}

// ScheduleGroupPush 启动定时组推送任务，返回可停止的句柄
func (s *Server) ScheduleGroupPush(groupID string, cmd int32, data []byte, interval time.Duration, maxCount int) *ScheduledPush {
	sp := &ScheduledPush{
		GroupID:  groupID,
		Cmd:      cmd,
		Data:     data,
		Interval: interval,
		MaxCount: maxCount,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		server:   s,
	}
	go sp.run()
	return sp
}

// run 定时推送任务的执行循环
func (sp *ScheduledPush) run() {
	defer close(sp.done)
	ticker := time.NewTicker(sp.Interval)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-sp.stopCh:
			return
		case <-ticker.C:
			sp.server.SendToGroup(sp.GroupID, sp.Cmd, sp.Data)
			sp.server.metrics.ScheduledPushes.Add(1)
			count++
			if sp.MaxCount > 0 && count >= sp.MaxCount {
				return
			}
		}
	}
}

// Stop 停止定时推送任务并等待协程结束
func (sp *ScheduledPush) Stop() {
	close(sp.stopCh)
	<-sp.done
}

// Stop 关闭所有流连接，停止服务端
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.streams.Range(func(_, value any) bool {
			conn := value.(*streamConn)
			conn.Close()
			return true
		})
	})
}
