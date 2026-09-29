package gateway

import (
	"encoding/binary"
	"testing"

	"github.com/panjf2000/gnet/v2"
)

// frameCaptureConn 捕获 AsyncWrite 的字节。
type frameCaptureConn struct {
	gnet.Conn
	written []byte
}

func (c *frameCaptureConn) AsyncWrite(b []byte, _ gnet.AsyncCallback) error {
	c.written = append(c.written, b...)
	return nil
}

// TestWriteFrameAsync_HasLengthPrefix 回归：writeFrameAsync 必须与 writeFrame 一致，
// 先写 4 字节大端长度前缀（9726a36 曾漏掉导致 bench1_tcp 登录全挂）。
func TestWriteFrameAsync_HasLengthPrefix(t *testing.T) {
	c := &frameCaptureConn{}
	payload := []byte("hello-frame")
	writeFrameAsync(c, payload)

	if len(c.written) != 4+len(payload) {
		t.Fatalf("written len=%d want %d (missing 4B prefix?)", len(c.written), 4+len(payload))
	}
	got := binary.BigEndian.Uint32(c.written[:4])
	if int(got) != len(payload) {
		t.Fatalf("prefix=%d want %d", got, len(payload))
	}
	if string(c.written[4:]) != string(payload) {
		t.Fatalf("payload mismatch: %q", c.written[4:])
	}
}

// TestWriteFrameAsync_NilConn 无连接时不得 panic。
func TestWriteFrameAsync_NilConn(t *testing.T) {
	writeFrameAsync(nil, []byte("x"))
}
