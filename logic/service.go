//go:build legacy

package logic

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/etcd"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	server     *Server
	registry   *etcd.Component
	listener   net.Listener
	grpcServer *grpc.Server
	cfg        ServiceConfig
	stopOnce   sync.Once
}

func NewService(opts ...ServiceOption) *Service {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.AdvertiseAddr == "" {
		cfg.AdvertiseAddr = "localhost:" + cfg.ListenPort
	}
	serverOpts := []ServerOption{WithServerID(cfg.ServiceID), WithStreamChSize(cfg.StreamSendChSize)}
	return &Service{server: NewServer(serverOpts...), cfg: cfg}
}

func (s *Service) Server() *Server { return s.server }

func (s *Service) RegisterProto(cmd int32, reqProto proto.Message, respCmd int32, handler ProtoHandler) {
	s.server.RegisterProto(cmd, reqProto, respCmd, handler)
}

func (s *Service) RegisterUser(userKey, sessionID string) { s.server.RegisterUser(userKey, sessionID) }
func (s *Service) UnregisterUser(userKey string)          { s.server.UnregisterUser(userKey) }
func (s *Service) GetCommands() []int32                   { return s.server.registeredCommands() }

func (s *Service) Start() error {
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
			tlog.Error("gRPC server stopped", "error", err)
		}
	}()
	s.initRegistry()
	tlog.Info("logic service started", "serviceID", s.cfg.ServiceID, "address", s.cfg.AdvertiseAddr)
	return nil
}

func (s *Service) initRegistry() {
	if s.cfg.ServiceID == "" || s.cfg.EtcdEndpoint == "" {
		return
	}
	zone := s.cfg.Zone
	if zone == "" {
		zone = "default"
	}
	endpoints := s.cfg.EtcdEndpoints
	if len(endpoints) == 0 {
		endpoints = []string{s.cfg.EtcdEndpoint}
	}
	s.registry = etcd.New(etcd.ComponentConfig{
		Enabled:      true,
		Etcd:         etcd.Config{Endpoints: endpoints, Endpoint: s.cfg.EtcdEndpoint, Username: s.cfg.EtcdUsername, Password: s.cfg.EtcdPassword, ServicePrefix: s.cfg.EtcdServicePrefix},
		Registration: etcd.RegistrationConfig{Enabled: true, ServiceID: fmt.Sprintf("%s:%s", s.cfg.ServerType, zone), InstanceID: s.cfg.ServiceID, Address: s.cfg.AdvertiseAddr, LeaseTTL: s.cfg.EtcdLeaseTTL},
	})
	if err := s.registry.Start(); err != nil {
		tlog.Error("service registration failed", "error", err)
	}
}

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
