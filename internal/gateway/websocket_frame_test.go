package gateway

import (
	"bytes"
	"testing"
)

// 构造一个 masked FIN binary 帧：FIN+op=2, MASK+len, 4B mask + payload
func buildMaskedFrame(fin bool, op byte, payload []byte) []byte {
	if len(payload) >= 65536 {
		return nil
	}
	b0 := op & 0x0F
	if fin {
		b0 |= 0x80
	}
	n := len(payload)
	var out []byte
	out = append(out, b0)
	if n < 126 {
		out = append(out, 0x80|byte(n))
	} else {
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	}
	mask := [4]byte{0x01, 0x02, 0x03, 0x04}
	out = append(out, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

func TestParseWebSocketFrame_FinAndMask(t *testing.T) {
	payload := []byte("hello")
	buf := buildMaskedFrame(true, 0x2, payload)
	op, got, size, fin, masked, err := parseWebSocketFrame(buf, 4096)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if op != 0x2 || !fin || !masked {
		t.Fatalf("op=%#x fin=%v masked=%v", op, fin, masked)
	}
	if size != len(buf) {
		t.Fatalf("size=%d want %d", size, len(buf))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload=%q", got)
	}
}

func TestParseWebSocketFrame_FragmentFin(t *testing.T) {
	buf := buildMaskedFrame(false, 0x2, []byte("par"))
	_, _, _, fin, _, err := parseWebSocketFrame(buf, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if fin {
		t.Fatal("expected FIN=0")
	}
}

func TestParseWebSocketFrame_UnmaskedFlag(t *testing.T) {
	// 手工构造 unmasked 帧：FIN+bin, len=3 (无 mask 位)
	buf := []byte{0x82, 0x03, 'a', 'b', 'c'}
	_, _, _, _, masked, err := parseWebSocketFrame(buf, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if masked {
		t.Fatal("expected masked=false")
	}
}
