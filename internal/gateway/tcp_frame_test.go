package gateway

import (
	"encoding/binary"
	"testing"
)

func frame(payloadLen int) []byte {
	buf := make([]byte, 4+payloadLen)
	binary.BigEndian.PutUint32(buf[:4], uint32(payloadLen))
	for i := range payloadLen {
		buf[4+i] = byte(i)
	}
	return buf
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestFrameBuf_SingleFrame 一次读事件恰好一帧。
func TestFrameBuf_SingleFrame(t *testing.T) {
	ctx := &ConnContext{}
	var got [][]byte
	if !ctx.feedFrames(frame(8), 65536, 4096, func(f []byte) bool {
		got = append(got, append([]byte(nil), f...))
		return true
	}) {
		t.Fatal("feedFrames returned false")
	}
	if len(got) != 1 || len(got[0]) != 8 {
		t.Fatalf("frames = %d (len %d), want 1 frame of 8", len(got), len(got[0]))
	}
	if len(ctx.FrameBuf) != 0 || ctx.frameOff != 0 {
		t.Fatalf("buffer not reset: len=%d off=%d", len(ctx.FrameBuf), ctx.frameOff)
	}
}

// TestFrameBuf_Coalesced 粘包：一次读事件多帧顺序交付。
func TestFrameBuf_Coalesced(t *testing.T) {
	ctx := &ConnContext{}
	var got [][]byte
	ok := ctx.feedFrames(concat(frame(4), frame(4), frame(12)), 65536, 4096, func(f []byte) bool {
		got = append(got, append([]byte(nil), f...))
		return true
	})
	if !ok || len(got) != 3 {
		t.Fatalf("ok=%v frames=%d, want true/3", ok, len(got))
	}
	if len(got[0]) != 4 || len(got[1]) != 4 || len(got[2]) != 12 {
		t.Fatalf("frame lens = %d/%d/%d, want 4/4/12", len(got[0]), len(got[1]), len(got[2]))
	}
}

// TestFrameBuf_PartialAcrossReads 跨读事件重组（回归：frameOff 偏移方案）。
func TestFrameBuf_PartialAcrossReads(t *testing.T) {
	ctx := &ConnContext{}
	full := frame(10)
	var got [][]byte
	if !ctx.feedFrames(full[:6], 65536, 4096, func(f []byte) bool {
		got = append(got, f)
		return true
	}) {
		t.Fatal("first feed returned false")
	}
	if len(got) != 0 {
		t.Fatalf("premature frame delivery: %d", len(got))
	}
	if !ctx.feedFrames(full[6:], 65536, 4096, func(f []byte) bool {
		got = append(got, append([]byte(nil), f...))
		return true
	}) {
		t.Fatal("second feed returned false")
	}
	if len(got) != 1 || len(got[0]) != 10 {
		t.Fatalf("frames = %d (len %d), want 1 frame of 10", len(got), len(got[0]))
	}
}

// TestFrameBuf_ExactBoundaryKeepsCapacity 精确边界消费后容量必须保留
// （旧实现置 nil 导致每次读事件重分配）。
func TestFrameBuf_ExactBoundaryKeepsCapacity(t *testing.T) {
	ctx := &ConnContext{}
	f := frame(16)
	if !ctx.feedFrames(f, 65536, 4096, func([]byte) bool { return true }) {
		t.Fatal("feed1 false")
	}
	if cap(ctx.FrameBuf) == 0 {
		t.Fatal("capacity dropped after exact consumption (old nil-reset behavior)")
	}
	// 再次满帧：不得增长 cap
	capBefore := cap(ctx.FrameBuf)
	if !ctx.feedFrames(f, 65536, 4096, func([]byte) bool { return true }) {
		t.Fatal("feed2 false")
	}
	if cap(ctx.FrameBuf) != capBefore {
		t.Fatalf("cap grew %d -> %d, want reuse", capBefore, cap(ctx.FrameBuf))
	}
}

// TestFrameBuf_PartialThenResidualCompact 残帧前移：部分帧到达后再收全+新帧。
func TestFrameBuf_PartialThenResidualCompact(t *testing.T) {
	ctx := &ConnContext{}
	full := frame(8)
	next := frame(4)
	var got [][]byte
	collect := func(f []byte) bool {
		got = append(got, append([]byte(nil), f...))
		return true
	}
	// 部分帧 + 后续同读补全并粘上新帧
	if !ctx.feedFrames(concat(full[:5], append(full[5:], next...)), 65536, 4096, collect) {
		t.Fatal("feed false")
	}
	if len(got) != 2 || len(got[0]) != 8 || len(got[1]) != 4 {
		t.Fatalf("frames=%d lens=%v, want 2 frames 8/4", len(got), func() []int {
			var r []int
			for _, g := range got {
				r = append(r, len(g))
			}
			return r
		}())
	}
}

// TestFrameBuf_ZeroLenRejected 长度 0 帧非法。
func TestFrameBuf_ZeroLenRejected(t *testing.T) {
	ctx := &ConnContext{}
	if ctx.feedFrames([]byte{0, 0, 0, 0}, 65536, 4096, func([]byte) bool { return true }) {
		t.Fatal("zero-length frame must be rejected")
	}
}

// TestFrameBuf_OversizedRejected 超 maxFrame 非法。
func TestFrameBuf_OversizedRejected(t *testing.T) {
	ctx := &ConnContext{}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, 5000)
	if ctx.feedFrames(buf, 65536, 4096, func([]byte) bool { return true }) {
		t.Fatal("oversized frame must be rejected")
	}
}

// TestFrameBuf_BufferLimitRejected 缓冲超限断连。
func TestFrameBuf_BufferLimitRejected(t *testing.T) {
	ctx := &ConnContext{}
	big := make([]byte, 100) // 头 4B 声称 200B 帧,缓冲上限 50
	binary.BigEndian.PutUint32(big, 200)
	if ctx.feedFrames(big, 50, 4096, func([]byte) bool { return true }) {
		t.Fatal("buffer limit must reject")
	}
}

// TestFrameBuf_ConsumerAbort 业务断连后停止后续帧。
func TestFrameBuf_ConsumerAbort(t *testing.T) {
	ctx := &ConnContext{}
	calls := 0
	ok := ctx.feedFrames(concat(frame(4), frame(4)), 65536, 4096, func([]byte) bool {
		calls++
		return false
	})
	if ok || calls != 1 {
		t.Fatalf("ok=%v calls=%d, want false/1", ok, calls)
	}
}
