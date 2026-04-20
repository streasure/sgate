package main

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

type LoadTestConfig struct {
	TotalRequests   int
	TCPConnections  int
	ServerTCPAddr  string
}

type LoadTestResult struct {
	TotalRequests      int64
	SuccessfulRequests int64
	FailedRequests    int64
	TotalDuration     time.Duration
	AvgLatency        time.Duration
	MinLatency        time.Duration
	MaxLatency        time.Duration
	P50Latency        time.Duration
	P95Latency        time.Duration
	P99Latency        time.Duration
	QPS               float64
}

func main() {
	config := LoadTestConfig{
		TotalRequests:  100000,
		TCPConnections: 100,
		ServerTCPAddr: "localhost:48080",
	}

	fmt.Printf("=== 高性能压测 ===\n")
	fmt.Printf("总请求数: %d\n", config.TotalRequests)
	fmt.Printf("TCP连接数: %d (复用)\n", config.TCPConnections)
	fmt.Printf("服务器地址: %s\n", config.ServerTCPAddr)
	fmt.Printf("\n")

	result := runLoadTest(config)

	fmt.Printf("\n=== 测试结果 ===\n")
	fmt.Printf("总请求数: %d\n", atomic.LoadInt64(&result.TotalRequests))
	fmt.Printf("成功请求数: %d\n", atomic.LoadInt64(&result.SuccessfulRequests))
	fmt.Printf("失败请求数: %d\n", atomic.LoadInt64(&result.FailedRequests))
	fmt.Printf("成功率: %.2f%%\n", float64(atomic.LoadInt64(&result.SuccessfulRequests))/float64(config.TotalRequests)*100)
	fmt.Printf("总耗时: %v\n", result.TotalDuration)
	fmt.Printf("平均延迟: %v\n", result.AvgLatency)
	fmt.Printf("最小延迟: %v\n", result.MinLatency)
	fmt.Printf("最大延迟: %v\n", result.MaxLatency)
	fmt.Printf("P50延迟: %v\n", result.P50Latency)
	fmt.Printf("P95延迟: %v\n", result.P95Latency)
	fmt.Printf("P99延迟: %v\n", result.P99Latency)
	fmt.Printf("QPS: %.2f\n", result.QPS)
}

func runLoadTest(config LoadTestConfig) *LoadTestResult {
	result := &LoadTestResult{
		MinLatency: time.Hour,
	}

	var wg sync.WaitGroup
	var allLatencies []time.Duration
	var latenciesMu sync.Mutex
	startTime := time.Now()

	requestsPerConn := config.TotalRequests / config.TCPConnections

	for i := 0; i < config.TCPConnections; i++ {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", config.ServerTCPAddr)
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, 1)
				return
			}
			defer conn.Close()

			for j := 0; j < requestsPerConn; j++ {
				atomic.AddInt64(&result.TotalRequests, 1)

				start := time.Now()

				pingMsg := &protobuf.Message{
					Route: "ping",
					Payload: map[string]string{
						"conn_id": fmt.Sprintf("%d", connID),
						"seq":     fmt.Sprintf("%d", j),
					},
				}

				data, err := proto.Marshal(pingMsg)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				_, err = conn.Write(data)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				buffer := make([]byte, 1024)
				_, err = conn.Read(buffer)
				latency := time.Since(start)

				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				atomic.AddInt64(&result.SuccessfulRequests, 1)

				latenciesMu.Lock()
				allLatencies = append(allLatencies, latency)
				if latency < result.MinLatency {
					result.MinLatency = latency
				}
				if latency > result.MaxLatency {
					result.MaxLatency = latency
				}
				latenciesMu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	result.TotalDuration = time.Since(startTime)

	if len(allLatencies) > 0 {
		var totalLatency time.Duration
		for _, l := range allLatencies {
			totalLatency += l
		}
		result.AvgLatency = totalLatency / time.Duration(len(allLatencies))

		sort.Slice(allLatencies, func(i, j int) bool {
			return allLatencies[i] < allLatencies[j]
		})

		idx50 := len(allLatencies) * 50 / 100
		idx95 := len(allLatencies) * 95 / 100
		idx99 := len(allLatencies) * 99 / 100

		if idx50 < len(allLatencies) {
			result.P50Latency = allLatencies[idx50]
		}
		if idx95 < len(allLatencies) {
			result.P95Latency = allLatencies[idx95]
		}
		if idx99 < len(allLatencies) {
			result.P99Latency = allLatencies[idx99]
		}
	}

	if atomic.LoadInt64(&result.SuccessfulRequests) > 0 {
		result.QPS = float64(atomic.LoadInt64(&result.SuccessfulRequests)) / result.TotalDuration.Seconds()
	}

	return result
}
