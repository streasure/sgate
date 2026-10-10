package connection

import (
	"encoding/binary"
	"testing"
)

// TestReframeCoalescedAsWSFrames 多段 [len][payload] 序列应转成等价的多帧 WS binary。
// 回归：旧实现有两趟循环，第一趟 EncodeWSFrame 结果被丢弃（纯浪费 + 每段一次多余分配）。
func TestReframeCoalescedAsWSFrames(t *testing.T) {
	p1 := []byte("hello")
	p2 := []byte("world-again-longer-payload")
	var raw []byte
	for _, p := range [][]byte{p1, p2} {
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(p)))
		raw = append(raw, lb[:]...)
		raw = append(raw, p...)
	}

	out := reframeCoalescedAsWSFrames(raw)
	if out == nil {
		t.Fatal("reframe returned nil for valid input")
	}

	// 解出两帧并核对 payload。
	want := [][]byte{p1, p2}
	off := 0
	for i, w := range want {
		if off+2 > len(out) {
			t.Fatalf("frame %d: truncated header", i)
		}
		if out[off] != 0x82 {
			t.Fatalf("frame %d opcode = %#x, want 0x82", i, out[off])
		}
		l := int(out[off+1])
		if l >= 126 {
			t.Fatalf("frame %d unexpected extended len %d (test payloads <126)", i, l)
		}
		off += 2
		if off+l > len(out) {
			t.Fatalf("frame %d: truncated payload", i)
		}
		if string(out[off:off+l]) != string(w) {
			t.Fatalf("frame %d payload = %q, want %q", i, out[off:off+l], w)
		}
		off += l
	}
	if off != len(out) {
		t.Fatalf("trailing bytes after frames: %d", len(out)-off)
	}
}

// TestReframeCoalescedRejectsCorruptLength 长度越界应返回 nil（丢弃整批，不半帧发送）。
func TestReframeCoalescedRejectsCorruptLength(t *testing.T) {
	raw := []byte{0, 0, 0, 200, 'a'} // len=200 但只剩 1 字节
	if out := reframeCoalescedAsWSFrames(raw); out != nil {
		t.Fatalf("want nil for corrupt length, got %d bytes", len(out))
	}
}
