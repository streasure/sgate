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

	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

var (
	totalSent    int64
	totalRecv    int64
	totalDropped int64
	sendFrame    []byte
	batchSend    map[int][]byte
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
	for _, n := range []int{1, 2, 4, 8, 16, 32, 64, 128, 256} {
		buf := make([]byte, len(frame)*n)
		for i := 0; i < n; i++ {
			copy(buf[i*len(frame):], frame)
		}
		batchSend[n] = buf
	}
}

func buildWSBinaryFrame(payload []byte) []byte {
	payloadLen := len(payload)
	var frame []byte
	if payloadLen < 126 {
		frame = make([]byte, 0, 2+payloadLen)
		frame = append(frame, 0x82, byte(payloadLen))
	} else if payloadLen <= 65535 {
		frame = make([]byte, 0, 4+payloadLen)
		frame = append(frame, 0x82, 126)
		frame = append(frame, byte(payloadLen>>8), byte(payloadLen))
	} else {
		frame = make([]byte, 0, 10+payloadLen)
		frame = append(frame, 0x82, 127)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(uint64(payloadLen)>>(uint(i)*8)))
		}
	}
	frame = append(frame, payload...)
	return frame
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

// runForwardConn 转发压测模式：只发送不等待响应，响应在后台被动读取以避免发送缓冲区满
// 通过查询 sgate 的 /stats 端点统计转发 QPS
// inflight 限制基于 bench 本地发送计数，避免无限制发送导致 OOM
func runForwardConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64, useWS bool, batchSize int, ratePerConn int64) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		return
	}
	defer bc.close()

	var batch []byte
	if useWS {
		handshake := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
		bc.conn.Write([]byte(handshake))
		respBuf := make([]byte, 4096)
		bc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := bc.conn.Read(respBuf)
		if err != nil || n == 0 {
			return
		}
		bc.conn.SetReadDeadline(time.Time{})

		protoData, _ := proto.Marshal(&protobuf.Message{
			Route:   protobuf.RouteTest,
			Payload: map[string]string{"data": "1"},
		})
		wsFrame := buildWSBinaryFrame(protoData)
		batch = make([]byte, len(wsFrame)*batchSize)
		for i := 0; i < batchSize; i++ {
			copy(batch[i*len(wsFrame):], wsFrame)
		}
	} else {
		batch = batchSend[batchSize]
	}

	// 后台读取响应以避免内核接收缓冲区满导致 sgate 写阻塞
	// 不设 read deadline：连接在 stopCh 关闭时由 bc.conn.Close() 唤醒退出
	// 设 deadline 会在高负载下因暂时无数据而误判超时退出，导致连接停止 drain →
	// 内核缓冲区满 → sgate 写阻塞 → gnet 关闭连接 → PushDroppedNoConn 飙升
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 4*1024*1024) // 4MB read buffer
		for {
			_, err := bc.conn.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// 速率限制：使用 token bucket
	var rateLimiter *time.Ticker
	if ratePerConn > 0 {
		// 每批发送 batchSize 个消息，所以 ticker 频率 = ratePerConn / batchSize
		interval := time.Duration(float64(batchSize) / float64(ratePerConn) * float64(time.Second))
		if interval < time.Microsecond {
			interval = time.Microsecond
		}
		rateLimiter = time.NewTicker(interval)
		defer rateLimiter.Stop()
	}

	for {
		select {
		case <-stopCh:
			bc.conn.Close()
			<-recvDone
			return
		default:
		}

		if rateLimiter != nil {
			<-rateLimiter.C
		}

		// 无 inflight 控制，尽可能快速发送
		// sgate 的过载保护器会丢弃超过处理能力的消息
		if _, err := bc.conn.Write(batch); err != nil {
			return
		}
		atomic.AddInt64(&totalSent, int64(batchSize))
	}
}

func runFullDuplexConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		return
	}
	defer bc.close()

	var inflight int64
	var lastRecvTime time.Time
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 4*1024*1024) // 4MB
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

	batch := batchSend[16]
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

func runWSFullDuplexConn(addr string, wg *sync.WaitGroup, stopCh chan struct{}, maxInflight int64) {
	defer wg.Done()

	bc, err := newBenchConn(addr)
	if err != nil {
		return
	}
	defer bc.close()

	handshake := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	bc.conn.Write([]byte(handshake))

	respBuf := make([]byte, 4096)
	bc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := bc.conn.Read(respBuf)
	if err != nil || n == 0 {
		return
	}

	protoData, _ := proto.Marshal(&protobuf.Message{
		Route:   protobuf.RouteTest,
		Payload: map[string]string{"data": "1"},
	})
	wsFrame := buildWSBinaryFrame(protoData)

	batchWSFrame := make([]byte, len(wsFrame)*16)
	for i := 0; i < 16; i++ {
		copy(batchWSFrame[i*len(wsFrame):], wsFrame)
	}

	var inflight int64
	var lastRecvTime time.Time
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 4*1024*1024) // 4MB
		var head, tail int
		for {
			n, err := bc.conn.Read(buf[tail:])
			if err != nil {
				return
			}
			tail += n
			lastRecvTime = time.Now()

			count := 0
			for head < tail {
				if buf[head] != 0x82 {
					head++
					continue
				}
				if head+2 > tail {
					break
				}
				pl := int(buf[head+1] & 0x7F)
				frameHdrLen := 2
				if pl == 126 {
					if head+4 > tail {
						break
					}
					pl = int(buf[head+2])<<8 | int(buf[head+3])
					frameHdrLen = 4
				} else if pl == 127 {
					if head+10 > tail {
						break
					}
					pl = 0
					for i := 2; i < 10; i++ {
						pl = pl<<8 | int(buf[head+i])
					}
					frameHdrLen = 10
				}
				totalFrame := frameHdrLen + pl
				if head+totalFrame > tail {
					break
				}
				head += totalFrame
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

		if _, err := bc.conn.Write(batchWSFrame); err != nil {
			return
		}
		atomic.AddInt64(&totalSent, 16)
		atomic.AddInt64(&inflight, 16)
		stallCount = 0
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
	batchSize := 16
	if len(os.Args) >= 5 {
		fmt.Sscanf(os.Args[4], "%d", &batchSize)
	}
	mode := "forward"
	if len(os.Args) >= 6 {
		mode = os.Args[5]
	}
	inflight := int64(8192)
	if len(os.Args) >= 7 {
		fmt.Sscanf(os.Args[6], "%d", &inflight)
	}
	statsAddr := "127.0.0.1:8081"
	if len(os.Args) >= 8 {
		statsAddr = os.Args[7]
	}
	ratePerConn := int64(0) // 0 = 无限制
	if len(os.Args) >= 9 {
		fmt.Sscanf(os.Args[8], "%d", &ratePerConn)
	}

	buildSendFrame()

	useWS := mode == "ws-forward" || mode == "ws" || mode == "websocket"

	fmt.Printf("Benchmark: addr=%s conns=%d duration=%ds batchSize=%d mode=%s inflight=%d statsAddr=%s ratePerConn=%d\n",
		addr, conns, duration, batchSize, mode, inflight, statsAddr, ratePerConn)
	fmt.Printf("CPU cores: %d\n", runtime.NumCPU())
	fmt.Printf("Send frame size: %d bytes\n", len(sendFrame))

	runtime.GOMAXPROCS(runtime.NumCPU())

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < conns; i++ {
		wg.Add(1)
		switch mode {
		case "duplex":
			go runFullDuplexConn(addr, &wg, stopCh, inflight)
		case "ws", "websocket":
			go runWSFullDuplexConn(addr, &wg, stopCh, inflight)
		case "forward", "ws-forward":
			go runForwardConn(addr, &wg, stopCh, inflight, useWS, batchSize, ratePerConn)
		default:
			go runForwardConn(addr, &wg, stopCh, inflight, false, batchSize, ratePerConn)
		}
		time.Sleep(time.Microsecond * 20)
	}

	fmt.Println("All connections established, benchmarking...")
	startTime := time.Now()
	var lastSent, lastForwarded, lastRecv int64
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

			if mode == "forward" || mode == "ws-forward" {
				// 查询 sgate 的转发统计
				if stats, err := queryStats(statsAddr); err == nil {
					curForwarded := stats.Forwarded
					intervalForwarded := curForwarded - lastForwarded
					forwardQPS := float64(intervalForwarded) / intervalSec

					intervalPushed := stats.PushedToClient - lastRecv
					pushQPS := float64(intervalPushed) / intervalSec

					fmt.Printf("[%.0fs] SendQPS: %.0f | FwdQPS: %.0f | PushQPS: %.0f | Sent: %d | Fwd: %d | Pushed: %d | DropOvl: %d | DropFull: %d | PushDrop: %d | CPU: %.1f%% | Mem: %.1f%% | Ovl: %t\n",
						elapsed, sendQPS, forwardQPS, pushQPS,
						curSent, curForwarded, stats.PushedToClient,
						stats.DroppedOverload, stats.DroppedFull, stats.PushDroppedNoConn,
						stats.CPUPercent, stats.MemPercent, stats.Overloaded)

					lastRecv = stats.PushedToClient

					lastForwarded = curForwarded
				} else {
					fmt.Printf("[%.0fs] SendQPS: %.0f | Sent: %d | stats query failed: %v\n",
						elapsed, sendQPS, curSent, err)
				}
			} else {
				intervalRecv := curRecv - lastRecv
				qps := float64(intervalRecv) / intervalSec
				avgQPS := float64(curRecv) / elapsed

				// 查询 sgate 推送统计
				var pushedStr, pushDropStr string
				if stats, err := queryStats(statsAddr); err == nil {
					pushedStr = fmt.Sprintf(" | PushedToClient: %d", stats.PushedToClient)
					pushDropStr = fmt.Sprintf(" | PushDroppedNoConn: %d", stats.PushDroppedNoConn)
				}

				fmt.Printf("[%.0fs] SendQPS: %.0f | RecvQPS: %.0f | AvgRecvQPS: %.0f | Sent: %d | Recv: %d | Dropped: %d | Pending: %d%s%s\n",
					elapsed, sendQPS, qps, avgQPS, curSent, curRecv, atomic.LoadInt64(&totalDropped), curSent-curRecv-atomic.LoadInt64(&totalDropped), pushedStr, pushDropStr)

				lastRecv = curRecv
			}

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

				if mode == "forward" || mode == "ws-forward" {
					if stats, err := queryStats(statsAddr); err == nil {
						fmt.Printf("\n=== sgate Stats ===\n")
						fmt.Printf("Total Received by sgate: %d\n", stats.Received)
						fmt.Printf("Total Forwarded (client->logic): %d\n", stats.Forwarded)
						fmt.Printf("Dropped (overload): %d\n", stats.DroppedOverload)
						fmt.Printf("Dropped (channel full): %d\n", stats.DroppedFull)
						fmt.Printf("Dropped Total: %d\n", stats.DroppedTotal)
						fmt.Printf("Average Forward QPS: %.0f\n", float64(stats.Forwarded)/totalElapsed)
						fmt.Printf("\n--- Reverse (logic->client) ---\n")
						fmt.Printf("Total PushedToClient: %d\n", stats.PushedToClient)
						fmt.Printf("PushDroppedNoConn: %d\n", stats.PushDroppedNoConn)
						fmt.Printf("Average Push QPS: %.0f\n", float64(stats.PushedToClient)/totalElapsed)
						if stats.DroppedTotal == 0 && stats.PushDroppedNoConn == 0 {
							fmt.Printf("RESULT: BIDIRECTIONAL SUCCESS (0 drops both ways)\n")
						} else {
							fmt.Printf("RESULT: drops fwd=%d push=%d\n", stats.DroppedTotal, stats.PushDroppedNoConn)
						}
					}
				} else {
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
				}
				return
			}
		}
	}
}
