package logic

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/streasure/protocol/enums"
	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/netutil"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

// Service 逻辑层服务封装，管理 gRPC 服务器、etcd 注册和生命周期
type Service struct {
	server     *Server          // 逻辑层服务端
	registry   *uetcd.Component // etcd 注册组件
	listener   net.Listener     // TCP 监听器
	grpcServer *grpc.Server     // gRPC 服务器
	cfg        ServiceConfig    // 服务配置
	stopOnce   sync.Once        // 确保只停止一次
}

// NewService 创建逻辑层服务实例，应用配置选项
func NewService(opts ...ServiceOption) *Service {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.AdvertiseAddr == "" {
		cfg.AdvertiseAddr = netutil.GetOutboundIPv4() + ":" + cfg.ListenPort
	}
	serverOpts := []ServerOption{WithServerID(cfg.ServiceID), WithStreamChSize(cfg.StreamSendChSize)}
	return &Service{server: NewServer(serverOpts...), cfg: cfg}
}

// Server 获取底层逻辑层服务端实例
func (s *Service) Server() *Server { return s.server }

// RegisterProto 注册 protobuf 协议处理器
func (s *Service) RegisterProto(cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) {
	s.server.RegisterProto(cmd, reqProto, respCmd, handler)
}

// RegisterUser 注册用户与会话的映射
func (s *Service) RegisterUser(userUUID, sessionID string) {
	s.server.RegisterUser(userUUID, sessionID)
}

// UnregisterUser 注销用户与会话的映射
func (s *Service) UnregisterUser(userUUID string) { s.server.UnregisterUser(userUUID) }

// Start 启动 gRPC 服务器和 etcd 注册
func (s *Service) Start() error {
	if s.cfg.AdvertiseAddr == "" {
		s.cfg.AdvertiseAddr = netutil.GetOutboundIPv4() + ":" + s.cfg.ListenPort
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddr+":"+s.cfg.ListenPort)
	if err != nil {
		return fmt.Errorf("listen on %s:%s: %w", s.cfg.ListenAddr, s.cfg.ListenPort, err)
	}
	s.listener = listener
	windowSize := s.cfg.GRPCWindowSize
	if windowSize <= 0 {
		windowSize = 524288
	}
	maxMsgSize := s.cfg.GRPCMaxMessageSize
	if maxMsgSize <= 0 {
		maxMsgSize = 4 * 1024 * 1024
	}
	s.grpcServer = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsgSize), grpc.MaxSendMsgSize(maxMsgSize),
		grpc.InitialWindowSize(int32(windowSize)), grpc.InitialConnWindowSize(int32(windowSize)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	protocol.RegisterGatewayStreamServer(s.grpcServer, s.server)
	go func() {
		if err := s.grpcServer.Serve(listener); err != nil {
			tlog.Error(context.TODO(), "gRPC server stopped error=%v", err)
		}
	}()
	// 等待 gRPC 服务就绪后再注册 etcd，避免 sgate 连接时服务端还没 accept
	time.Sleep(200 * time.Millisecond)
	s.initRegistry()
	tlog.Info(context.TODO(), "logic service started serviceID=%s address=%s", s.cfg.ServiceID, s.cfg.AdvertiseAddr)
	return nil
}

// initRegistry 初始化 etcd 服务注册
func (s *Service) initRegistry() {
	if s.cfg.ServiceID == "" || s.cfg.EtcdEndpoint == "" {
		return
	}
	zone := s.cfg.Zone
	if zone == "" {
		zone = "default"
	}
	belong := s.cfg.Belong
	if belong == "" {
		belong = "default"
	}
	endpoints := s.cfg.EtcdEndpoints
	if len(endpoints) == 0 {
		endpoints = []string{s.cfg.EtcdEndpoint}
	}
	// ServiceID 使用 protocol enums 的 SERVER_TYPE_LOGICSERVER，与网关发现一致
	// 格式: {belong}/{serverType}:{zone}，etcd key: /services/{belong}/{serverType}:{zone}/{instanceId}
	logicType := enums.ServerType_name[int32(enums.ServerType_SERVER_TYPE_LOGICSERVER)]
	serviceID := belong + "/" + logicType + ":" + zone
	s.registry = uetcd.New(uetcd.ComponentConfig{
		Etcd:         uetcd.Config{Endpoints: endpoints, Endpoint: s.cfg.EtcdEndpoint, Username: s.cfg.EtcdUsername, Password: s.cfg.EtcdPassword, ServicePrefix: s.cfg.EtcdServicePrefix},
		Registration: uetcd.RegistrationConfig{ServiceID: serviceID, InstanceID: s.cfg.ServiceID, Address: s.cfg.AdvertiseAddr, LeaseTTL: s.cfg.EtcdLeaseTTL},
	})
	if err := s.registry.Start(); err != nil {
		tlog.Error(context.TODO(), "service registration failed error=%v", err)
	}
}

// Stop 优雅停止服务（等待连接关闭）
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		if s.registry != nil {
			s.registry.Destroy()
		}
		if s.grpcServer != nil {
			s.grpcServer.GracefulStop()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.server.Stop()
	})
}

// Run 启动服务并监听系统信号，收到 SIGINT/SIGTERM 时优雅停止
func (s *Service) Run() error {
	if err := s.Start(); err != nil {
		return err
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	s.Stop()
	return nil
}
