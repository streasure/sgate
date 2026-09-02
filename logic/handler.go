package logic

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/streasure/sgate/protobuf"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

type Context struct {
	ConnectionID string
	UserUUID     string
	Server       *Server
	Msg          *protobuf.Message
}

type ProtoHandler func(ctx *Context, req proto.Message) proto.Message

type RouteHandler func(msg *protobuf.Message) *protobuf.Message

// BurstRouteHandler 允许单次触发推送多条响应消息，用于压测反向链路吞吐量。
// push 回调可被调用任意次数，每次调用发送一条消息回客户端。
type BurstRouteHandler func(msg *protobuf.Message, push func(*protobuf.Message))

type cmdEntry struct {
	reqType  reflect.Type
	handler  ProtoHandler
	respCmd  int32
	reqPool  sync.Pool
	respPool sync.Pool
}

type Dispatcher struct {
	route    string
	handlers sync.Map
}

func NewDispatcher(route string) *Dispatcher {
	return &Dispatcher{route: route}
}

func (d *Dispatcher) Handle(cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) *Dispatcher {
	rt := reflect.TypeOf(reqProto).Elem()
	d.handlers.Store(cmd, &cmdEntry{
		reqType: rt,
		handler: handler,
		respCmd: respCmd,
		reqPool: sync.Pool{New: func() interface{} { return reflect.New(rt).Interface() }},
	})
	tlog.Info("dispatcher cmd registered", "route", d.route, "cmd", cmd, "reqType", rt.Name())
	return d
}

func (d *Dispatcher) HandleFromProto(reqProto proto.Message, handler ProtoHandler) *Dispatcher {
	cmdVal, respCmdVal := protobuf.CmdFromProto(d.route, reqProto)
	return d.Handle(cmdVal, reqProto, respCmdVal, handler)
}

func (d *Dispatcher) dispatch(ctx *Context, msg *protobuf.Message, callback func(*protobuf.Message)) {
	val, ok := d.handlers.Load(msg.Cmd)
	if !ok {
		callback(errorReply(msg.ConnectionId, msg.Cmd, "Cmd not found in dispatcher", "404",
			"route", d.route, "cmd", fmt.Sprintf("%d", msg.Cmd)))
		return
	}
	entry := val.(*cmdEntry)
	reqVal := entry.reqPool.Get().(proto.Message)

	if len(msg.Data) > 0 {
		proto.Reset(reqVal)
		if err := proto.Unmarshal(msg.Data, reqVal); err != nil {
			entry.reqPool.Put(reqVal)
			callback(errorReply(msg.ConnectionId, msg.Cmd, "Failed to decode request", "400", "details", err.Error()))
			return
		}
	}

	resp := entry.handler(ctx, reqVal)
	entry.reqPool.Put(reqVal)

	respData, err := proto.Marshal(resp)
	if err != nil {
		callback(errorReply(msg.ConnectionId, msg.Cmd, "Failed to encode response", "500"))
		return
	}

	callback(&protobuf.Message{
		ConnectionId: msg.ConnectionId,
		UserUuid:     msg.UserUuid,
		Route:        msg.Route,
		Cmd:          entry.respCmd,
		Data:         respData,
	})
}

type protoEntry struct {
	reqType reflect.Type
	handler ProtoHandler
	respCmd int32
	reqPool sync.Pool
}

func (s *Server) RegisterProto(route string, cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) {
	key := routeKey(route, cmd)
	rt := reflect.TypeOf(reqProto).Elem()
	s.routes.Store(key, &protoEntry{
		reqType: rt,
		handler: handler,
		respCmd: respCmd,
		reqPool: sync.Pool{New: func() interface{} { return reflect.New(rt).Interface() }},
	})
	tlog.Info("proto registered", "route", route, "cmd", cmd, "reqType", rt.Name())
}

func (s *Server) RegisterDispatcher(d *Dispatcher) {
	s.routes.Store(d.route, d)
	tlog.Info("dispatcher registered", "route", d.route)
}

func (s *Server) RegisterRoute(route string, handler RouteHandler) {
	s.routes.Store(route, handler)
	tlog.Info("route registered", "route", route)
}

func (s *Server) RegisterBurstRoute(route string, handler BurstRouteHandler) {
	s.routes.Store(route, handler)
	tlog.Info("burst route registered", "route", route)
}

func routeKey(route string, cmd int32) string {
	if cmd == 0 {
		return route
	}
	return fmt.Sprintf("%s:%d", route, cmd)
}

func (s *Server) dispatchMessage(msg *protobuf.Message, callback func(*protobuf.Message)) {
	if msg.Route == "" {
		callback(errorReply(msg.ConnectionId, 0, "Missing route", "400"))
		return
	}

	val, ok := s.routes.Load(msg.Route)
	if !ok {
		val, ok = s.routes.Load(routeKey(msg.Route, msg.Cmd))
	}
	if !ok {
		callback(errorReply(msg.ConnectionId, msg.Cmd, "Route not found", "404", "details", msg.Route))
		return
	}

	switch entry := val.(type) {
	case *Dispatcher:
		entry.dispatch(&Context{
			ConnectionID: msg.ConnectionId,
			UserUUID:     msg.UserUuid,
			Server:       s,
			Msg:          msg,
		}, msg, callback)

	case *protoEntry:
		reqVal := entry.reqPool.Get().(proto.Message)

		if len(msg.Data) > 0 {
			proto.Reset(reqVal)
			if err := proto.Unmarshal(msg.Data, reqVal); err != nil {
				entry.reqPool.Put(reqVal)
				callback(errorReply(msg.ConnectionId, msg.Cmd, "Failed to decode request", "400", "details", err.Error()))
				return
			}
		}

		resp := entry.handler(&Context{
			ConnectionID: msg.ConnectionId,
			UserUUID:     msg.UserUuid,
			Server:       s,
			Msg:          msg,
		}, reqVal)
		entry.reqPool.Put(reqVal)

		respData, err := proto.Marshal(resp)
		if err != nil {
			callback(errorReply(msg.ConnectionId, msg.Cmd, "Failed to encode response", "500"))
			return
		}

		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			UserUuid:     msg.UserUuid,
			Route:        msg.Route,
			Cmd:          entry.respCmd,
			Data:         respData,
		})

	case RouteHandler:
		callback(entry(msg))
	case BurstRouteHandler:
		// 发送单条响应消息，由 flushLoop 批量打包后再 stream.Send。
		// 旧实现将所有 burst 响应预打包为单个 RouteBatch 再 callback 一次，
		// 导致 BURST_COUNT=1 时每条正向消息产生一次 stream.Send（反向 QPS 瓶颈）。
		// 现改为每条响应单独 callback，flushLoop 可将最多 256 条合并为一次 stream.Send，
		// 将 gRPC Send 调用数降低约 256 倍。
		// BURST_COUNT>1 时，相同 Route+Timestamp 的消息会在 flushLoop 的 marshal 缓存中命中，
		// 避免重复 Marshal。
		entry(msg, func(response *protobuf.Message) {
			if response == nil {
				return
			}
			if response.ConnectionId == "" {
				response.ConnectionId = msg.ConnectionId
			}
			if response.ConnectionId == "" {
				return
			}
			callback(response)
		})
	}
}

func errorReply(connID string, cmd int32, message, code string, kv ...string) *protobuf.Message {
	payload := map[string]string{
		"message": message,
		"code":    code,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		payload[kv[i]] = kv[i+1]
	}
	return &protobuf.Message{
		ConnectionId: connID,
		Route:        protobuf.RouteError,
		Cmd:          cmd,
		Payload:      payload,
		Timestamp:    time.Now().UnixMilli(),
	}
}
