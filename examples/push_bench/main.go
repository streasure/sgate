// Package main benchmarks push patterns with optimized batching.
//
// Usage:
//
//	go run . <addr> <conns> <duration> <mode> [batchSize] [inflight]
//
// Modes:
//
//	personal  - each client sends batched ChatMsg, receives push back
//	group     - N clients send ChatMsg with group target_id, logic handles routing
//	broadcast - each client sends batched ChatMsg, all receive
package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	protoLogic "github.com/streasure/protocol/logic"
	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

const (
	cmdChatMsg      int32 = 1100008
	cmdPushNotify   int32 = 1100006
	cmdLoginGate    int32 = 1000001
	cmdLoginGateAck int32 = 1000002
	cmdLogicLogin   int32 = 1100001
)

var (
	totalSent int64
	totalRecv int64
)

var drainBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 4*1024*1024) },
}

func buildSingleFrame(cmd int32, body proto.Message) []byte {
	data, _ := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: mustMarshal(body)})
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}

func mustMarshal(msg proto.Message) []byte {
	data, _ := proto.Marshal(msg)
	return data
}

func buildBatchFrame(cmd int32, body proto.Message, batchSize int) []byte {
	single := buildSingleFrame(cmd, body)
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

	buf := make([]byte, 65536)
	userID := fmt.Sprintf("u_%d_%d_%d", mode[0], clientID, rand.Int63())
	gateBody, _ := proto.Marshal(&protoGw.LoginGateReq{ServerId: "logic-1", UserId: userID})
	conn.Write(buildRawFrame(cmdLoginGate, gateBody))
	if !readOneFrame(conn, buf) {
		return
	}

	loginBody, _ := proto.Marshal(&protoLogic.LoginReq{UserId: userID})
	loginFrame := buildRawFrame(cmdLogicLogin, loginBody)
	conn.Write(loginFrame)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	totalRead := 0
	for totalRead < 4 {
		n, err := conn.Read(buf[totalRead:])
		if err != nil || n == 0 {
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

	var sendFrame []byte
	switch mode {
	case "group":
		sendFrame = buildBatchFrame(cmdChatMsg, &protoLogic.ChatMsg{Content: "hello", TargetId: "bench_group"}, batchSize)
	case "broadcast":
		sendFrame = buildBatchFrame(cmdChatMsg, &protoLogic.ChatMsg{Content: "hello all"}, batchSize)
	default:
		sendFrame = buildBatchFrame(cmdPushNotify, &protoLogic.PushNotify{Content: "hello"}, batchSize)
	}

	var inflight int64

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

func readOneFrame(conn net.Conn, buf []byte) bool {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	total := 0
	for total < 4 {
		n, err := conn.Read(buf[total:])
		if err != nil || n == 0 {
			return false
		}
		total += n
	}
	frameLen := binary.BigEndian.Uint32(buf[:4])
	for total < 4+int(frameLen) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return false
		}
		total += n
	}
	conn.SetReadDeadline(time.Time{})
	return true
}

func buildRawFrame(cmd int32, body []byte) []byte {
	data, _ := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: body})
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
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
