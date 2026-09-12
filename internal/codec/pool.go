package codec

// PutDecodeBuffer returns a decode buffer to the pool after use.
func PutDecodeBuffer(buf *[]byte) {
	if cap(*buf) == 64*1024 {
		decodeBufPool.Put(buf)
	}
}

// PutWSDecodeBuffer returns a WebSocket decode buffer to the pool after use.
func PutWSDecodeBuffer(buf *[]byte) {
	if cap(*buf) == 64*1024 {
		wsDecodeBufPool.Put(buf)
	}
}
