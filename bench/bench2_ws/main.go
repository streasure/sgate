package main

import (
	"context"
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
	serverID := flag.String("server-id", "logic2-ws", "logic server ID for login")
	logConfig := flag.String("config", "configs/log.yaml", "log config")
	loginBatch := flag.Int("login-batch", 100, "concurrent login batch size")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	tlog.Info(context.TODO(), "bench2 started addr=%s duration=%s parallel=%d serverID=%s loginBatch=%d", *addr, duration.String(), *parallel, *serverID, *loginBatch)

	var totalRecv atomic.Int64
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64
	var measureStart atomic.Int64

	batchSize := *loginBatch
	if batchSize > *parallel {
		batchSize = *parallel
	}

	type connEntry struct {
		conn net.Conn
		idx  int
	}
	var conns []*connEntry
	var connMu sync.Mutex

	tlog.Info(context.TODO(), "bench2 login phase started parallel=%d batchSize=%d", *parallel, batchSize)

	for batch := 0; batch < *parallel; batch += batchSize {
		end := batch + batchSize
		if end > *parallel {
			end = *parallel
		}

		var batchReady sync.WaitGroup
		batchReady.Add(end - batch)

		for i := batch; i < end; i++ {
			idx := i
			go func() {
				defer batchReady.Done()
				c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err != nil {
					tlog.Warn(context.TODO(), "bench2 dial failed client=%d error=%v", idx, err)
					connectionsFailed.Add(1)
					return
				}

				if err := wsUpgrade(c, *addr); err != nil {
					tlog.Warn(context.TODO(), "bench2 ws upgrade failed client=%d error=%v", idx, err)
					c.Close()
					connectionsFailed.Add(1)
					return
				}

				userID := fmt.Sprintf("bench_user_%d", idx)
				loginReq := &protocol.LoginGateReq{ServerId: *serverID, UserId: userID}
				loginBody, _ := proto.Marshal(loginReq)
				if err := sendWSBinary(c, &protocol.MessageFrame{Cmd: cmdLoginGate, SeqId: 1, Body: loginBody}); err != nil {
					tlog.Warn(context.TODO(), "bench2 send login failed client=%d error=%v", idx, err)
					c.Close()
					connectionsFailed.Add(1)
					return
				}

				resp, err := readWSBinary(c)
				if err != nil || resp.Cmd != cmdLoginGateAck {
					tlog.Warn(context.TODO(), "bench2 login ack failed client=%d error=%v", idx, err)
					c.Close()
					connectionsFailed.Add(1)
					return
				}
				ack := new(protocol.LoginGateAck)
				if err := proto.Unmarshal(resp.Body, ack); err != nil || ack.Code != 0 {
					code := int32(-1)
					if ack != nil {
						code = ack.Code
					}
					tlog.Warn(context.TODO(), "bench2 login rejected client=%d code=%d message=%s", idx, code, ack.GetMessage())
					c.Close()
					connectionsFailed.Add(1)
					return
				}

				totalAck.Add(1)
				connMu.Lock()
				conns = append(conns, &connEntry{conn: c, idx: idx})
				connMu.Unlock()
			}()
		}
		batchReady.Wait()
		tlog.Info(context.TODO(), "bench2 login batch done batch=%d~%d ack=%d failed=%d totalConns=%d", batch, end-1, totalAck.Load(), connectionsFailed.Load(), len(conns))
	}

	connected := len(conns)
	tlog.Info(context.TODO(), "bench2 all logins done connected=%d failed=%d", connected, connectionsFailed.Load())
	if connected == 0 {
		tlog.Info(context.TODO(), "bench2 completed totalReceived=0 totalAck=%d connectionsFailed=%d elapsed=0.0", totalAck.Load(), connectionsFailed.Load())
		return
	}

	deadline := time.Now().Add(*duration + 30*time.Second)
	for _, e := range conns {
		e.conn.SetReadDeadline(deadline)
	}

	tlog.Info(context.TODO(), "bench2 receive phase started connections=%d duration=%s", connected, duration.String())
	measureStart.Store(time.Now().UnixNano())

	var recvWG sync.WaitGroup
	recvWG.Add(connected)
	for _, e := range conns {
		go func(entry *connEntry) {
			defer recvWG.Done()
			header := make([]byte, 2)
			ext := make([]byte, 8)
			skip := make([]byte, 64*1024)
			for {
				if _, err := readFull(entry.conn, header); err != nil {
					return
				}
				length := uint64(header[1] & 0x7F)
				switch length {
				case 126:
					if _, err := readFull(entry.conn, ext[:2]); err != nil {
						return
					}
					length = uint64(binary.BigEndian.Uint16(ext[:2]))
				case 127:
					if _, err := readFull(entry.conn, ext); err != nil {
						return
					}
					length = binary.BigEndian.Uint64(ext)
				}
				remaining := int(length)
				for remaining > 0 {
					n := remaining
					if n > len(skip) {
						n = len(skip)
					}
					if _, err := readFull(entry.conn, skip[:n]); err != nil {
						return
					}
					remaining -= n
				}
				totalRecv.Add(1)
			}
		}(e)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				startedAt := measureStart.Load()
				elapsed := time.Since(time.Unix(0, startedAt)).Seconds()
				recv := totalRecv.Load()
				rate := float64(recv) / elapsed
				tlog.Info(context.TODO(), "bench2 progress elapsed=%.1f received=%d ack=%d rate=%.0f", elapsed, recv, totalAck.Load(), rate)
			case <-done:
				return
			}
		}
	}()

	time.Sleep(*duration)
	close(done)

	for _, e := range conns {
		e.conn.Close()
	}
	recvWG.Wait()

	total := totalRecv.Load()
	startedAt := measureStart.Load()
	elapsed := 0.0
	if startedAt != 0 {
		elapsed = time.Since(time.Unix(0, startedAt)).Seconds()
	}

	tlog.Info(context.TODO(), "bench2 completed totalReceived=%d totalAck=%d connectionsFailed=%d elapsed=%.1f", total, totalAck.Load(), connectionsFailed.Load(), elapsed)
	if elapsed > 0 {
		tlog.Info(context.TODO(), "bench2 result avgReceiveRate=%.0f", float64(total)/elapsed)
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
	if !strings.HasPrefix(string(respBuf), "HTTP/1.1 101") {
		return fmt.Errorf("upgrade failed: %s", strings.Split(string(respBuf), "\r\n")[0])
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
