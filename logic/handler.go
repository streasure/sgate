//go:build legacy

package logic

import (
	"fmt"
	"reflect"
	"sync"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// Context is the business context for one Gateway StreamData request.
type Context struct {
	ConnectionID string
	UserUUID     string
	Server       *Server
	Msg          *protocol.StreamData
}

type ProtoHandler func(ctx *Context, req proto.Message) proto.Message

type protoEntry struct {
	reqType reflect.Type
	handler ProtoHandler
	respCmd int32
	reqPool sync.Pool
}

// RegisterProto registers a protobuf handler for exactly one command.
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
	tlog.Info("proto handler registered", "cmd", cmd, "reqType", rt.Name())
}

func (s *Server) dispatchMessage(msg *protocol.StreamData, callback func(*protocol.StreamData)) {
	value, ok := s.handlers.Load(msg.Cmd)
	if !ok {
		tlog.Warn("received unregistered cmd", "cmd", msg.Cmd, "sessionID", msg.SessionId)
		return
	}

	entry := value.(*protoEntry)
	req := entry.reqPool.Get().(proto.Message)
	defer entry.reqPool.Put(req)
	proto.Reset(req)
	if len(msg.Data) > 0 {
		if err := proto.Unmarshal(msg.Data, req); err != nil {
			tlog.Warn("failed to decode request", "cmd", msg.Cmd, "sessionID", msg.SessionId, "error", err)
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
		tlog.Error("failed to encode response", "cmd", msg.Cmd, "sessionID", msg.SessionId, "error", err)
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

func (s *Server) registeredCommands() []int32 {
	commands := make([]int32, 0)
	s.handlers.Range(func(key, _ any) bool {
		commands = append(commands, key.(int32))
		return true
	})
	return commands
}

func invalidControlPayload(cmd int32, err error) error {
	return fmt.Errorf("logic: marshal control command %d: %w", cmd, err)
}
