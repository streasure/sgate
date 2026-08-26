package logic

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/streasure/sgate/discovery"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	server     *Server
	registry   *discovery.ServiceRegistry
	listener   net.Listener
	grpcServer *grpc.Server
	cfg        ServiceConfig
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func NewService(opts ...ServiceOption) *Service {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.ServiceID == "" {
		tlog.Warn("ServiceID is empty, service discovery will be disabled")
	}
	if cfg.AdvertiseAddr == "" {
		cfg.AdvertiseAddr = "localhost:" + cfg.ListenPort
	}

	serverOpts := []ServerOption{WithServerID(cfg.ServiceID)}
	if cfg.DispatchWorkers > 0 {
		serverOpts = append(serverOpts, WithDispatchWorkers(cfg.DispatchWorkers))
	}
	if cfg.DispatchChSize > 0 {
		serverOpts = append(serverOpts, WithDispatchChSize(cfg.DispatchChSize))
	}
	if cfg.StreamSendChSize > 0 {
		serverOpts = append(serverOpts, WithStreamChSize(cfg.StreamSendChSize))
	}
	if cfg.Passthrough {
		serverOpts = append(serverOpts, WithServerPassthrough())
	}

	return &Service{
		server: NewServer(serverOpts...),
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

func (s *Service) Server() *Server {
	return s.server
}

func (s *Service) RegisterRoute(route string, handler RouteHandler) {
	s.server.RegisterRoute(route, handler)
}

func (s *Service) RegisterBurstRoute(route string, handler BurstRouteHandler) {
	s.server.RegisterBurstRoute(route, handler)
}

func (s *Service) RegisterProto(route string, cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) {
	s.server.RegisterProto(route, cmd, reqProto, respCmd, handler)
}

func (s *Service) RegisterDispatcher(d *Dispatcher) {
	s.server.RegisterDispatcher(d)
}

func (s *Service) GetRoutes() []string {
	var routes []string
	s.Server().routes.Range(func(key, value interface{}) bool {
		routes = append(routes, key.(string))
		return true
	})
	return routes
}

func (s *Service) Start() error {
	listenAddr := s.cfg.ListenAddr + ":" + s.cfg.ListenPort
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
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
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
		grpc.InitialWindowSize(int32(windowSize)),
		grpc.InitialConnWindowSize(int32(windowSize)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	s.server.RegisterGatewayServiceServer(s.grpcServer)

	go func() {
		tlog.Info("gRPC server starting", "port", s.cfg.ListenPort)
		if err := s.grpcServer.Serve(listener); err != nil {
			tlog.Error("gRPC server failed", "error", err)
		}
	}()

	s.initRegistry()

	tlog.Info("logic service started",
		"serviceID", s.cfg.ServiceID,
		"address", s.cfg.AdvertiseAddr,
		"port", s.cfg.ListenPort,
	)
	return nil
}

func (s *Service) initRegistry() {
	if s.cfg.ServiceID == "" {
		tlog.Warn("ServiceID is empty, skipping service discovery registration")
		return
	}
	if s.cfg.NacosEndpoint == "" {
		tlog.Warn("Nacos endpoint is empty, skipping service discovery registration")
		return
	}

	zone := s.cfg.Zone
	if zone == "" {
		zone = "default"
	}
	serviceInfo := &discovery.ServiceInfo{
		ServiceID:   s.cfg.ServiceID,
		ServiceName: s.cfg.ServiceName,
		Address:     s.cfg.AdvertiseAddr,
		Weight:      1,
		Metadata: map[string]string{
			"version": "1.0.0",
			"port":    s.cfg.ListenPort,
			"routes":  strings.Join(s.GetRoutes(), ","),
			"zone":    zone,
		},
		StartTime: time.Now().UnixMilli(),
	}

	s.registry = discovery.NewServiceRegistry(serviceInfo, s.cfg.HeartbeatInterval, s.cfg.HeartbeatTTL)
	s.registry.SetNacosConfig(discovery.NacosNamingConfig{
		Endpoint:       s.cfg.NacosEndpoint,
		NamingEndpoint: s.cfg.NacosNamingEndpoint,
		Namespace:      s.cfg.NacosNamespace,
		Group:          s.cfg.NacosGroup,
		Username:       s.cfg.NacosUsername,
		Password:       s.cfg.NacosPassword,
		APIVersion:     s.cfg.NacosAPIVersion,
	})
	if err := s.registry.Start(); err != nil {
		tlog.Error("service registration failed", "error", err)
	}
}

func (s *Service) Stop() {
	if s.registry != nil {
		s.registry.Stop()
	}

	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}

	tlog.Info("logic service stopped",
		"serviceID", s.cfg.ServiceID,
		"address", s.cfg.AdvertiseAddr,
	)
}

func (s *Service) Run() error {
	if err := s.Start(); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	tlog.Info("received shutdown signal", "signal", sig.String())
	s.Stop()
	return nil
}
