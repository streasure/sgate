package main

import (
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
			if err := runClient(addr, *duration, idx, *serverID, ready, start, abort, &measureStart, &totalRecv, &totalAck); err != nil {
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

func runClient(addr *string, duration time.Duration, idx int, serverID string, ready chan<- bool, start, abort <-chan struct{}, measureStart, totalRecv, totalAck *atomic.Int64) error {
	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		ready <- false
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	userID := fmt.Sprintf("bench_user_%d", idx)

	loginReq := &protocol.LoginGateReq{
		ServerId: serverID,
		UserId:   userID,
		LoginKey: "",
	}
	loginBody, _ := proto.Marshal(loginReq)

	if err := sendTCPFrame(conn, &protocol.MessageFrame{
		Cmd:   cmdLoginGate,
		SeqId: 1,
		Body:  loginBody,
	}); err != nil {
		ready <- false
		return fmt.Errorf("send login: %w", err)
	}

	resp, err := readTCPFrame(conn)
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
		frame, err := readTCPFrame(conn)
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
