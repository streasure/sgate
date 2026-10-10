package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

const (
	cmdLoginGate    int32 = 1000001
	cmdLoginGateAck int32 = 1000002
	cmdHeartbeatReq int32 = 1100010
)

// main TCP基准测试入口，执行登录阶段和数据发送阶段
func main() {
	addr := flag.String("addr", "127.0.0.1:48080", "sgate TCP address")
	duration := flag.Duration("duration", 10*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 100, "number of parallel connections")
	serverID := flag.String("server-id", "logic1-tcp", "logic server instance ID to bind")
	sendWorkers := flag.Int("send-workers-per-conn", 1, "send goroutines per connection (1 = single sender per conn; >1 stresses sgate forwarding, writes serialized by a per-conn mutex)")
	flag.Parse()

	fmt.Printf("bench1_tcp: addr=%s duration=%s parallel=%d serverID=%s sendWorkersPerConn=%d\n", *addr, *duration, *parallel, *serverID, *sendWorkers)

	var totalSent atomic.Int64 // 客户端实际写入成功的消息数（发出侧口径，非网关转发数）
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64

	// 阶段1：按顺序登录所有连接，避免事件循环过载
	type connResult struct {
		conn net.Conn
		err  error
	}
	results := make([]connResult, *parallel)

	fmt.Println("阶段1：登录中...")
	for i := range *parallel {
		conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client %d dial error: %v\n", i, err)
			connectionsFailed.Add(1)
			results[i] = connResult{nil, err}
			continue
		}

		loginReq := &protocol.LoginGateReq{
			ServerId: *serverID,
			UserId:   fmt.Sprintf("bench_user_%d", i),
			LoginKey: "",
		}
		loginBody, _ := proto.Marshal(loginReq)

		if err := sendTCPFrame(conn, &protocol.MessageFrame{
			Cmd:   cmdLoginGate,
			SeqId: 1,
			Body:  loginBody,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "client %d send login error: %v\n", i, err)
			conn.Close()
			connectionsFailed.Add(1)
			results[i] = connResult{nil, err}
			continue
		}

		resp, err := readTCPFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client %d read ack error: %v\n", i, err)
			conn.Close()
			connectionsFailed.Add(1)
			results[i] = connResult{nil, err}
			continue
		}
		if resp.Cmd != cmdLoginGateAck {
			fmt.Fprintf(os.Stderr, "client %d unexpected cmd %d\n", i, resp.Cmd)
			conn.Close()
			connectionsFailed.Add(1)
			results[i] = connResult{nil, fmt.Errorf("unexpected cmd")}
			continue
		}
		ack := new(protocol.LoginGateAck)
		if err := proto.Unmarshal(resp.Body, ack); err != nil || ack.Code != 0 {
			code := ack.Code
			fmt.Fprintf(os.Stderr, "client %d login rejected code=%d message=%s\n", i, code, ack.GetMessage())
			conn.Close()
			connectionsFailed.Add(1)
			results[i] = connResult{nil, fmt.Errorf("login rejected code=%d", code)}
			continue
		}
		totalAck.Add(1)
		results[i] = connResult{conn, nil}
	}

	loggedIn := totalAck.Load()
	failed := connectionsFailed.Load()
	fmt.Printf("阶段1完成：%d个登录成功，%d个失败\n", loggedIn, failed)

	if loggedIn == 0 {
		fmt.Println("无连接，退出")
		return
	}

	// 收集活跃连接
	conns := make([]net.Conn, 0, loggedIn)
	for _, r := range results {
		if r.conn != nil {
			conns = append(conns, r.conn)
		}
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// 启动后台读取协程（丢弃所有响应）
	var readerWg sync.WaitGroup
	stopReaders := make(chan struct{})
	for _, c := range conns {
		readerWg.Add(1)
		go func(conn net.Conn) {
			defer readerWg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_, err := readTCPFrame(conn)
					if err != nil {
						return
					}
				}
			}
		}(c)
	}

	// 阶段2：同时向所有连接发送大量数据。
	// 每个连接可用多个发送 goroutine 来打满 sgate 的转发上限；同一连接的写必须串行，
	// 否则多个 goroutine 交错写会把 TCP 流打成乱序字节、导致网关解帧失败。
	workers := *sendWorkers
	if workers < 1 {
		workers = 1
	}
	fmt.Printf("阶段2：向%d个连接发送数据（每连接%d个发送协程），持续时间：%s...\n", len(conns), workers, *duration)

	var floodWg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(*duration)
	for _, c := range conns {
		var writeMu sync.Mutex
		for w := 0; w < workers; w++ {
			floodWg.Add(1)
			go func(conn net.Conn) {
				defer floodWg.Done()
				heartbeatBody := []byte("bench")
				var seqID int64 = 2
				for time.Now().Before(deadline) {
					msg := &protocol.MessageFrame{
						Cmd:   cmdHeartbeatReq,
						SeqId: seqID,
						Body:  heartbeatBody,
					}
					writeMu.Lock()
					err := sendTCPFrame(conn, msg)
					writeMu.Unlock()
					if err != nil {
						return
					}
					totalSent.Add(1)
					seqID++
				}
			}(c)
		}
	}

	// 统计定时器
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			elapsed := time.Since(start).Seconds()
			sent := totalSent.Load()
			rate := float64(sent) / elapsed
			fmt.Printf("[%.0fs] sent=%d rate=%.0f msg/s\n", elapsed, sent, rate)
		}
	}()

	floodWg.Wait()
	close(stopReaders)
	readerWg.Wait()

	total := totalSent.Load()
	elapsed := time.Since(start).Seconds()

	fmt.Println("---")
	fmt.Printf("connections logged in: %d\n", loggedIn)
	fmt.Printf("connections failed: %d\n", failed)
	fmt.Printf("send workers per conn: %d\n", workers)
	fmt.Printf("total sent (client-side): %d\n", total)
	fmt.Printf("elapsed: %.2fs\n", elapsed)
	if elapsed > 0 {
		fmt.Printf("avg rate: %.0f msg/s\n", float64(total)/elapsed)
	}
}

// sendTCPFrame 向TCP连接发送一个消息帧
func sendTCPFrame(conn net.Conn, frame *protocol.MessageFrame) error {
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	buf := append(header, data...)
	_, err = conn.Write(buf)
	return err
}

// readTCPFrame 从TCP连接读取一个消息帧
func readTCPFrame(conn net.Conn) (*protocol.MessageFrame, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	header := make([]byte, 4)
	if _, err := readFull(conn, header); err != nil {
		return nil, err
	}
	dataLen := binary.BigEndian.Uint32(header)
	if dataLen == 0 || dataLen > 4*1024*1024 {
		return nil, fmt.Errorf("invalid frame length: %d", dataLen)
	}
	data := make([]byte, dataLen)
	if _, err := readFull(conn, data); err != nil {
		return nil, err
	}
	frame := &protocol.MessageFrame{}
	if err := proto.Unmarshal(data, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// readFull 从连接中读取指定字节数的数据
func readFull(conn net.Conn, buf []byte) (int, error) {
	return io.ReadFull(conn, buf)
}
