package main

import (
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

	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/gateway"
	"google.golang.org/protobuf/proto"
)

var (
	totalSent       int64
	totalRecv       int64
	totalDropped    int64
	totalAuthFailed int64
	sendFrame       []byte
	batchSend       map[int][]byte
	targetServerID  = "logic-1"
)

// drainBufPool 复用读取缓冲区，避免高并发下大量 4MB 分配导致 Go runtime heap 扩展失败
var drainBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 1024*1024) }, // 1MB
}

func buildSendFrame() {
	heartbeat, _ := proto.Marshal(&protoLogic.HeartbeatReq{ClientTime: time.Now().UnixMilli()})
	data, _ := proto.Marshal(&gateway.MessageFrame{Cmd: gateway.CmdHeartbeatReq, Body: heartbeat})
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

// authenticate sends the logic-owned login request directly after connection setup.
func (bc *benchConn) authenticate(clientID int) error {
	buf := make([]byte, 65536)
	gateBody, _ := proto.Marshal(&gateway.LoginGateReq{ServerId: targetServerID, UserId: fmt.Sprintf("bench_%d", clientID)})
	gateFrame := buildRawFrame(gateway.CmdLoginGate, gateBody)
	if _, err := bc.conn.Write(gateFrame); err != nil {
		return err
	}
	gatePayload, err := readFrame(bc.conn, buf, "login gate")
	if err != nil {
		return err
	}
	var frame gateway.MessageFrame
	var gateAck gateway.LoginGateAck
	if proto.Unmarshal(gatePayload, &frame) != nil || frame.Cmd != gateway.CmdLoginGateAck || proto.Unmarshal(frame.Body, &gateAck) != nil || gateAck.Code != 0 {
		return fmt.Errorf("login gate rejected: code=%d message=%s", gateAck.Code, gateAck.Message)
	}
	loginBody, _ := proto.Marshal(&protoLogic.LoginReq{UserId: fmt.Sprintf("bench_%d", clientID)})
	loginFrame := buildRawFrame(gateway.CmdLogicLoginReq, loginBody)
	if _, err := bc.conn.Write(loginFrame); err != nil {
		return err
	}

	bc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	totalRead := 0
	for totalRead < 4 {
		n, err := bc.conn.Read(buf[totalRead:])
		if err != nil {
			return fmt.Errorf("login read: %w", err)
		}
		totalRead += n
	}
	fl := binary.BigEndian.Uint32(buf[:4])
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

func readFrame(conn net.Conn, buf []byte, name string) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	total := 0
	for total < 4 {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return nil, fmt.Errorf("%s read: %w", name, err)
		}
		total += n
	}
	frameLen := binary.BigEndian.Uint32(buf[:4])
	for total < 4+int(frameLen) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return nil, fmt.Errorf("%s body: %w", name, err)
		}
		total += n
	}
	conn.SetReadDeadline(time.Time{})
	return buf[4 : 4+frameLen], nil
}

func buildRawFrame(cmd int32, body []byte) []byte {
	data, _ := proto.Marshal(&gateway.MessageFrame{Cmd: cmd, Body: body})
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
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
		atomic.AddInt64(&totalAuthFailed, 1)
		return
	}
	defer bc.close()

	if err := bc.authenticate(clientID); err != nil {
		atomic.AddInt64(&totalAuthFailed, 1)
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
		fmt.Fprintf(os.Stderr, "Usage: %s <addr> <conns> [duration] [batchSize] [inflight] [statsAddr] [serverId]\n", os.Args[0])
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
	if len(os.Args) >= 8 {
		targetServerID = os.Args[7]
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

			fmt.Printf("[%.0fs] SendQPS: %.0f | RecvQPS: %.0f | AvgRecvQPS: %.0f | Sent: %d | Recv: %d | Dropped: %d | AuthFailed: %d | Pending: %d%s%s\n",
				elapsed, sendQPS, qps, avgQPS, curSent, curRecv, atomic.LoadInt64(&totalDropped), atomic.LoadInt64(&totalAuthFailed), curSent-curRecv-atomic.LoadInt64(&totalDropped), pushedStr, pushDropStr)

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
				fmt.Printf("Auth Failed: %d\n", atomic.LoadInt64(&totalAuthFailed))
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
