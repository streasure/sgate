package logic

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/streasure/sgate/discovery"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	server     *Server
	registry   *discovery.ServiceRegistry
	rdb        *redis.Client
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

	return &Service{
		server: NewServer(WithServerID(cfg.ServiceID)),
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

	s.grpcServer = grpc.NewServer(
		grpc.MaxRecvMsgSize(4*1024*1024),
		grpc.MaxSendMsgSize(4*1024*1024),
		grpc.InitialWindowSize(524288),
		grpc.InitialConnWindowSize(524288),
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

	s.rdb = redis.NewClient(&redis.Options{
		Addr:     s.cfg.RedisAddr,
		Password: s.cfg.RedisPassword,
		DB:       s.cfg.RedisDB,
	})

	ctx := context.Background()
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		tlog.Warn("Redis connection failed, service discovery unavailable", "error", err, "addr", s.cfg.RedisAddr)
		s.rdb.Close()
		s.rdb = nil
		return
	}

	tlog.Info("Redis connected", "addr", s.cfg.RedisAddr)

	serviceInfo := &discovery.ServiceInfo{
		ServiceID:   s.cfg.ServiceID,
		ServiceName: s.cfg.ServiceName,
		Address:     s.cfg.AdvertiseAddr,
		Weight:      1,
		Metadata: map[string]string{
			"version": "1.0.0",
			"port":    s.cfg.ListenPort,
			"routes":  strings.Join(s.GetRoutes(), ","),
		},
		StartTime: time.Now().UnixMilli(),
	}

	s.registry = discovery.NewServiceRegistry(s.rdb, serviceInfo, s.cfg.HeartbeatInterval, s.cfg.HeartbeatTTL)
	if err := s.registry.Start(); err != nil {
		tlog.Error("service registration failed", "error", err)
	}
}

func (s *Service) Stop() {
	if s.registry != nil {
		s.registry.Stop()
	}

	if s.rdb != nil {
		s.rdb.Close()
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
