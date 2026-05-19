package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

var (
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procSetProcessAffinity = modkernel32.NewProc("SetProcessAffinityMask")
)

func setProcessAffinity(mask uintptr) {
	handle, _ := syscall.GetCurrentProcess()
	procSetProcessAffinity.Call(uintptr(handle), mask)
	_ = handle
}

func main() {
	runtime.GOMAXPROCS(4)

	numConns := 500
	reqsPerConn := 1000
	pipeline := 128
	addr := "localhost:48080"
	serverID := "S1"

	if len(os.Args) > 1 {
		fmt.Sscanf(os.Args[1], "%d", &numConns)
	}
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &reqsPerConn)
	}
	if len(os.Args) > 3 {
		fmt.Sscanf(os.Args[3], "%d", &pipeline)
	}
	if len(os.Args) > 4 {
		addr = os.Args[4]
	}
	if len(os.Args) > 5 {
		serverID = os.Args[5]
	}

	route := "test"
	if len(os.Args) > 6 {
		route = os.Args[6]
	}

	msg := &protobuf.Message{
		Route:   route,
		Payload: map[string]string{"data": "1"},
	}
	data, _ := proto.Marshal(msg)
	testFrame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(testFrame[:4], uint32(len(data)))
	copy(testFrame[4:], data)

	handshakeFrame := buildHandshakeFrame(serverID)
	loginFrame := buildLoginFrame()

	var successfulRequests int64
	var failedRequests int64

	conns := make([]net.Conn, numConns)
	batchSize := 50
	for i := 0; i < numConns; i++ {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			fmt.Printf("connect failed: %d %v\n", i, err)
			continue
		}
		tcpConn := conn.(*net.TCPConn)
		tcpConn.SetNoDelay(true)
		tcpConn.SetReadBuffer(262144)
		tcpConn.SetWriteBuffer(262144)
		conns[i] = conn

		if (i+1)%batchSize == 0 {
			fmt.Printf("  connected: %d / %d\n", i+1, numConns)
		}

		conn.Write(handshakeFrame)
		var h [4]byte
		io.ReadFull(conn, h[:])
		hsz := binary.BigEndian.Uint32(h[:])
		if hsz > 0 && hsz < 4096 {
			tmp := make([]byte, hsz)
			io.ReadFull(conn, tmp)
		}

		conn.Write(loginFrame)
		io.ReadFull(conn, h[:])
		hsz = binary.BigEndian.Uint32(h[:])
		if hsz > 0 && hsz < 4096 {
			tmp := make([]byte, hsz)
			io.ReadFull(conn, tmp)
		}
	}

	fmt.Printf("  all connected: %d\n", numConns)
	time.Sleep(100 * time.Millisecond)

	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < numConns; i++ {
		if conns[i] == nil {
			continue
		}
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()

			sent := 0
			recv := 0
			pending := 0
			msgLen := len(testFrame)
			batchBuf := make([]byte, 0, msgLen*pipeline)
			discardBuf := make([]byte, 256)

			for sent < reqsPerConn || recv < reqsPerConn {
				batchBuf = batchBuf[:0]
				batchCount := 0
				for pending < pipeline && sent < reqsPerConn {
					batchBuf = append(batchBuf, testFrame...)
					sent++
					pending++
					batchCount++
				}
				if batchCount > 0 {
					conn.Write(batchBuf)
				}

				conn.SetReadDeadline(time.Now().Add(30 * time.Second))
				for i := 0; i < batchCount && recv < reqsPerConn; i++ {
					var header [4]byte
					if _, err := io.ReadFull(conn, header[:]); err != nil {
						atomic.AddInt64(&failedRequests, int64(reqsPerConn-recv))
						return
					}
					size := int(binary.BigEndian.Uint32(header[:]))
					if size > 0 && size <= len(discardBuf) {
						io.ReadFull(conn, discardBuf[:size])
					} else if size > 0 {
						tmp := make([]byte, size)
						io.ReadFull(conn, tmp)
					}
					recv++
					pending--
				}
			}
			atomic.AddInt64(&successfulRequests, int64(recv))
		}(conns[i])
	}

	wg.Wait()
	elapsed := time.Since(start)

	totalReq := int64(numConns) * int64(reqsPerConn)
	qps := float64(atomic.LoadInt64(&successfulRequests)) / elapsed.Seconds()

	fmt.Printf("Conns=%d Reqs=%d Pipeline=%d\n", numConns, reqsPerConn, pipeline)
	fmt.Printf("Success=%d Failed=%d\n", atomic.LoadInt64(&successfulRequests), atomic.LoadInt64(&failedRequests))
	fmt.Printf("Duration=%v\n", elapsed)
	fmt.Printf("QPS=%.0f\n", qps)
	fmt.Printf("TotalQPS=%.0f\n", float64(totalReq)/elapsed.Seconds())

	for _, conn := range conns {
		if conn != nil {
			conn.Close()
		}
	}
}

func buildHandshakeFrame(serverID string) []byte {
	msg := &protobuf.Message{
		Route: "handshake",
		Payload: map[string]string{
			"version":   "2.0.0",
			"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
			"serverId":  serverID,
		},
		ProtocolVersion: "2.0.0",
	}
	data, _ := proto.Marshal(msg)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}

func buildLoginFrame() []byte {
	msg := &protobuf.Message{
		Route: "login",
		Payload: map[string]string{
			"userId":   "loadtest",
			"token":    "test-token",
			"serverId": "S1",
		},
	}
	data, _ := proto.Marshal(msg)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}
