package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/protocol/commonstruct"
	"github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

var (
	totalSent    int64
	totalRecv    int64
	totalDropped int64
	sendFrame    []byte
	batchSend    map[int][]byte
)

// drainBufPool 复用读取缓冲区，避免高并发下大量 4MB 分配导致 Go runtime heap 扩展失败
var drainBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 1024*1024) }, // 1MB
}

func buildSendFrame() {
	body := &gateway.StreamData{
		Route:   gateway.RouteTest,
		Payload: map[string]string{"data": "1"},
	}
	bodyData, _ := proto.Marshal(body)
	data, _ := proto.Marshal(&gateway.MessageFrame{
		Cmd:  gateway.CmdForRoute(gateway.RouteTest),
		Body: bodyData,
	})
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	sendFrame = frame

	batchSend = make(map[int][]byte)
}

func buildTCPBatch(n int) []byte {
	if n <= 0 {
		n = 1
	}
	if batch, ok := batchSend[n]; ok {
		return batch
	}
	batch := make([]byte, len(sendFrame)*n)
	for i := 0; i < n; i++ {
		copy(batch[i*len(sendFrame):], sendFrame)
	}
	batchSend[n] = batch
	return batch
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
	tcpConn.SetReadBuffer(64 * 1024 * 1024) // 64MB kernel recv buffer
	tcpConn.SetWriteBuffer(8 * 1024 * 1024) // 8MB kernel send buffer
	return &benchConn{conn: tcpConn}, nil
}

func (bc *benchConn) close() {
	bc.conn.Close()
}

// authenticate 发送 handshake + login 使连接通过认证
func (bc *benchConn) authenticate(clientID int) error {
	hs := &commonstruct.Handshake{
		ProtocolVersion: "1.0",
		ClientType:      "bench",
		Timestamp:       time.Now().UnixMilli(),
	}
	hsData, _ := proto.Marshal(hs)
	body := &gateway.StreamData{
		Route:   gateway.RouteHandshake,
		Payload: map[string]string{"serverId": "bench_server", "handshake_data": base64.StdEncoding.EncodeToString(hsData)},
	}
	bodyData, _ := proto.Marshal(body)
	handshakeData, _ := proto.Marshal(&gateway.MessageFrame{
		Cmd:  gateway.CmdForRoute(gateway.RouteHandshake),
		Body: bodyData,
	})
	handshakeFrame := make([]byte, 4+len(handshakeData))
	binary.BigEndian.PutUint32(handshakeFrame[:4], uint32(len(handshakeData)))
	copy(handshakeFrame[4:], handshakeData)
	if _, err := bc.conn.Write(handshakeFrame); err != nil {
		return err
	}

	// Read handshake response
	bc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65536)
	totalRead := 0
	for totalRead < 4 {
		n, err := bc.conn.Read(buf[totalRead:])
		if err != nil {
			return fmt.Errorf("handshake read: %w", err)
		}
		totalRead += n
	}
	fl := binary.BigEndian.Uint32(buf[:4])
	for totalRead < 4+int(fl) {
		n, err := bc.conn.Read(buf[totalRead:])
		if err != nil {
			return fmt.Errorf("handshake body: %w", err)
		}
		totalRead += n
	}
	bc.conn.SetReadDeadline(time.Time{})

	loginBody := &gateway.StreamData{
		Route:   gateway.RouteLogin,
		Payload: map[string]string{"userId": fmt.Sprintf("bench_%d", clientID)},
	}
	loginBodyData, _ := proto.Marshal(loginBody)
	loginData, _ := proto.Marshal(&gateway.MessageFrame{
		Cmd:  gateway.CmdForRoute(gateway.RouteLogin),
		Body: loginBodyData,
	})
	loginFrame := make([]byte, 4+len(loginData))
	binary.BigEndian.PutUint32(loginFrame[:4], uint32(len(loginData)))
	copy(loginFrame[4:], loginData)
	if _, err := bc.conn.Write(loginFrame); err != nil {
		return err
	}

	bc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	totalRead = 0
	for totalRead < 4 {
		n, err := bc.conn.Read(buf[totalRead:])
		if err != nil {
			return fmt.Errorf("login read: %w", err)
		}
		totalRead += n
	}
	fl = binary.BigEndian.Uint32(buf[:4])
	for totalRead < 4+int(fl) {
		n, err := bc.conn.Read(buf[totalRead:])
		if err != nil {
			return fmt.Errorf("login body: %w", err)
		}
		totalRead += n
	}
	bc.conn.SetReadDeadline(time.Time{})
	return nil
}

// statsResp sgate /stats 返回结构
type statsResp struct {
	Received          int64   `json:"received"`
	Forwarded         int64   `json:"forwarded"`
	DroppedOverload   int64   `json:"droppedOverload"`
	DroppedFull       int64   `json:"droppedFull"`
	DroppedTotal      int64   `json:"droppedTotal"`
	PushedToClient    int64   `json:"pushedToClient"`
	PushDroppedNoConn int64   `json:"pushDroppedNoConn"`
	Overloaded        bool    `json:"overloaded"`
	CPUPercent        float64 `json:"cpuPercent"`
	MemPercent        float64 `json:"memPercent"`
	OverloadDropped   int64   `json:"overloadDropped"`
	ActiveConnections int64   `json:"activeConnections"`
}

func queryStats(statsAddr string) (*statsResp, error) {
	resp, err := http.Get("http://" + statsAddr + "/stats")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var s statsResp
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func runDuplexConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64, clientID int) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		return
	}
	defer bc.close()

	if err := bc.authenticate(clientID); err != nil {
		return
	}

	var inflight int64
	var lastRecvTime time.Time
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := drainBufPool.Get().([]byte)
		defer drainBufPool.Put(buf)
		var head, tail int
		for {
			n, err := bc.conn.Read(buf[tail:])
			if err != nil {
				return
			}
			tail += n
			lastRecvTime = time.Now()

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

	batch := buildTCPBatch(16)
	lastRecvTime = time.Now()
	stallCount := 0

	for {
		select {
		case <-stopCh:
			bc.conn.Close()
			<-recvDone
			return
		default:
		}

		curInflight := atomic.LoadInt64(&inflight)

		if curInflight > maxInflight {
			time.Sleep(time.Millisecond)
			if time.Since(lastRecvTime) > 10*time.Second {
				dropped := atomic.LoadInt64(&inflight)
				atomic.AddInt64(&totalDropped, dropped)
				atomic.AddInt64(&inflight, -dropped)
				stallCount++
				if stallCount > 3 {
					return
				}
			}
			continue
		}

		if _, err := bc.conn.Write(batch); err != nil {
			return
		}
		atomic.AddInt64(&totalSent, 16)
		atomic.AddInt64(&inflight, 16)
		stallCount = 0
	}
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <addr> <conns> [duration] [batchSize] [inflight] [statsAddr] [ratePerConn]\n", os.Args[0])
		os.Exit(1)
	}

	addr := os.Args[1]
	conns := 100
	fmt.Sscanf(os.Args[2], "%d", &conns)
	duration := 10
	if len(os.Args) >= 4 {
		fmt.Sscanf(os.Args[3], "%d", &duration)
	}
	batchSize := 16
	if len(os.Args) >= 5 {
		fmt.Sscanf(os.Args[4], "%d", &batchSize)
	}
	inflight := int64(8192)
	if len(os.Args) >= 6 {
		fmt.Sscanf(os.Args[5], "%d", &inflight)
	}
	statsAddr := "127.0.0.1:8081"
	if len(os.Args) >= 7 {
		statsAddr = os.Args[6]
	}

	buildSendFrame()
	buildTCPBatch(batchSize)

	fmt.Printf("Benchmark: addr=%s conns=%d duration=%ds batchSize=%d inflight=%d statsAddr=%s\n",
		addr, conns, duration, batchSize, inflight, statsAddr)
	fmt.Printf("CPU cores: %d\n", runtime.NumCPU())
	fmt.Printf("Send frame size: %d bytes\n", len(sendFrame))

	runtime.GOMAXPROCS(runtime.NumCPU())

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < conns; i++ {
		wg.Add(1)
		go runDuplexConn(addr, &wg, stopCh, inflight, i)
		time.Sleep(time.Microsecond * 20)
	}

	fmt.Println("All connections established, benchmarking...")
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
			intervalSec := now.Sub(lastPrint).Seconds()
			sendQPS := float64(intervalSent) / intervalSec

			intervalRecv := curRecv - lastRecv
			qps := float64(intervalRecv) / intervalSec
			avgQPS := float64(curRecv) / elapsed

			var pushedStr, pushDropStr string
			if stats, err := queryStats(statsAddr); err == nil {
				pushedStr = fmt.Sprintf(" | PushedToClient: %d", stats.PushedToClient)
				pushDropStr = fmt.Sprintf(" | PushDroppedNoConn: %d", stats.PushDroppedNoConn)
			}

			fmt.Printf("[%.0fs] SendQPS: %.0f | RecvQPS: %.0f | AvgRecvQPS: %.0f | Sent: %d | Recv: %d | Dropped: %d | Pending: %d%s%s\n",
				elapsed, sendQPS, qps, avgQPS, curSent, curRecv, atomic.LoadInt64(&totalDropped), curSent-curRecv-atomic.LoadInt64(&totalDropped), pushedStr, pushDropStr)

			lastRecv = curRecv
			lastSent = curSent
			lastPrint = now

			if elapsed >= float64(duration) {
				close(stopCh)
				wg.Wait()

				totalElapsed := time.Since(startTime).Seconds()
				fmt.Printf("\n=== Final Results ===\n")
				fmt.Printf("Total Sent: %d\n", atomic.LoadInt64(&totalSent))
				fmt.Printf("Total Recv: %d\n", atomic.LoadInt64(&totalRecv))
				fmt.Printf("Total Dropped (bench): %d\n", atomic.LoadInt64(&totalDropped))
				fmt.Printf("Duration: %.2fs\n", totalElapsed)

				finalQPS := float64(atomic.LoadInt64(&totalRecv)) / totalElapsed
				fmt.Printf("Average Recv QPS: %.0f\n", finalQPS)
				if stats, err := queryStats(statsAddr); err == nil {
					fmt.Printf("\n=== sgate Stats ===\n")
					fmt.Printf("Total Received by sgate (client->logic): %d\n", stats.Received)
					fmt.Printf("Total Forwarded (client->logic): %d\n", stats.Forwarded)
					fmt.Printf("Dropped (client->logic): %d\n", stats.DroppedTotal)
					fmt.Printf("Total PushedToClient (logic->client): %d\n", stats.PushedToClient)
					fmt.Printf("PushDroppedNoConn (logic->client): %d\n", stats.PushDroppedNoConn)
					fmt.Printf("Average Push QPS: %.0f\n", float64(stats.PushedToClient)/totalElapsed)
					fmt.Printf("Average Forward QPS: %.0f\n", float64(stats.Forwarded)/totalElapsed)
					if stats.DroppedTotal == 0 && stats.PushDroppedNoConn == 0 {
						fmt.Printf("RESULT: BIDIRECTIONAL SUCCESS (0 drops both ways)\n")
					} else {
						fmt.Printf("RESULT: drops fwd=%d push=%d\n", stats.DroppedTotal, stats.PushDroppedNoConn)
					}
				}
				return
			}
		}
	}
}
