// Package main benchmarks all push patterns with optimized batching.
//
// Usage:
//   go run . <addr> <conns> <duration> <mode> [batchSize] [inflight]
//
// Modes:
//   personal  - each client sends batched "push_me", receives push back
//   group     - N clients join group, batched "group_msg", all receive
//   broadcast - each client sends batched "broadcast_msg", all receive
package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

var (
	totalSent int64
	totalRecv int64
)

var drainBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 4*1024*1024) },
}

func buildSingleFrame(route string, payload map[string]string) []byte {
	msg := &protobuf.Message{
		Route:   route,
		Payload: payload,
	}
	data, _ := proto.Marshal(msg)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}

func buildBatchFrame(route string, payload map[string]string, batchSize int) []byte {
	single := buildSingleFrame(route, payload)
	batch := make([]byte, len(single)*batchSize)
	for i := 0; i < batchSize; i++ {
		copy(batch[i*len(single):], single)
	}
	return batch
}

func runPushConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64, mode string, clientID int, batchSize int) {
	defer wg.Done()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return
	}
	tcpConn := conn.(*net.TCPConn)
	tcpConn.SetNoDelay(true)
	tcpConn.SetReadBuffer(64 * 1024 * 1024)
	tcpConn.SetWriteBuffer(8 * 1024 * 1024)
	defer conn.Close()

	// Handshake: set serverId + handshake_data (requires Timestamp)
	hs := &protobuf.Handshake{
		ProtocolVersion: "1.0",
		ClientType:      "push_bench",
		Timestamp:       time.Now().UnixMilli(),
	}
	hsData, _ := proto.Marshal(hs)
	handshakeFrame := buildSingleFrame(protobuf.RouteHandshake, map[string]string{
		"serverId":       fmt.Sprintf("bench_server_%d", clientID),
		"handshake_data": base64.StdEncoding.EncodeToString(hsData),
	})
	conn.Write(handshakeFrame)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65536)
	totalRead := 0
	for totalRead < 4 {
		n, err := conn.Read(buf[totalRead:])
		if err != nil {
			return
		}
		totalRead += n
	}
	fl := binary.BigEndian.Uint32(buf[:4])
	for totalRead < 4+int(fl) {
		n, err := conn.Read(buf[totalRead:])
		if err != nil {
			return
		}
		totalRead += n
	}
	conn.SetReadDeadline(time.Time{})

	// Login: set userUUID
	loginFrame := buildSingleFrame(protobuf.RouteLogin, map[string]string{
		"userId": fmt.Sprintf("u_%d_%d_%d", mode[0], clientID, rand.Int63()),
	})
	conn.Write(loginFrame)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	totalRead = 0
	for totalRead < 4 {
		n, err := conn.Read(buf[totalRead:])
		if err != nil || n == 0 {
			return
		}
		totalRead += n
	}
	fl = binary.BigEndian.Uint32(buf[:4])
	for totalRead < 4+int(fl) {
		n, err := conn.Read(buf[totalRead:])
		if err != nil {
			return
		}
		totalRead += n
	}
	conn.SetReadDeadline(time.Time{})

	// Mode-specific setup
	switch mode {
	case "group":
		joinFrame := buildSingleFrame("join_group", map[string]string{"groupID": "bench_group"})
		conn.Write(joinFrame)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.Read(buf)
		conn.SetReadDeadline(time.Time{})
		// PushToServer is async — wait for gateway to process server.join_group
		time.Sleep(300 * time.Millisecond)
	}

	var sendFrame []byte
	switch mode {
	case "group":
		sendFrame = buildBatchFrame("group_msg", map[string]string{"groupID": "bench_group", "message": "hello"}, batchSize)
	case "broadcast":
		sendFrame = buildBatchFrame("broadcast_msg", map[string]string{"message": "hello all"}, batchSize)
	default:
		sendFrame = buildBatchFrame("push_me", map[string]string{}, batchSize)
	}

	var inflight int64

	// Receiver goroutine
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		recvBuf := drainBufPool.Get().([]byte)
		defer drainBufPool.Put(recvBuf)
		var head, tail int
		for {
			n, err := conn.Read(recvBuf[tail:])
			if err != nil {
				return
			}
			tail += n
			count := 0
			for head+4 <= tail {
				fl := int(binary.BigEndian.Uint32(recvBuf[head : head+4]))
				if fl == 0 || fl > 16*1024*1024 {
					return
				}
				total := 4 + fl
				if head+total > tail {
					break
				}
				head += total
				count++
			}
			if count > 0 {
				atomic.AddInt64(&totalRecv, int64(count))
				atomic.AddInt64(&inflight, -int64(count))
			}
			if head > 0 && head < tail {
				copy(recvBuf, recvBuf[head:tail])
				tail -= head
				head = 0
			} else if head >= tail {
				head = 0
				tail = 0
			}
		}
	}()

	// Sender loop
	for {
		select {
		case <-stopCh:
			conn.Close()
			<-recvDone
			return
		default:
		}
		if atomic.LoadInt64(&inflight) > maxInflight {
			time.Sleep(time.Millisecond)
			continue
		}
		if _, err := conn.Write(sendFrame); err != nil {
			conn.Close()
			<-recvDone
			return
		}
		atomic.AddInt64(&totalSent, int64(batchSize))
		atomic.AddInt64(&inflight, int64(batchSize))
	}
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <addr> <conns> <duration> [mode] [batchSize] [inflight]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  mode: personal | group | broadcast\n")
		os.Exit(1)
	}

	addr := os.Args[1]
	conns := 100
	fmt.Sscanf(os.Args[2], "%d", &conns)
	duration := 10
	fmt.Sscanf(os.Args[3], "%d", &duration)
	mode := "personal"
	if len(os.Args) >= 5 {
		mode = os.Args[4]
	}
	batchSize := 16
	if len(os.Args) >= 6 {
		fmt.Sscanf(os.Args[5], "%d", &batchSize)
	}
	inflight := int64(5000)
	if len(os.Args) >= 7 {
		fmt.Sscanf(os.Args[6], "%d", &inflight)
	}

	fmt.Printf("Push Bench: addr=%s conns=%d duration=%ds mode=%s batchSize=%d inflight=%d\n",
		addr, conns, duration, mode, batchSize, inflight)
	fmt.Printf("CPU cores: %d\n", runtime.NumCPU())
	runtime.GOMAXPROCS(runtime.NumCPU())

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < conns; i++ {
		wg.Add(1)
		go runPushConn(addr, &wg, stopCh, inflight, mode, i, batchSize)
		time.Sleep(time.Microsecond * 20)
	}

	fmt.Printf("All connections established, benchmarking %s...\n", mode)
	startTime := time.Now()
	var lastSent, lastRecv int64
	lastPrint := startTime

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(startTime).Seconds()
			curSent := atomic.LoadInt64(&totalSent)
			curRecv := atomic.LoadInt64(&totalRecv)

			intervalSent := curSent - lastSent
			intervalRecv := curRecv - lastRecv
			intervalSec := now.Sub(lastPrint).Seconds()

			fmt.Printf("[%.0fs] SendQPS: %.0f | RecvQPS: %.0f | Sent: %d | Recv: %d | Pending: %d\n",
				elapsed,
				float64(intervalSent)/intervalSec,
				float64(intervalRecv)/intervalSec,
				curSent, curRecv,
				curSent-curRecv)

			lastRecv = curRecv
			lastSent = curSent
			lastPrint = now

			if elapsed >= float64(duration) {
				close(stopCh)
				wg.Wait()

				totalElapsed := time.Since(startTime).Seconds()
				fmt.Printf("\n=== Final Results (%s, batchSize=%d) ===\n", mode, batchSize)
				fmt.Printf("Total Sent: %d\n", atomic.LoadInt64(&totalSent))
				fmt.Printf("Total Recv: %d\n", atomic.LoadInt64(&totalRecv))
				fmt.Printf("Duration: %.2fs\n", totalElapsed)
				fmt.Printf("Avg Send QPS: %.0f\n", float64(atomic.LoadInt64(&totalSent))/totalElapsed)
				fmt.Printf("Avg Recv QPS: %.0f\n", float64(atomic.LoadInt64(&totalRecv))/totalElapsed)
				return
			}
		}
	}
}
