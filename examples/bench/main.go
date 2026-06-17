package main

import (
	"encoding/binary"
	"fmt"
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
	sendFrame []byte
	batchSend map[int][]byte
)

func buildSendFrame() {
	msg := &protobuf.Message{
		Route:   protobuf.RouteTest,
		Payload: map[string]string{"data": "1"},
	}
	data, _ := proto.Marshal(msg)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	sendFrame = frame

	batchSend = make(map[int][]byte)
	for _, n := range []int{1, 2, 4, 8, 16, 32, 64} {
		buf := make([]byte, len(frame)*n)
		for i := 0; i < n; i++ {
			copy(buf[i*len(frame):], frame)
		}
		batchSend[n] = buf
	}
}

type benchConn struct {
	conn *net.TCPConn
}

func newBenchConn(addr string) (*benchConn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	tcpConn := conn.(*net.TCPConn)
	tcpConn.SetNoDelay(true)
	tcpConn.SetReadBuffer(1 * 1024 * 1024)
	tcpConn.SetWriteBuffer(1 * 1024 * 1024)
	return &benchConn{conn: tcpConn}, nil
}

func (bc *benchConn) close() {
	bc.conn.Close()
}

func runFullDuplexConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		return
	}
	defer bc.close()

	var inflight int64
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 512*1024)
		var head, tail int
		for {
			if tail == cap(buf) {
				if head > 0 && head < tail {
					copy(buf, buf[head:tail])
					tail -= head
					head = 0
				} else {
					head = 0
					tail = 0
				}
			}
			n, err := bc.conn.Read(buf[tail:])
			if err != nil {
				return
			}
			tail += n

			count := 0
			for head+4 <= tail {
				fl := int(binary.BigEndian.Uint32(buf[head : head+4]))
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
				copy(buf, buf[head:tail])
				tail -= head
				head = 0
			} else if head >= tail {
				head = 0
				tail = 0
			}
		}
	}()

	batch := batchSend[16]
	for {
		select {
		case <-stopCh:
			bc.conn.Close()
			<-recvDone
			return
		default:
		}
		for atomic.LoadInt64(&inflight) > maxInflight {
			runtime.Gosched()
		}
		if _, err := bc.conn.Write(batch); err != nil {
			return
		}
		atomic.AddInt64(&totalSent, 16)
		atomic.AddInt64(&inflight, 16)
	}
}

func runPipelineConn(addr string, wg *sync.WaitGroup, pipelineDepth int, stopCh chan struct{}) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		return
	}
	defer bc.close()

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 512*1024)
		var head, tail int
		for {
			if tail == cap(buf) {
				if head > 0 && head < tail {
					copy(buf, buf[head:tail])
					tail -= head
					head = 0
				} else {
					head = 0
					tail = 0
				}
			}
			n, err := bc.conn.Read(buf[tail:])
			if err != nil {
				return
			}
			tail += n
			count := 0
			for head+4 <= tail {
				fl := int(binary.BigEndian.Uint32(buf[head : head+4]))
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
			}
			if head > 0 && head < tail {
				copy(buf, buf[head:tail])
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
			bc.conn.Close()
			<-recvDone
			return
		default:
		}

		for i := 0; i < pipelineDepth; i++ {
			if _, err := bc.conn.Write(sendFrame); err != nil {
				return
			}
		}
		atomic.AddInt64(&totalSent, int64(pipelineDepth))
	}
}

func main() {
	addr := "127.0.0.1:48080"
	if len(os.Args) >= 2 {
		addr = os.Args[1]
	}
	conns := 200
	if len(os.Args) >= 3 {
		fmt.Sscanf(os.Args[2], "%d", &conns)
	}
	duration := 10
	if len(os.Args) >= 4 {
		fmt.Sscanf(os.Args[3], "%d", &duration)
	}
	pipeline := 16
	if len(os.Args) >= 5 {
		fmt.Sscanf(os.Args[4], "%d", &pipeline)
	}
	mode := "duplex"
	if len(os.Args) >= 6 {
		mode = os.Args[5]
	}
	inflight := int64(4096)
	if len(os.Args) >= 7 {
		fmt.Sscanf(os.Args[6], "%d", &inflight)
	}

	buildSendFrame()

	fmt.Printf("Benchmark: addr=%s conns=%d duration=%ds pipeline=%d mode=%s\n",
		addr, conns, duration, pipeline, mode)
	fmt.Printf("CPU cores: %d\n", runtime.NumCPU())
	fmt.Printf("Send frame size: %d bytes\n", len(sendFrame))

	runtime.GOMAXPROCS(runtime.NumCPU())

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < conns; i++ {
		wg.Add(1)
		switch mode {
		case "fire", "duplex":
			go runFullDuplexConn(addr, &wg, stopCh, inflight)
		case "pipeline":
			go runPipelineConn(addr, &wg, pipeline, stopCh)
		}
		time.Sleep(time.Microsecond * 50)
	}

	fmt.Println("All connections established, benchmarking...")
	startTime := time.Now()
	var lastRecv int64
	lastPrint := startTime

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(startTime).Seconds()
			curRecv := atomic.LoadInt64(&totalRecv)

			intervalRecv := curRecv - lastRecv
			intervalSec := now.Sub(lastPrint).Seconds()

			qps := float64(intervalRecv) / intervalSec
			avgQPS := float64(curRecv) / elapsed

			fmt.Printf("[%.0fs] QPS: %.0f | Avg QPS: %.0f | Sent: %d | Recv: %d | Pending: %d\n",
				elapsed, qps, avgQPS, atomic.LoadInt64(&totalSent), curRecv, atomic.LoadInt64(&totalSent)-curRecv)

			lastRecv = curRecv
			lastPrint = now

			if elapsed >= float64(duration) {
				close(stopCh)
				wg.Wait()

				totalElapsed := time.Since(startTime).Seconds()
				finalQPS := float64(atomic.LoadInt64(&totalRecv)) / totalElapsed
				fmt.Printf("\n=== Final Results ===\n")
				fmt.Printf("Total Sent: %d\n", atomic.LoadInt64(&totalSent))
				fmt.Printf("Total Recv: %d\n", atomic.LoadInt64(&totalRecv))
				fmt.Printf("Duration: %.2fs\n", totalElapsed)
				fmt.Printf("Average QPS: %.0f\n", finalQPS)
				return
			}
		}
	}
}
