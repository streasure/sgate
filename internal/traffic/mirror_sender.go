package traffic

import (
	"context"
	"errors"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/panicutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// errMirrorBackoff 目标处于退避窗口，本条消息不投递（快速失败）。
var errMirrorBackoff = errors.New("mirror target in retry backoff")

const (
	mirrorBackoffBase = time.Second
	mirrorBackoffMax  = 30 * time.Second
)

// mirrorSender 把镜像流量经 protocol GatewayStream.OnData 双向流推给镜像目标。
//
// 自含轻量 gRPC 客户端（而非复用 backend.LogicClient）：包依赖方向严格
// gateway → backend → connection，traffic 不得反向 import backend。
//
// 镜像目标必须是独立的测试环境 logic：镜像流在目标侧与真实网关无异，会绑定
// session 并承载其回包路由，指向生产 logic 会劫持同名会话。该前提配置校验
// 无法识别，只能由部署保证（见 config/gateway.yaml 注释）。
type mirrorSender struct {
	ctx        context.Context
	cancel     context.CancelFunc
	targetAddr string

	conn   *grpc.ClientConn
	client protoGw.GatewayStreamClient

	// mu 串行化建流与 SendMsg：gRPC 规定同一 stream 不得并发 SendMsg，
	// 多个 worker 共用一条流必须互斥。
	mu        sync.Mutex
	stream    protoGw.GatewayStream_OnDataClient
	backoff   time.Duration // 连续失败退避（成功发送后复位）
	nextRetry time.Time     // 退避截止时刻；零值=可立即连接
	recvWG    sync.WaitGroup
}

// newMirrorSender 构造镜像发送端；grpc.NewClient 是惰性建连（不阻塞），
// 仅在 targetAddr 无法解析时返回错误（此时返回 nil，由调用方告警）。
func newMirrorSender(targetAddr string) *mirrorSender {
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := grpc.NewClient(targetAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// keepalive 与 logic/dialer.go 拨出侧一致：≥5min 且不无流 ping，
		// 否则触发对端 GoAway ENHANCE_YOUR_CALM "too_many_pings"。
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	)
	if err != nil {
		cancel()
		return nil
	}
	return &mirrorSender{
		ctx:        ctx,
		cancel:     cancel,
		targetAddr: targetAddr,
		conn:       conn,
		client:     protoGw.NewGatewayStreamClient(conn),
	}
}

// Send 发送一条镜像消息。目标不可达时快速失败：退避窗口内直接返回错误，
// 单条消息不重试 —— worker 只负责消费队列，镜像目标故障不得拖住队列。
func (s *mirrorSender) Send(msg *protoGw.StreamData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == nil {
		if err := s.connectLocked(); err != nil {
			return err
		}
	}
	if err := s.stream.SendMsg(msg); err != nil {
		// Send 失败即流作废：丢弃引用，由退避窗口后的重建接续
		// （失败流必然已中止，放弃引用不会残留未关闭的流）。
		s.stream = nil
		s.failLocked()
		return err
	}
	s.backoff = 0
	s.nextRetry = time.Time{}
	return nil
}

// connectLocked 建立（或重建）镜像流并启动接收协程；持调用方锁。
// fail-fast 语义：目标故障时 OnData 快速返回错误，不等待连接成功。
func (s *mirrorSender) connectLocked() error {
	if !s.nextRetry.IsZero() && time.Now().Before(s.nextRetry) {
		return errMirrorBackoff
	}
	stream, err := s.client.OnData(s.ctx)
	if err != nil {
		s.failLocked()
		return err
	}
	s.stream = stream
	s.startRecvLocked(stream)
	tlog.Info(s.ctx, "traffic mirror stream connected target=%s", s.targetAddr)
	return nil
}

// startRecvLocked 启动接收协程排空目标侧回推的消息（响应/推送）。
// 镜像是单向上行复制：本地没有镜像会话对应的连接可投递，回包只能丢弃；
// 不排空则接收窗口写满后会反压目标侧对该流的写入。单协程 RecvMsg 与
// 并发 SendMsg 是 gRPC 明确允许的组合。
func (s *mirrorSender) startRecvLocked(stream protoGw.GatewayStream_OnDataClient) {
	s.recvWG.Add(1)
	panicutil.Go(s.ctx, "traffic.mirrorSender.recv", func() {
		defer s.recvWG.Done()
		in := &protoGw.StreamData{}
		for {
			if err := stream.RecvMsg(in); err != nil {
				// 流终止（目标断开）：只作废仍归属本协程的那条流
				s.mu.Lock()
				if s.stream == stream {
					s.stream = nil
					s.failLocked()
				}
				s.mu.Unlock()
				return
			}
		}
	})
}

// failLocked 记录一次失败并推进退避窗口（指数退避，封顶 30s）；持调用方锁。
// 退避只在「成功发送」后复位，避免目标反复抖动时每条消息都重建流。
func (s *mirrorSender) failLocked() {
	if s.backoff == 0 {
		s.backoff = mirrorBackoffBase
	} else {
		s.backoff = min(s.backoff*2, mirrorBackoffMax)
	}
	s.nextRetry = time.Now().Add(s.backoff)
	tlog.Warn(s.ctx, "traffic mirror stream failed, backing off target=%s backoff=%s", s.targetAddr, s.backoff)
}

// Close 关闭流与连接并等待接收协程退出（幂等，供 TrafficMirror.Stop 调用）。
// 先 cancel：可能正阻塞在建流上的 Send 需要靠 ctx 取消立即返回。
func (s *mirrorSender) Close() {
	s.cancel()
	s.mu.Lock()
	s.stream = nil
	s.mu.Unlock()
	_ = s.conn.Close()
	s.recvWG.Wait()
}
