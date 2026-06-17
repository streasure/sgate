package main

import (
	"strconv"
	"time"

	"github.com/streasure/sgate/logic"
	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

func init() {
	RegisterRouteBuilder(registerPingRoute)
	RegisterRouteBuilder(registerTestRoute)
	RegisterRouteBuilder(registerEchoRoute)
	RegisterRouteBuilder(registerGetConnectionsRoute)
}

var (
	prebuiltTestResp *protobuf.Message
	prebuiltPongResp *protobuf.Message
)

func prebuildResponses() {
	testRespInner := &protobuf.Message{
		Route:   protobuf.RouteTestResult,
		Payload: map[string]string{"success": "true", "message": "Test route works"},
	}
	testData, _ := proto.Marshal(testRespInner)
	prebuiltTestResp = &protobuf.Message{
		Route: protobuf.RouteTestResult,
		Data:  testData,
	}

	pongRespInner := &protobuf.Message{
		Route:   protobuf.RoutePong,
		Payload: map[string]string{"timestamp": "0"},
	}
	pongData, _ := proto.Marshal(pongRespInner)
	prebuiltPongResp = &protobuf.Message{
		Route: protobuf.RoutePong,
		Data:  pongData,
	}
}

func registerPingRoute(svc *logic.Service) {
	svc.RegisterRoute(protobuf.RoutePing, func(msg *protobuf.Message) *protobuf.Message {
		resp := *prebuiltPongResp
		resp.ConnectionId = msg.ConnectionId
		resp.Timestamp = time.Now().UnixMilli()
		return &resp
	})
}

func registerTestRoute(svc *logic.Service) {
	svc.RegisterRoute(protobuf.RouteTest, func(msg *protobuf.Message) *protobuf.Message {
		resp := *prebuiltTestResp
		resp.ConnectionId = msg.ConnectionId
		resp.Timestamp = time.Now().UnixMilli()
		return &resp
	})
}

func registerEchoRoute(svc *logic.Service) {
	svc.RegisterRoute(protobuf.RouteEcho, func(msg *protobuf.Message) *protobuf.Message {
		inner := &protobuf.Message{
			Route: protobuf.RouteEcho,
			Payload: map[string]string{
				"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10),
				"message":   "Echo from logic server",
				"echo":      msg.GetPayload()["message"],
			},
		}
		data, _ := proto.Marshal(inner)
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RouteEcho,
			Data:         data,
			Timestamp:    time.Now().UnixMilli(),
		}
	})
}

func registerGetConnectionsRoute(svc *logic.Service) {
	svc.RegisterRoute(protobuf.RouteGetConnections, func(msg *protobuf.Message) *protobuf.Message {
		inner := &protobuf.Message{
			Route: protobuf.RouteGetConnections,
			Payload: map[string]string{
				"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10),
				"count":     strconv.FormatInt(int64(svc.Server().GetConnectionCount()), 10),
			},
		}
		data, _ := proto.Marshal(inner)
		return &protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        protobuf.RouteGetConnections,
			Data:         data,
			Timestamp:    time.Now().UnixMilli(),
		}
	})
}
