package codec

import "testing"

func TestWebSocketEncodeLengths(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 65535, 65536} {
		payload := make([]byte, size)
		encoded := NewWebSocketCodec().Encode(payload)
		if len(encoded) != size+2 && size < 126 {
			t.Fatalf("size %d: unexpected frame length %d", size, len(encoded))
		}
		if size >= 126 && size <= 65535 && len(encoded) != size+4 {
			t.Fatalf("size %d: unexpected extended frame length %d", size, len(encoded))
		}
		if size > 65535 && len(encoded) != size+10 {
			t.Fatalf("size %d: unexpected long frame length %d", size, len(encoded))
		}
		if encoded[0] != 0x82 {
			t.Fatalf("size %d: expected binary final frame", size)
		}
	}
}
