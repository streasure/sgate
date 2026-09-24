package connection

import "encoding/binary"

// EncodeWSFrame 构造 WebSocket 帧（服务端发送，FIN=1, 无 mask）
func EncodeWSFrame(opCode byte, payload []byte) []byte {
	n := len(payload)
	switch {
	case n < 126:
		frame := make([]byte, 0, 2+n)
		frame = append(frame, opCode, byte(n))
		return append(frame, payload...)
	case n <= 65535:
		frame := make([]byte, 0, 4+n)
		frame = append(frame, opCode, 126, byte(n>>8), byte(n))
		return append(frame, payload...)
	default:
		frame := make([]byte, 0, 10+n)
		frame = append(frame, opCode, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		frame = append(frame, b[:]...)
		return append(frame, payload...)
	}
}
