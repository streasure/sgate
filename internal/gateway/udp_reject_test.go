package gateway

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

// udpDatagram 构造 UDP 数据报：4B 大端长度 + MessageFrame。
func udpDatagram(t *testing.T, cmd int32, body []byte) []byte {
	t.Helper()
	frame, err := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: cmd, Data: body})
	if err != nil {
		t.Fatalf("marshal client message: %v", err)
	}
	buf := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(buf, uint32(len(frame)))
	copy(buf[4:], frame)
	return buf
}

// decodeErrorFrame 解析回写帧（4B 长度 + MessageFrame{CmdError}）为错误消息。
func decodeErrorFrame(t *testing.T, written []byte) string {
	t.Helper()
	if len(written) < 4 {
		t.Fatalf("written len=%d, want >=4", len(written))
	}
	n := binary.BigEndian.Uint32(written[:4])
	if int(n)+4 > len(written) {
		t.Fatalf("frame len=%d exceeds written=%d", n, len(written))
	}
	msg, ok := protoGw.DecodeClientMessage(written[4 : 4+n])
	if !ok {
		t.Fatalf("decode response frame failed bytes=%v", written[4:4+n])
	}
	defer protoGw.PutClientMessage(msg)
	if msg.Cmd != protoGw.CmdError {
		t.Fatalf("resp cmd=%d, want CmdError=%d", msg.Cmd, protoGw.CmdError)
	}
	var resp commonstruct.ErrorResponse
	if err := proto.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal ErrorResponse: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("nil error payload")
	}
	return resp.Error.Message
}

// TestUDPDatagramRejectedWithoutPipeline 回归：UDP 曾被投进 pipeline，
// 而 gnet UDP 不触发 OnOpen、ConnectionManager 中无对应连接，阶段 2 必然以
// unknown connection 失败 —— 一次必然失败的完整往返。定位为预留通道后，
// 这里应明确拒绝（语义诚实的错误帧 + droppedUnsupported 计数），且不建连接、
// 不投 pipeline。
func TestUDPDatagramRejectedWithoutPipeline(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	c := &kickMockConn{}

	gw.handleUDPDatagram(c, udpDatagram(t, 1100001, []byte("login")))

	if got := gw.messagesDroppedUnsupported.Load(); got != 1 {
		t.Fatalf("droppedUnsupported = %d, want 1", got)
	}
	msg := decodeErrorFrame(t, c.writtenCopy())
	if !strings.Contains(msg, errUDPUnsupported) {
		t.Fatalf("error message = %q, want contains %q", msg, errUDPUnsupported)
	}
	// 不得注册任何连接（UDP 无会话），也就不该有 pipeline 可投递的连接
	if got := gw.connectionManager.GetConnectionCount(); got != 0 {
		t.Fatalf("connection count = %d, want 0 (UDP must not register connections)", got)
	}
}

// TestUDPDatagramJunkDroppedSilently 垃圾数据报静默丢弃：不回包（控制回包放大）、
// 不计数（计数只统计能解码的业务消息）。
func TestUDPDatagramJunkDroppedSilently(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	c := &kickMockConn{}

	gw.handleUDPDatagram(c, []byte("short"))
	gw.handleUDPDatagram(c, []byte{0, 0, 0, 9, 1, 2, 3}) // 长度头与实际不符

	if len(c.writtenCopy()) != 0 {
		t.Fatalf("junk datagram got %d bytes reply, want silent drop", len(c.writtenCopy()))
	}
	if got := gw.messagesDroppedUnsupported.Load(); got != 0 {
		t.Fatalf("droppedUnsupported = %d, want 0", got)
	}
}
