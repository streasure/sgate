package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

func main() {
	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			if _, err := tlog.New("../../config/tlog.yaml"); err != nil {
				slog.Error("failed to initialize tlog", "error", err)
			}
		}
	}

	serverAddr := "localhost:8083"
	totalConn := 100
	requestsPerConn := 100

	if len(os.Args) >= 2 {
		fmt.Sscanf(os.Args[1], "%d", &totalConn)
		if len(os.Args) >= 3 {
			fmt.Sscanf(os.Args[2], "%d", &requestsPerConn)
		}
		if len(os.Args) >= 4 {
			serverAddr = os.Args[3]
		}
	}

	tlog.Info("=== SGate Load Test ===")
	tlog.Info("test config", "connections", totalConn, "requestsPerConn", requestsPerConn, "server", serverAddr, "cpus", runtime.NumCPU())

	var successfulRequests, failedRequests, connFailures, timeoutCount int64
	var latencies []time.Duration
	var mu sync.Mutex
	minLatency := time.Hour
	var firstErr string
	var firstErrOnce sync.Once

	type connWrap struct {
		conn   net.Conn
		connID int
	}

	var conns []connWrap
	var connWg sync.WaitGroup

	batchSize := 50
	for batchStart := 0; batchStart < totalConn; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > totalConn {
			batchEnd = totalConn
		}

		for i := batchStart; i < batchEnd; i++ {
			connWg.Add(1)
			go func(connID int) {
				defer connWg.Done()
				conn, err := net.DialTimeout("tcp", serverAddr, 10*time.Second)
				if err != nil {
					atomic.AddInt64(&connFailures, 1)
					atomic.AddInt64(&failedRequests, int64(requestsPerConn))
					firstErrOnce.Do(func() { firstErr = fmt.Sprintf("dial error: %v", err) })
					return
				}
				mu.Lock()
				conns = append(conns, connWrap{conn: conn, connID: connID})
				mu.Unlock()
			}(i)
		}
		connWg.Wait()

		if batchEnd < totalConn {
			tlog.Info("batch connected", "count", len(conns), "target", totalConn)
			time.Sleep(500 * time.Millisecond)
		}
	}

	tlog.Info("all connections established", "success", len(conns), "failures", connFailures)

	if len(conns) == 0 {
		tlog.Error("no connections established, aborting")
		writeResults(0, 0, connFailures, 0, firstErr, 0, nil, time.Duration(0))
		tlog.Sync()
		time.Sleep(500 * time.Millisecond)
		return
	}

	start := time.Now()

	var reqWg sync.WaitGroup
	for _, cw := range conns {
		reqWg.Add(1)
		go func(c connWrap) {
			defer reqWg.Done()
			defer c.conn.Close()

			c.conn.SetDeadline(time.Now().Add(120 * time.Second))

			for j := 0; j < requestsPerConn; j++ {
				reqStart := time.Now()

				msg := &protobuf.Message{
					Route: "test",
					Payload: map[string]string{
						"conn_id": fmt.Sprintf("%d", c.connID),
						"seq":     fmt.Sprintf("%d", j),
					},
				}

				data, _ := proto.Marshal(msg)

				buf := make([]byte, 4+len(data))
				binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
				copy(buf[4:], data)

				_, err := c.conn.Write(buf)
				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					firstErrOnce.Do(func() { firstErr = fmt.Sprintf("write error (conn=%d,seq=%d): %v", c.connID, j, err) })
					return
				}

				readBuf := make([]byte, 4096)
				c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := c.conn.Read(readBuf)
				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						atomic.AddInt64(&timeoutCount, 1)
						firstErrOnce.Do(func() { firstErr = fmt.Sprintf("read timeout (conn=%d,seq=%d): %v", c.connID, j, err) })
						continue
					}
					firstErrOnce.Do(func() { firstErr = fmt.Sprintf("read error (conn=%d,seq=%d,n=%d): %v", c.connID, j, n, err) })
					return
				}

				latency := time.Since(reqStart)
				atomic.AddInt64(&successfulRequests, 1)

				mu.Lock()
				latencies = append(latencies, latency)
				if latency < minLatency {
					minLatency = latency
				}
				mu.Unlock()

				c.conn.SetDeadline(time.Now().Add(120 * time.Second))
			}
		}(cw)
	}

	reqWg.Wait()
	duration := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	writeResults(successfulRequests, failedRequests, connFailures, timeoutCount, firstErr, minLatency, latencies, duration)

	tlog.Sync()
	time.Sleep(500 * time.Millisecond)
}

func writeResults(successfulRequests, failedRequests, connFailures, timeoutCount int64, firstErr string, minLatency time.Duration, latencies []time.Duration, duration time.Duration) {
	var avgLatency, p50, p95, p99, maxLatency time.Duration
	if len(latencies) > 0 {
		var totalLatency time.Duration
		for _, l := range latencies {
			totalLatency += l
		}
		avgLatency = totalLatency / time.Duration(len(latencies))
		p50 = latencies[len(latencies)*50/100]
		p95 = latencies[len(latencies)*95/100]
		p99 = latencies[len(latencies)*99/100]
		maxLatency = latencies[len(latencies)-1]
	}

	totalRequests := successfulRequests + failedRequests

	var qps float64
	if duration.Seconds() > 0 {
		qps = float64(successfulRequests) / duration.Seconds()
	}

	var successRate float64
	if totalRequests > 0 {
		successRate = float64(successfulRequests) / float64(totalRequests) * 100
	}

	lines := []string{
		"=== Results ===",
		fmt.Sprintf("Total Requests: %d", totalRequests),
		fmt.Sprintf("Successful: %d", successfulRequests),
		fmt.Sprintf("Failed: %d", failedRequests),
		fmt.Sprintf("Conn Failures: %d", connFailures),
		fmt.Sprintf("Timeouts: %d", timeoutCount),
	}
	if totalRequests > 0 {
		lines = append(lines, fmt.Sprintf("Success Rate: %.2f%%", successRate))
	}
	if firstErr != "" {
		lines = append(lines, fmt.Sprintf("First Error: %s", firstErr))
	}
	lines = append(lines,
		fmt.Sprintf("Duration: %v", duration),
		fmt.Sprintf("Avg Latency: %v", avgLatency),
		fmt.Sprintf("Min Latency: %v", minLatency),
		fmt.Sprintf("Max Latency: %v", maxLatency),
		fmt.Sprintf("P50 Latency: %v", p50),
		fmt.Sprintf("P95 Latency: %v", p95),
		fmt.Sprintf("P99 Latency: %v", p99),
	)
	if duration.Seconds() > 0 {
		lines = append(lines, fmt.Sprintf("QPS: %.2f", qps))
	}

	for _, line := range lines {
		fmt.Println(line)
		tlog.Info(line)
	}
}
