package logic

import (
	"context"

	"reflect"
	"sync"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// Context 网关 StreamData 请求的业务上下文
type Context struct {
	ConnectionID string                    // 连接 ID（会话 ID）
	UserUUID     string                    // 用户 UUID
	Server       *Server                   // 逻辑层服务端引用
	Msg          *protocol.StreamData      // 原始流数据消息
}

// ProtoHandler protobuf 协议处理器函数类型
type ProtoHandler func(ctx *Context, req proto.Message) proto.Message

// protoEntry 协议处理器注册项，包含请求类型、处理函数和响应命令码
type protoEntry struct {
	reqType reflect.Type    // 请求 protobuf 消息类型
	handler ProtoHandler    // 处理函数
	respCmd int32           // 响应命令码
	reqPool sync.Pool       // 请求对象池，减少内存分配
}

// RegisterProto 注册单个命令码的 protobuf 处理器
func (s *Server) RegisterProto(cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) {
	if cmd == 0 {
		panic("logic: RegisterProto requires a non-zero cmd")
	}
	if reqProto == nil || reflect.TypeOf(reqProto).Kind() != reflect.Ptr {
		panic("logic: RegisterProto requires a non-nil protobuf pointer")
	}
	if handler == nil {
		panic("logic: RegisterProto requires a handler")
	}

	rt := reflect.TypeOf(reqProto).Elem()
	s.handlers.Store(cmd, &protoEntry{
		reqType: rt,
		handler: handler,
		respCmd: respCmd,
		reqPool: sync.Pool{New: func() any { return reflect.New(rt).Interface() }},
	})
	tlog.Info(context.Background(), "proto handler registered cmd=%d reqType=%s", cmd, rt.Name())
}

// dispatchMessage 根据命令码分发消息到注册的处理器
func (s *Server) dispatchMessage(msg *protocol.StreamData, callback func(*protocol.StreamData)) {
	value, ok := s.handlers.Load(msg.Cmd)
	if !ok {
		tlog.Warn(context.Background(), "received unregistered cmd cmd=%d sessionID=%s", msg.Cmd, msg.SessionId)
		return
	}

	entry := value.(*protoEntry)
	req := entry.reqPool.Get().(proto.Message)
	defer entry.reqPool.Put(req)
	proto.Reset(req)
	if len(msg.Data) > 0 {
		if err := proto.Unmarshal(msg.Data, req); err != nil {
			tlog.Warn(context.Background(), "failed to decode request cmd=%d sessionID=%s error=%v", msg.Cmd, msg.SessionId, err)
			return
		}
	}

	resp := entry.handler(&Context{
		ConnectionID: msg.SessionId,
		UserUUID:     msg.UserKey,
		Server:       s,
		Msg:          msg,
	}, req)
	if resp == nil || entry.respCmd == 0 {
		return
	}
	data, err := proto.Marshal(resp)
	if err != nil {
		tlog.Error(context.Background(), "failed to encode response cmd=%d sessionID=%s error=%v", msg.Cmd, msg.SessionId, err)
		return
	}

	userKey := msg.UserKey
	if keyed, ok := resp.(interface{ GetUserKey() string }); ok && keyed.GetUserKey() != "" {
		userKey = keyed.GetUserKey()
	}
	callback(&protocol.StreamData{
		SessionId: msg.SessionId,
		UserKey:   userKey,
		Cmd:       entry.respCmd,
		SeqId:     msg.SeqId,
		Data:      data,
		ClientIp:  msg.ClientIp,
	})
}

// registeredCommands 获取所有已注册的命令码列表
func (s *Server) registeredCommands() []int32 {
	commands := make([]int32, 0)
	s.handlers.Range(func(key, _ any) bool {
		commands = append(commands, key.(int32))
		return true
	})
	return commands
}


