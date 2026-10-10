package logic

import (
	"testing"
	"time"

	protocol "github.com/streasure/protocol/gateway"
)

// capturingStream 在 fakeStream 基础上捕获 Send/SendMsg 负载。
type capturingStream struct {
	fakeStream
	raw     []*protocol.StreamData
	batches [][]*protocol.StreamData
}

func (f *capturingStream) Send(m *protocol.StreamData) error {
	f.mu.Lock()
	f.raw = append(f.raw, m)
	f.mu.Unlock()
	return nil
}

func (f *capturingStream) SendMsg(m any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb, ok := m.(*protocol.StreamBatch); ok {
		f.batches = append(f.batches, sb.Items)
	}
	return nil
}

func (f *capturingStream) snapshot() (raw int, batches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raw), len(f.batches)
}

func waitBatchSent(f *capturingStream) bool {
	for range 500 {
		raw, batches := f.snapshot()
		if raw+batches > 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestStreamConnSendsStreamBatch 回归：单条消息也必须合帧为 StreamBatch
// （StreamBatch 为固定线格式，接收端固定按 StreamBatch 解码，
// 混发裸帧会导致对端 unmarshal 失败 → 重拨风暴）。
func TestStreamConnSendsStreamBatch(t *testing.T) {
	fs := &capturingStream{}
	c := newStreamConn(fs, 16, "gw-1", 0)

	if err := c.Send(&protocol.StreamData{Cmd: 5, Data: []byte("x")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !waitBatchSent(fs) {
		t.Fatal("batch send not observed in time")
	}
	raw, batches := fs.snapshot()
	if raw != 0 {
		t.Fatalf("batch mode mixed %d raw frames", raw)
	}
	if batches != 1 {
		t.Fatalf("batches = %d, want 1", batches)
	}
	fs.mu.Lock()
	items := fs.batches[0]
	fs.mu.Unlock()
	if len(items) != 1 || items[0].Cmd != 5 {
		t.Fatalf("unexpected batch items: %#v", items)
	}
	c.Close()
}
