package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
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
	addr := flag.String("addr", "127.0.0.1:48080", "sgate TCP address")
	duration := flag.Duration("duration", 30*time.Second, "benchmark duration")
	parallel := flag.Int("parallel", 1, "number of parallel connections")
	serverID := flag.String("server-id", "logic2", "logic server ID for login")
	logConfig := flag.String("config", "configs/log.yaml", "log config")
	loginBatch := flag.Int("login-batch", 100, "concurrent login batch size")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	tlog.Info(context.Background(), "bench2 started addr=%s duration=%s parallel=%d serverID=%s loginBatch=%d", *addr, duration.String(), *parallel, *serverID, *loginBatch)

	var totalRecv atomic.Int64
	var totalAck atomic.Int64
	var connectionsFailed atomic.Int64
	var measureStart atomic.Int64

	// 阶段1：分批登录，每批 loginBatch 个连接
	batchSize := *loginBatch
	if batchSize > *parallel {
		batchSize = *parallel
	}

	// 存活连接：登录成功后存入，start 信号后开始接收
	type connEntry struct {
		conn   net.Conn
		idx    int
	}
	var conns []*connEntry
	var connMu sync.Mutex

	tlog.Info(context.Background(), "bench2 login phase started parallel=%d batchSize=%d", *parallel, batchSize)

	for batch := 0; batch < *parallel; batch += batchSize {
		end := batch + batchSize
		if end > *parallel {
			end = *parallel
		}

		var batchWG sync.WaitGroup
		var batchReady sync.WaitGroup
		batchReady.Add(end - batch)

		for i := batch; i < end; i++ {
			idx := i
			batchWG.Add(1)
			go func() {
				defer batchWG.Done()
				c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err != nil {
					tlog.Warn(context.Background(), "bench2 dial failed client=%d error=%v", idx, err)
					connectionsFailed.Add(1)
					batchReady.Done()
					return
				}

				userID := fmt.Sprintf("bench_user_%d", idx)
				loginReq := &protocol.LoginGateReq{ServerId: *serverID, UserId: userID}
				loginBody, _ := proto.Marshal(loginReq)
				if err := sendTCPFrame(c, &protocol.MessageFrame{Cmd: cmdLoginGate, SeqId: 1, Body: loginBody}); err != nil {
					tlog.Warn(context.Background(), "bench2 send login failed client=%d error=%v", idx, err)
					c.Close()
					connectionsFailed.Add(1)
					batchReady.Done()
					return
				}

				resp, err := readTCPFrame(c)
				if err != nil || resp.Cmd != cmdLoginGateAck {
					tlog.Warn(context.Background(), "bench2 login ack failed client=%d error=%v", idx, err)
					c.Close()
					connectionsFailed.Add(1)
					batchReady.Done()
					return
				}

				totalAck.Add(1)
				connMu.Lock()
				conns = append(conns, &connEntry{conn: c, idx: idx})
				connMu.Unlock()
				batchReady.Done()
			}()
		}
		batchReady.Wait()
		// 本批完成，继续下一批
		tlog.Info(context.Background(), "bench2 login batch done batch=%d~%d ack=%d failed=%d totalConns=%d", batch, end-1, totalAck.Load(), connectionsFailed.Load(), len(conns))
	}

	connected := len(conns)
	tlog.Info(context.Background(), "bench2 all logins done connected=%d failed=%d", connected, connectionsFailed.Load())
	if connected == 0 {
		tlog.Info(context.Background(), "bench2 completed totalReceived=0 totalAck=%d connectionsFailed=%d elapsed=0.0", totalAck.Load(), connectionsFailed.Load())
		return
	}

	// 设置读取超时
	deadline := time.Now().Add(*duration + 30*time.Second)
	for _, e := range conns {
		e.conn.SetReadDeadline(deadline)
	}

	// 阶段2：开始接收推送
	tlog.Info(context.Background(), "bench2 receive phase started connections=%d duration=%s", connected, duration.String())
	measureStart.Store(time.Now().UnixNano())

	var recvWG sync.WaitGroup
	recvWG.Add(connected)
	for _, e := range conns {
		go func(entry *connEntry) {
			defer recvWG.Done()
			header := make([]byte, 4)
			skip := make([]byte, 64*1024) // 64KB 复用 buffer
			for {
				if _, err := readFull(entry.conn, header); err != nil {
					return
				}
				dataLen := binary.BigEndian.Uint32(header)
				if dataLen == 0 || dataLen > 4*1024*1024 {
					return
				}
				remaining := int(dataLen)
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

	// 进度打印
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
				tlog.Info(context.Background(), "bench2 progress elapsed=%.1f received=%d ack=%d rate=%.0f", elapsed, recv, totalAck.Load(), rate)
			case <-done:
				return
			}
		}
	}()

	// 等待 duration
	time.Sleep(*duration)
	close(done)

	// 关闭所有连接触发 recv goroutine 退出
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

	tlog.Info(context.Background(), "bench2 completed totalReceived=%d totalAck=%d connectionsFailed=%d elapsed=%.1f", total, totalAck.Load(), connectionsFailed.Load(), elapsed)
	if elapsed > 0 {
		tlog.Info(context.Background(), "bench2 result avgReceiveRate=%.0f", float64(total)/elapsed)
	}
}

func sendTCPFrame(conn net.Conn, frame *protocol.MessageFrame) error {
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	_, err = conn.Write(append(header, data...))
	return err
}

func readTCPFrame(conn net.Conn) (*protocol.MessageFrame, error) {
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
