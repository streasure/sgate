package main

import (
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
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
	addr := flag.String("addr", "127.0.0.1:48081", "sgate WebSocket address")
	duration := flag.Duration("duration", 10*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 100, "number of parallel connections")
	flag.Parse()

	fmt.Printf("bench1_ws: addr=%s duration=%s parallel=%d\n", *addr, *duration, *parallel)

	var totalForwarded atomic.Int64
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64

	type connResult struct {
		conn net.Conn
		err  error
	}
	results := make([]connResult, *parallel)

	fmt.Println("Phase 1: logging in...")
	loginDeadline := time.Now().Add(30 * time.Second)
	for i := 0; i < *parallel; i++ {
		if time.Now().After(loginDeadline) {
			fmt.Fprintf(os.Stderr, "login phase timeout after %d connections\n", i)
			connectionsFailed.Add(int64(*parallel - i))
			break
		}

		conn, err := net.DialTimeout("tcp", *addr, 3*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client %d dial error: %v\n", i, err)
			connectionsFailed.Add(1)
			results[i] = connResult{nil, err}
			continue
		}

		conn.SetDeadline(time.Now().Add(5 * time.Second))

		if err := wsUpgrade(conn, *addr); err != nil {
			fmt.Fprintf(os.Stderr, "client %d ws upgrade error: %v\n", i, err)
			conn.Close()
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

		if err := sendWSBinary(conn, &protocol.MessageFrame{
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

		resp, err := readWSBinary(conn)
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

		conn.SetDeadline(time.Time{})
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
					_, err := readWSBinary(conn)
					if err != nil {
						return
					}
				}
			}
		}(c)
	}

	fmt.Printf("Phase 2: flooding %d connections for %s...\n", len(conns), *duration)

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
				if err := sendWSBinary(conn, msg); err != nil {
					return
				}
				totalForwarded.Add(1)
				seqID++
			}
		}(c)
	}

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

func wsUpgrade(conn net.Conn, host string) error {
	key := base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(rand.Int63(), 16)))

	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	respBuf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for {
		n, err := conn.Read(tmp)
		if err != nil {
			return err
		}
		respBuf = append(respBuf, tmp[:n]...)
		if len(respBuf) >= 4 && string(respBuf[len(respBuf)-4:]) == "\r\n\r\n" {
			break
		}
		if len(respBuf) > 16*1024 {
			return fmt.Errorf("websocket handshake response too large")
		}
	}
	respStr := string(respBuf)
	if !strings.HasPrefix(respStr, "HTTP/1.1 101") {
		return fmt.Errorf("upgrade failed: %s", strings.Split(respStr, "\r\n")[0])
	}
	return nil
}

func sendWSBinary(conn net.Conn, frame *protocol.MessageFrame) error {
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	mask := [4]byte{byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))}

	var wsFrame []byte
	n := len(data)
	if n < 126 {
		wsFrame = make([]byte, 0, 2+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|byte(n))
	} else if n <= 65535 {
		wsFrame = make([]byte, 0, 4+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|126, byte(n>>8), byte(n))
	} else {
		wsFrame = make([]byte, 0, 10+4+n)
		wsFrame = append(wsFrame, 0x82, 0x80|127)
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(n))
		wsFrame = append(wsFrame, lenBuf[:]...)
	}
	wsFrame = append(wsFrame, mask[:]...)
	masked := make([]byte, n)
	copy(masked, data)
	for i := range masked {
		masked[i] ^= mask[i%4]
	}
	wsFrame = append(wsFrame, masked...)

	_, err = conn.Write(wsFrame)
	return err
}

func readWSBinary(conn net.Conn) (*protocol.MessageFrame, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	header := make([]byte, 2)
	if _, err := readFull(conn, header); err != nil {
		return nil, err
	}

	length := uint64(header[1] & 0x7F)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := readFull(conn, ext); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := readFull(conn, ext); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	payload := make([]byte, length)
	if _, err := readFull(conn, payload); err != nil {
		return nil, err
	}

	frame := &protocol.MessageFrame{}
	if err := proto.Unmarshal(payload, frame); err != nil {
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
