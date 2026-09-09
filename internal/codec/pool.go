package codec

// ReleaseDecodeBuffer returns a buffer to the pool after use.
// Call this when the buffer is no longer needed.
func ReleaseDecodeBuffer(buf []byte) {
	if cap(buf) == 64*1024 {
		// Return to pool if it's a pooled buffer
		select {
		case decodeBufPool <- &buf:
		default:
			// Pool is full, let GC collect it
		}
	}
}

// ReleaseWSDecodeBuffer returns a WebSocket buffer to the pool after use.
func ReleaseWSDecodeBuffer(buf []byte) {
	if cap(buf) == 64*1024 {
		select {
		case wsDecodeBufPool <- &buf:
		default:
		}
	}
}
