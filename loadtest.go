package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

func main() {
	serverAddr := "localhost:48080"
	totalConn := 500
	requestsPerConn := 200000

	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "fast":
			totalConn = 10000
			requestsPerConn = 1000
		case "normal":
			totalConn = 100000
			requestsPerConn = 100
		case "stress":
			totalConn = 2000000
			requestsPerConn = 100
		default:
			fmt.Sscanf(os.Args[1], "%d", &totalConn)
			if len(os.Args) >= 3 {
				fmt.Sscanf(os.Args[2], "%d", &requestsPerConn)
			}
			if len(os.Args) >= 4 {
				serverAddr = os.Args[3]
			}
		}
	}

	fmt.Printf("=== SGate 压测工具 ===\n")
	fmt.Printf("总连接数: %d\n", totalConn)
	fmt.Printf("每连接请求数: %d\n", requestsPerConn)
	fmt.Printf("服务器地址: %s\n", serverAddr)
	fmt.Printf("Goroutines: %d\n", runtime.NumCPU())
	fmt.Printf("\n")

	var totalRequests, successfulRequests, failedRequests int64
	var latencies []time.Duration
	var mu sync.Mutex
	minLatency := time.Hour

	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < totalConn; i++ {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				atomic.AddInt64(&failedRequests, int64(requestsPerConn))
				return
			}
			defer conn.Close()

			for j := 0; j < requestsPerConn; j++ {
				reqStart := time.Now()

				msg := &protobuf.Message{
					Route: "ping",
					Payload: map[string]string{
						"conn_id": fmt.Sprintf("%d", connID),
						"seq":     fmt.Sprintf("%d", j),
					},
				}

				data, _ := proto.Marshal(msg)
				conn.Write(data)

				buf := make([]byte, 1024)
				conn.Read(buf)

				latency := time.Since(reqStart)
				atomic.AddInt64(&totalRequests, 1)
				atomic.AddInt64(&successfulRequests, 1)

				mu.Lock()
				latencies = append(latencies, latency)
				if latency < minLatency {
					minLatency = latency
				}
				mu.Unlock()
			}
		}(i)

		if i%100 == 0 && i > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	wg.Wait()
	duration := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

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
		for _, l := range latencies {
			if l > maxLatency {
				maxLatency = l
			}
		}
	}

	fmt.Printf("\n=== 测试结果 ===\n")
	fmt.Printf("总请求数: %d\n", totalRequests)
	fmt.Printf("成功请求数: %d\n", successfulRequests)
	fmt.Printf("失败请求数: %d\n", failedRequests)
	if totalRequests > 0 {
		fmt.Printf("成功率: %.2f%%\n", float64(successfulRequests)/float64(totalRequests)*100)
	}
	fmt.Printf("总耗时: %v\n", duration)
	fmt.Printf("平均延迟: %v\n", avgLatency)
	fmt.Printf("最小延迟: %v\n", minLatency)
	fmt.Printf("最大延迟: %v\n", maxLatency)
	fmt.Printf("P50延迟: %v\n", p50)
	fmt.Printf("P95延迟: %v\n", p95)
	fmt.Printf("P99延迟: %v\n", p99)
	fmt.Printf("QPS: %.2f\n", float64(successfulRequests)/duration.Seconds())
}
