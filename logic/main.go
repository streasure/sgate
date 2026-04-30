package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
)



// LogicServer 逻辑服 gRPC 服务实现
type LogicServer struct {
	protobuf.UnimplementedGatewayServiceServer
	connections sync.Map // 存储连接信息
}

// NewLogicServer 创建逻辑服 gRPC 服务实例
func NewLogicServer() *LogicServer {
	return &LogicServer{}
}

// StreamMessages 流式消息处理
func (s *LogicServer) StreamMessages(stream protobuf.GatewayService_StreamMessagesServer) error {
	// 生成连接ID
	connectionID := fmt.Sprintf("conn_%d", time.Now().UnixNano())

	// 存储连接信息
	s.connections.Store(connectionID, stream)
	tlog.Info("新连接建立", "connectionID", connectionID)

	// 处理双向流
	for {
		// 接收客户端消息
		msg, err := stream.Recv()
		if err != nil {
			// 客户端关闭连接
			// 使用 CompareAndDelete 删除连接
			if s.connections.CompareAndDelete(connectionID, stream) {
				tlog.Info("连接已关闭", "connectionID", connectionID)
			}
			return err
		}

		// 处理消息
		s.handleMessage(msg, func(response interface{}) {
			if protoMsg, ok := response.(*protobuf.Message); ok {
				if protoMsg.ConnectionId == "" {
					protoMsg.ConnectionId = msg.ConnectionId
				}
				stream.Send(protoMsg)
			} else if errorMsg, ok := response.(*protobuf.ErrorResponse); ok {
				responseMsg := &protobuf.Message{
					ConnectionId: msg.ConnectionId,
					Route:        "error",
					Payload: map[string]string{
						"message": errorMsg.Error.Message,
						"code":    errorMsg.Error.Code,
						"details": errorMsg.Error.Details,
					},
				}
				stream.Send(responseMsg)
			}
		})
	}
}

// SendMessage 发送单条消息
func (s *LogicServer) SendMessage(ctx context.Context, msg *protobuf.Message) (*protobuf.Message, error) {
	var response *protobuf.Message
	var wg sync.WaitGroup
	wg.Add(1)

	// 处理消息
	s.handleMessage(msg, func(resp interface{}) {
		defer wg.Done()
		if protoMsg, ok := resp.(*protobuf.Message); ok {
			response = protoMsg
		} else if errorMsg, ok := resp.(*protobuf.ErrorResponse); ok {
			// 转换为Message
			response = &protobuf.Message{
				Route: "error",
				Payload: map[string]string{
					"message": errorMsg.Error.Message,
					"code":    errorMsg.Error.Code,
					"details": errorMsg.Error.Details,
				},
			}
		}
	})

	wg.Wait()
	return response, nil
}

// handleMessage 处理消息
func (s *LogicServer) handleMessage(msg *protobuf.Message, callback func(interface{})) {
	// 输出日志
	tlog.Info("收到消息", "route", msg.Route, "connectionID", msg.ConnectionId, "payload", msg.Payload)

	// 检查路由
	if msg.Route == "" {
		callback(&protobuf.ErrorResponse{
			Error: &protobuf.ErrorData{
				Message: "Missing route",
				Code:    "400",
				Details: "",
			},
		})
		return
	}

	// 处理不同的路由
	switch msg.Route {
	case "ping":
		var connectionCount int
		s.connections.Range(func(key, value interface{}) bool {
			connectionCount++
			return true
		})
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "ping",
			Payload: map[string]string{
				"timestamp":       fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":         "Pong from logic server",
				"connectionCount": fmt.Sprintf("%d", connectionCount),
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case "test":
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "test",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":   "Test response from logic server",
				"data":      msg.GetPayload()["data"],
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case "echo":
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "echo",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"message":   "Echo from logic server",
				"echo":      msg.GetPayload()["message"],
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case "getConnections":
		var connectionCount int
		s.connections.Range(func(key, value interface{}) bool {
			connectionCount++
			return true
		})
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "getConnections",
			Payload: map[string]string{
				"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"count":     fmt.Sprintf("%d", connectionCount),
			},
			Timestamp: time.Now().UnixMilli(),
		})

	default:
		callback(&protobuf.ErrorResponse{
			Error: &protobuf.ErrorData{
				Message: "Route not found",
				Code:    "404",
				Details: "The requested route does not exist",
			},
		})
	}
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, buf[:n])
		}
	}()

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			fmt.Println("failed to initialize tlog:", err)
		}
	}

	listener, err := net.Listen("tcp", ":50052")
	if err != nil {
		tlog.Error("无法监听端口", "error", err)
		os.Exit(1)
	}

	server := grpc.NewServer()
	protobuf.RegisterGatewayServiceServer(server, NewLogicServer())

	go func() {
		tlog.Info("逻辑服 gRPC 服务器启动", "port", "50052")
		if err := server.Serve(listener); err != nil {
			tlog.Error("启动服务器失败", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	tlog.Info("收到退出信号，开始关闭...", "signal", sig.String())
	server.GracefulStop()
	tlog.Info("逻辑服已关闭")
	tlog.Sync()
}
