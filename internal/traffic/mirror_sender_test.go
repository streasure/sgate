package traffic

import (
	"errors"
	"net"
	"testing"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
	"google.golang.org/grpc"
)

// mirrorSink 朴素镜像目标：收下每条 StreamData 供断言，不回推任何消息。
type mirrorSink struct {
	protoGw.UnimplementedGatewayStreamServer
	ch chan *protoGw.StreamData
}

func (s *mirrorSink) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		select {
		case s.ch <- msg:
		default:
		}
	}
}

// startMirrorSink 启动一个真实 gRPC 镜像目标，返回地址与停止函数。
func startMirrorSink(t *testing.T) (string, *mirrorSink) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sink := &mirrorSink{ch: make(chan *protoGw.StreamData, 8)}
	srv := grpc.NewServer()
	protoGw.RegisterGatewayStreamServer(srv, sink)
	go srv.Serve(ln) //nolint:errcheck // Serve 在 Stop 时返回
	t.Cleanup(srv.Stop)
	return ln.Addr().String(), sink
}

func recvOne(t *testing.T, sink *mirrorSink) *protoGw.StreamData {
	t.Helper()
	select {
	case msg := <-sink.ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("mirror target received nothing within 5s")
		return nil
	}
}

// TestMirrorSender_DeliversStreamData 回归：镜像不再是日志桩 ——
// 消息经 OnData 双向流真实送达目标，字段映射完整。
func TestMirrorSender_DeliversStreamData(t *testing.T) {
	addr, sink := startMirrorSink(t)

	s := newMirrorSender(addr)
	if s == nil {
		t.Fatal("newMirrorSender returned nil")
	}
	defer s.Close()

	msg := &protoGw.StreamData{
		SessionId: "conn-1",
		UserKey:   "user-1",
		Cmd:       42,
		Data:      []byte("payload"),
		ClientIp:  "10.0.0.1",
	}
	if err := s.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := recvOne(t, sink)
	if got.SessionId != "conn-1" || got.UserKey != "user-1" || got.Cmd != 42 ||
		got.ClientIp != "10.0.0.1" || string(got.Data) != "payload" {
		t.Fatalf("received %+v, want fields of %+v", got, msg)
	}
}

// TestMirrorSender_UnreachableTargetBacksOff 回归：目标不可达时快速失败并进入
// 退避窗口，后续消息在窗口内不再尝试建流（镜像故障不得拖住 worker）。
func TestMirrorSender_UnreachableTargetBacksOff(t *testing.T) {
	// 127.0.0.1:1 无监听 → 连接被拒（保留端口 0 由内核分配，此处取必然空闲的 1）
	s := newMirrorSender("127.0.0.1:1")
	if s == nil {
		t.Fatal("newMirrorSender returned nil")
	}
	defer s.Close()

	msg := &protoGw.StreamData{SessionId: "c", Cmd: 1}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Send(msg) }()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Send to unreachable target returned nil, want error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send blocked on unreachable target, want fast failure")
	}

	// 退避窗口内第二次发送必须立刻失败，不重新拨号
	if err := s.Send(msg); !errors.Is(err, errMirrorBackoff) {
		t.Fatalf("second Send error = %v, want errMirrorBackoff", err)
	}
}

// TestTrafficMirror_WorkerForwardsToTarget 回归：MirrorFilter 入队后由 worker
// 真实转发到镜像目标（端到端覆盖采样 → 队列 → 发送全链路）。
func TestTrafficMirror_WorkerForwardsToTarget(t *testing.T) {
	addr, sink := startMirrorSink(t)

	tm := NewTrafficMirror(config.TrafficMirrorConfig{
		Enabled: true, Percent: 100, TargetAddr: addr, QueueSize: 16, Workers: 1,
	})
	defer tm.Stop()

	tm.Mirror(&types.FilterContext{
		ConnectionID: "conn-9", UserUUID: "user-9", Cmd: 77, Data: []byte("body"),
	})
	got := recvOne(t, sink)
	if got.SessionId != "conn-9" || got.UserKey != "user-9" || got.Cmd != 77 ||
		string(got.Data) != "body" {
		t.Fatalf("received %+v, want conn-9/user-9/77/body", got)
	}
	if failed := tm.SendFailed(); failed != 0 {
		t.Fatalf("SendFailed = %d, want 0", failed)
	}
}

// TestTrafficMirror_NoTargetCountsSendFailed 回归：enabled 但未配目标时
// 采样不静默消失，全部计入 sendFailed（配置错误可观测）。
func TestTrafficMirror_NoTargetCountsSendFailed(t *testing.T) {
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: true, Percent: 100, QueueSize: 16, Workers: 1})
	defer tm.Stop()

	tm.Mirror(&types.FilterContext{ConnectionID: "conn-1", Cmd: 1})
	deadline := time.Now().Add(3 * time.Second)
	for tm.SendFailed() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("SendFailed never incremented")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f, d := tm.Stats(); f != 1 || d != 0 {
		t.Fatalf("Stats() = (%d,%d), want (1,0)", f, d)
	}
}
