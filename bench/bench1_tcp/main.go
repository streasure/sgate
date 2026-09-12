package main

import (
	"encoding/binary"
	"flag"
	"fmt"
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

func main() {
	addr := flag.String("addr", "127.0.0.1:48080", "sgate TCP address")
	duration := flag.Duration("duration", 10*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 100, "number of parallel connections")
	flag.Parse()

	fmt.Printf("bench1_tcp: addr=%s duration=%s parallel=%d\n", *addr, *duration, *parallel)

	var totalForwarded atomic.Int64
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64

	// Phase 1: login all connections sequentially to avoid event-loop saturation
	type connResult struct {
		conn net.Conn
		err  error
	}
	results := make([]connResult, *parallel)

	fmt.Println("Phase 1: logging in...")
	for i := 0; i < *parallel; i++ {
		conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client %d dial error: %v\n", i, err)
			connectionsFailed.Add(1)
			results[i] = connResult{nil, err}
			continue
		}

		loginReq := &protocol.LoginGateReq{
			ServerId: "logic1",
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
		totalAck.Add(1)
		results[i] = connResult{conn, nil}
	}

	loggedIn := totalAck.Load()
	failed := connectionsFailed.Load()
	fmt.Printf("Phase 1 done: %d logged in, %d failed\n", loggedIn, failed)

	if loggedIn == 0 {
		fmt.Println("no connections, exiting")
		return
	}

	// Collect live connections
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

	// Start background readers (discard all responses)
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

	// Phase 2: flood all connections simultaneously
	fmt.Printf("Phase 2: flooding %d connections for %s...\n", len(conns), duration)

	var floodWg sync.WaitGroup
	for _, c := range conns {
		floodWg.Add(1)
		go func(conn net.Conn) {
			defer floodWg.Done()
			deadline := time.Now().Add(*duration)
			heartbeatBody := []byte("bench")
			var seqID int64 = 2
			for time.Now().Before(deadline) {
				msg := &protocol.MessageFrame{
					Cmd:   cmdHeartbeatReq,
					SeqId: seqID,
					Body:  heartbeatBody,
				}
				if err := sendTCPFrame(conn, msg); err != nil {
					return
				}
				totalForwarded.Add(1)
				seqID++
			}
		}(c)
	}

	// Stats ticker
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	start := time.Now()

	go func() {
		for range ticker.C {
			elapsed := time.Since(start).Seconds()
			fwd := totalForwarded.Load()
			rate := float64(fwd) / elapsed
			fmt.Printf("[%.0fs] forwarded=%d rate=%.0f msg/s\n", elapsed, fwd, rate)
		}
	}()

	floodWg.Wait()
	close(stopReaders)
	readerWg.Wait()

	total := totalForwarded.Load()
	elapsed := time.Since(start).Seconds()

	fmt.Println("---")
	fmt.Printf("connections logged in: %d\n", loggedIn)
	fmt.Printf("connections failed: %d\n", failed)
	fmt.Printf("total forwarded: %d\n", total)
	fmt.Printf("elapsed: %.2fs\n", elapsed)
	if elapsed > 0 {
		fmt.Printf("avg rate: %.0f msg/s\n", float64(total)/elapsed)
	}
}

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

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
