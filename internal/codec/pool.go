package codec

// PutDecodeBuffer 将使用完毕的解码缓冲区归还到对象池。
func PutDecodeBuffer(buf *[]byte) {
	if cap(*buf) == 64*1024 {
		decodeBufPool.Put(buf)
	}
}

// PutWSDecodeBuffer 将使用完毕的 WebSocket 解码缓冲区归还到对象池。
func PutWSDecodeBuffer(buf *[]byte) {
	if cap(*buf) == 64*1024 {
		wsDecodeBufPool.Put(buf)
	}
}
