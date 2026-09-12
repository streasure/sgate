package main

import (
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

const (
	cmdLoginGate    int32 = 1000001
	cmdLoginGateAck int32 = 1000002
	cmdPushBatch    int32 = 9000002
)

func main() {
	addr := flag.String("addr", "127.0.0.1:48081", "sgate WebSocket address")
	duration := flag.Duration("duration", 30*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 1, "number of parallel connections")
	serverID := flag.String("server-id", "logic2", "logic server ID for login")
	logConfig := flag.String("config", "configs/log.yaml", "log configuration")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	tlog.Info("bench2 started", "addr", *addr, "duration", duration.String(), "parallel", *parallel, "serverID", *serverID)

	var totalRecv atomic.Int64
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64
	var measureStart atomic.Int64

	ready := make(chan bool, *parallel)
	start := make(chan struct{})
	abort := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(*parallel)
	for i := 0; i < *parallel; i++ {
		go func(idx int) {
			defer wg.Done()
			if err := runWSClient(addr, *duration, idx, *serverID, ready, start, abort, &measureStart, &totalRecv, &totalAck); err != nil {
				tlog.Warn("bench2 client failed", "client", idx, "error", err)
				connectionsFailed.Add(1)
			}
		}(i)
	}

	allReady := true
	for i := 0; i < *parallel; i++ {
		if !<-ready {
			allReady = false
		}
	}
	if !allReady {
		close(abort)
	} else {
		close(start)
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			startedAt := measureStart.Load()
			if startedAt == 0 {
				tlog.Info("bench2 progress", "state", "waiting for first push", "ack", totalAck.Load())
				continue
			}
			elapsed := time.Since(time.Unix(0, startedAt)).Seconds()
			recv := totalRecv.Load()
			ack := totalAck.Load()
			rate := float64(recv) / elapsed
			tlog.Info("bench2 progress", "elapsed", elapsed, "received", recv, "ack", ack, "rate", rate)
		}
	}()

	wg.Wait()
	total := totalRecv.Load()
	startedAt := measureStart.Load()
	elapsed := 0.0
	if startedAt != 0 {
		elapsed = time.Since(time.Unix(0, startedAt)).Seconds()
	}
	failed := connectionsFailed.Load()

	tlog.Info("bench2 completed", "totalReceived", total, "totalAck", totalAck.Load(), "connectionsFailed", failed, "elapsed", elapsed)
	if elapsed > 0 {
		tlog.Info("bench2 result", "avgReceiveRate", float64(total)/elapsed)
	}
}

func runWSClient(addr *string, duration time.Duration, idx int, serverID string, ready chan<- bool, start, abort <-chan struct{}, measureStart, totalRecv, totalAck *atomic.Int64) error {
	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		ready <- false
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if err := wsUpgrade(conn, *addr); err != nil {
		ready <- false
		return fmt.Errorf("ws upgrade: %w", err)
	}

	userID := fmt.Sprintf("bench_user_%d", idx)

	loginReq := &protocol.LoginGateReq{
		ServerId: serverID,
		UserId:   userID,
		LoginKey: "",
	}
	loginBody, _ := proto.Marshal(loginReq)

	if err := sendWSBinary(conn, &protocol.MessageFrame{
		Cmd:   cmdLoginGate,
		SeqId: 1,
		Body:  loginBody,
	}); err != nil {
		ready <- false
		return fmt.Errorf("send login: %w", err)
	}

	resp, err := readWSBinary(conn)
	if err != nil {
		ready <- false
		return fmt.Errorf("read login ack: %w", err)
	}
	if resp.Cmd != cmdLoginGateAck {
		ready <- false
		return fmt.Errorf("unexpected cmd %d, expected LoginGateAck", resp.Cmd)
	}
	totalAck.Add(1)
	ready <- true
	select {
	case <-start:
	case <-abort:
		return nil
	}

	deadline := time.Now().Add(duration + 30*time.Second)
	conn.SetReadDeadline(deadline)

	for {
		frame, err := readWSBinary(conn)
		if err != nil {
			break
		}
		now := time.Now()
		if measureStart.Load() == 0 {
			measureStart.CompareAndSwap(0, now.UnixNano())
		}
		if now.Sub(time.Unix(0, measureStart.Load())) >= duration {
			return nil
		}
		if frame.Cmd == cmdPushBatch {
			var batch protocol.PushBatch
			if proto.Unmarshal(frame.Body, &batch) == nil {
				totalRecv.Add(int64(len(batch.Items)))
			}
		} else {
			totalRecv.Add(1)
		}
	}

	return nil
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
