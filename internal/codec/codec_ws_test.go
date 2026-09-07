package codec

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// mockConn implements gnet.Conn for unit testing.
type mockConn struct {
	mu        sync.Mutex
	inbound   []byte
	outbound  []byte
	discarded int
	closed    bool
}

func newMockConn() *mockConn { return &mockConn{} }

func (m *mockConn) Read(p []byte) (int, error)                  { return 0, io.ErrNoProgress }
func (m *mockConn) ReadFrom(r io.Reader) (int64, error)          { return 0, nil }
func (m *mockConn) WriteTo(w io.Writer) (int64, error)           { return 0, nil }
func (m *mockConn) Write(p []byte) (int, error)                 { m.mu.Lock(); defer m.mu.Unlock(); m.outbound = append(m.outbound, p...); return len(p), nil }
func (m *mockConn) Close() error                                { m.closed = true; return nil }
func (m *mockConn) LocalAddr() net.Addr                         { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr                        { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345} }
func (m *mockConn) SetDeadline(t time.Time) error               { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error           { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error          { return nil }
func (m *mockConn) InboundBuffered() int                        { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inbound) - m.discarded }
func (m *mockConn) Peek(n int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	avail := m.inbound[m.discarded:]
	if n < 0 {
		n = len(avail)
	}
	if len(avail) == 0 {
		return nil, nil
	}
	if len(avail) < n {
		return nil, io.ErrShortBuffer
	}
	return avail[:n], nil
}
func (m *mockConn) Next(n int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.inbound)-m.discarded < n {
		return nil, io.ErrShortBuffer
	}
	data := m.inbound[m.discarded : m.discarded+n]
	m.discarded += n
	return data, nil
}
func (m *mockConn) Discard(n int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discarded += n
	return n, nil
}
func (m *mockConn) AsyncWrite(buf []byte, callback gnet.AsyncCallback) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outbound = append(m.outbound, buf...)
	if callback != nil {
		return callback(m, nil)
	}
	return nil
}
func (m *mockConn) AsyncWritev(bs [][]byte, callback gnet.AsyncCallback) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range bs {
		m.outbound = append(m.outbound, b...)
	}
	if callback != nil {
		return callback(m, nil)
	}
	return nil
}
func (m *mockConn) Writev(bs [][]byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range bs {
		m.outbound = append(m.outbound, b...)
		n += len(b)
	}
	return n, nil
}
func (m *mockConn) Flush() error                                { return nil }
func (m *mockConn) OutboundBuffered() int                       { return 0 }
func (m *mockConn) SendTo(buf []byte, addr net.Addr) (int, error) { return 0, nil }
func (m *mockConn) Context() any                                { return nil }
func (m *mockConn) SetContext(ctx any)                          {}
func (m *mockConn) EventLoop() gnet.EventLoop                   { return nil }
func (m *mockConn) Wake(callback gnet.AsyncCallback) error      { return nil }
func (m *mockConn) CloseWithCallback(cb gnet.AsyncCallback) error { return m.Close() }
func (m *mockConn) Fd() int                                     { return 0 }
func (m *mockConn) Dup() (int, error)                           { return 0, nil }
func (m *mockConn) SetReadBuffer(size int) error                { return nil }
func (m *mockConn) SetWriteBuffer(size int) error               { return nil }
func (m *mockConn) SetLinger(secs int) error                    { return nil }
func (m *mockConn) SetKeepAlivePeriod(d time.Duration) error    { return nil }
func (m *mockConn) SetKeepAlive(enabled bool, idle, intvl time.Duration, cnt int) error { return nil }
func (m *mockConn) SetNoDelay(noDelay bool) error               { return nil }

func (m *mockConn) feed(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inbound = append(m.inbound, data...)
}

func (m *mockConn) readOutbound() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.outbound
	m.outbound = nil
	return out
}

// --- helpers ---

func buildUpgradeRequest(path string, headers map[string]string) []byte {
	var buf bytes.Buffer
	if path == "" {
		path = "/"
	}
	buf.WriteString("GET " + path + " HTTP/1.1\r\nHost: localhost\r\n")
	for k, v := range headers {
		buf.WriteString(k + ": " + v + "\r\n")
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func wsKey() string {
	var key [16]byte
	rand.Read(key[:])
	return base64.StdEncoding.EncodeToString(key[:])
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func buildMaskedBinaryFrame(payload []byte) []byte {
	var mask [4]byte
	rand.Read(mask[:])
	var header []byte
	n := len(payload)
	if n < 126 {
		header = []byte{0x82, byte(0x80 | n)}
	} else if n <= 0xffff {
		header = []byte{0x82, 0x80 | 126, byte(n >> 8), byte(n)}
	} else {
		header = []byte{0x82, 0x80 | 127}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	header = append(header, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	return append(header, masked...)
}

func buildMaskedControlFrame(opcode byte, payload []byte) []byte {
	var mask [4]byte
	rand.Read(mask[:])
	if len(payload) > 125 {
		payload = payload[:125]
	}
	header := []byte{0x80 | opcode, byte(0x80 | len(payload))}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	return append(header, masked...)
}

func buildMaskedBinaryFragment(opcode byte, fin bool, payload []byte) []byte {
	var mask [4]byte
	rand.Read(mask[:])
	n := len(payload)
	var hdr byte = opcode
	if fin {
		hdr |= 0x80
	}
	var frame []byte
	if n < 126 {
		frame = []byte{hdr, byte(0x80 | n)}
	} else if n <= 0xffff {
		frame = []byte{hdr, 0x80 | 126, byte(n >> 8), byte(n)}
	} else {
		frame = []byte{hdr, 0x80 | 127}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		frame = append(frame, b[:]...)
	}
	frame = append(frame, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	return append(frame, masked...)
}

func completeUpgrade(t *testing.T, codec *WebSocketCodec, conn *mockConn) {
	t.Helper()
	key := wsKey()
	conn.feed(buildUpgradeRequest("/", map[string]string{
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key":     key,
	}))
	_, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	conn.readOutbound()
}

// --- Upgrade tests ---

func TestUpgradeValid(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	key := wsKey()

	conn.feed(buildUpgradeRequest("/", map[string]string{
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key":     key,
	}))

	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages after upgrade, got %d", len(msgs))
	}
	if !codec.upgraded {
		t.Fatal("codec should be upgraded")
	}

	out := conn.readOutbound()
	if !bytes.Contains(out, []byte("101 Switching Protocols")) {
		t.Fatal("expected 101 response")
	}
	expectedAccept := wsAccept(key)
	if !bytes.Contains(out, []byte(expectedAccept)) {
		t.Fatalf("expected Sec-WebSocket-Accept: %s", expectedAccept)
	}
}

func TestUpgradeHalfPacket(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()

	conn.feed([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n"))
	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 || codec.upgraded {
		t.Fatal("should not be upgraded yet")
	}

	conn.feed([]byte("Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + wsKey() + "\r\n\r\n"))
	msgs, err = codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if !codec.upgraded {
		t.Fatal("codec should be upgraded after completing headers")
	}
}

func TestUpgradeMissingWebSocketKey(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()

	conn.feed(buildUpgradeRequest("/", map[string]string{
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-WebSocket-Version": "13",
	}))

	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for missing Sec-WebSocket-Key")
	}
}

func TestUpgradeMissingUpgradeHeader(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()

	conn.feed(buildUpgradeRequest("/", map[string]string{
		"Connection":            "Upgrade",
		"Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key":     wsKey(),
	}))

	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for missing Upgrade header")
	}
}

func TestUpgradeNotGET(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()

	req := []byte("POST / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + wsKey() + "\r\n\r\n")
	conn.feed(req)

	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for non-GET request")
	}
}

func TestUpgradeOversizedHeaders(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()

	bigValue := make([]byte, 20*1024)
	rand.Read(bigValue)
	req := []byte("GET / HTTP/1.1\r\nHost: localhost\r\nX-Big: " + string(bigValue) + "\r\n\r\n")
	conn.feed(req)

	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for oversized headers")
	}
}

func TestUpgradeXForwardedFor(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	key := wsKey()

	conn.feed(buildUpgradeRequest("/", map[string]string{
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key":     key,
		"X-Forwarded-For":       "10.0.0.1, 192.168.1.1",
	}))

	_, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if codec.GetIP() != "10.0.0.1" {
		t.Fatalf("expected IP 10.0.0.1, got %s", codec.GetIP())
	}
}

// --- Malformed frame tests ---

func TestMalformedFrameUnmaskedClient(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed([]byte{0x82, 0x05, 'h', 'e', 'l', 'l', 'o'})
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for unmasked client frame")
	}
}

func TestMalformedFrameExtensions(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed([]byte{0xf2, 0x85, 0, 0, 0, 0, 'h', 'e', 'l', 'l', 'o'})
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for frames with extensions")
	}
}

func TestMalformedFrameOversized(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodecWithLimit(1024)
	completeUpgrade(t, codec, conn)

	payload := make([]byte, 2000)
	masked := buildMaskedBinaryFrame(payload)
	conn.feed(masked)
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

func TestMalformedControlFrameNoFIN(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	// Build a ping control frame WITHOUT FIN bit (opcode=0x9, FIN=0)
	var mask [4]byte
	rand.Read(mask[:])
	frame := []byte{0x09, byte(0x80 | 4)} // FIN=0, opcode=ping, masked, len=4
	frame = append(frame, mask[:]...)
	frame = append(frame, 'p', 'i', 'n', 'g')
	conn.feed(frame)
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for control frame without FIN")
	}
}

func TestMalformedUnsupportedOpcode(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	var mask [4]byte
	rand.Read(mask[:])
	frame := []byte{0x83, byte(0x80 | 1)}
	frame = append(frame, mask[:]...)
	frame = append(frame, 'x')
	conn.feed(frame)
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for unsupported opcode")
	}
}

// --- Fragmentation tests ---

func TestFragmentationBinaryMultiFrame(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedBinaryFragment(0x2, false, []byte("hello ")))
	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatal("expected no messages for first fragment")
	}

	conn.feed(buildMaskedBinaryFragment(0x0, true, []byte("world")))
	msgs, err = codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !bytes.Equal(msgs[0], []byte("hello world")) {
		t.Fatalf("expected 'hello world', got %q", msgs)
	}
}

func TestFragmentationThreeFrames(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedBinaryFragment(0x2, false, []byte("a")))
	conn.feed(buildMaskedBinaryFragment(0x0, false, []byte("b")))
	conn.feed(buildMaskedBinaryFragment(0x0, true, []byte("c")))

	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || string(msgs[0]) != "abc" {
		t.Fatalf("expected 'abc', got %q", msgs)
	}
}

func TestFragmentationOversized(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodecWithLimit(100)
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedBinaryFragment(0x2, false, make([]byte, 80)))
	_, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}

	conn.feed(buildMaskedBinaryFragment(0x0, true, make([]byte, 50)))
	_, err = codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for oversized fragmented message")
	}
}

func TestFragmentationInterleavedDataFrames(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedBinaryFragment(0x2, false, []byte("a")))
	_, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}

	conn.feed(buildMaskedBinaryFrame([]byte("x")))
	_, err = codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for interleaved data frames")
	}
}

func TestFragmentationTextRejected(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	var mask [4]byte
	rand.Read(mask[:])
	text := []byte("hello")
	frame := []byte{0x81, byte(0x80 | len(text))}
	frame = append(frame, mask[:]...)
	for i, b := range text {
		frame = append(frame, b^mask[i&3])
	}
	conn.feed(frame)
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for text websocket message")
	}
}

func TestContinuationWithoutStart(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedBinaryFragment(0x0, true, []byte("orphan")))
	_, err := codec.Decode(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for orphan continuation frame")
	}
}

// --- Control frame tests ---

func TestPingPong(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedControlFrame(0x9, []byte("ping")))
	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatal("ping should not produce a message")
	}

	out := conn.readOutbound()
	if len(out) < 2 || out[0] != 0x8a {
		t.Fatalf("expected pong frame, got %x", out)
	}
}

func TestCloseFrame(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	conn.feed(buildMaskedControlFrame(0x8, []byte{0x03, 0xe8}))
	_, err := codec.Decode(context.Background(), conn)
	if err != ErrWebSocketClose {
		t.Fatalf("expected ErrWebSocketClose, got %v", err)
	}
}

// --- Encode tests ---

func TestEncodeSmallPayload(t *testing.T) {
	codec := NewWebSocketCodec()
	encoded := codec.Encode([]byte("hi"))
	if encoded[0] != 0x82 {
		t.Fatal("expected binary opcode")
	}
	if encoded[1] != 2 {
		t.Fatalf("expected length 2, got %d", encoded[1])
	}
	if !bytes.Equal(encoded[2:], []byte("hi")) {
		t.Fatal("payload mismatch")
	}
}

func TestEncode16BitLength(t *testing.T) {
	codec := NewWebSocketCodec()
	payload := make([]byte, 300)
	encoded := codec.Encode(payload)
	if encoded[1] != 126 {
		t.Fatalf("expected extended length indicator 126, got %d", encoded[1])
	}
	actualLen := binary.BigEndian.Uint16(encoded[2:4])
	if actualLen != 300 {
		t.Fatalf("expected 300, got %d", actualLen)
	}
}

func TestEncode64BitLength(t *testing.T) {
	codec := NewWebSocketCodec()
	payload := make([]byte, 70000)
	encoded := codec.Encode(payload)
	if encoded[1] != 127 {
		t.Fatalf("expected long length indicator 127, got %d", encoded[1])
	}
	actualLen := binary.BigEndian.Uint64(encoded[2:10])
	if actualLen != 70000 {
		t.Fatalf("expected 70000, got %d", actualLen)
	}
}

// --- Roundtrip test ---

func TestEncodeDecodeRoundtrip(t *testing.T) {
	conn := newMockConn()
	codec := NewWebSocketCodec()
	completeUpgrade(t, codec, conn)

	original := []byte("roundtrip test data 12345")
	conn.feed(buildMaskedBinaryFrame(original))
	msgs, err := codec.Decode(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !bytes.Equal(msgs[0], original) {
		t.Fatalf("roundtrip failed: expected %q, got %q", original, msgs)
	}
}
