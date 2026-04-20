package main

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

func main() {
	// 测试参数
	const (
		nRequests = 20000  // 总请求数
		concurrency = 1000 // 并发数
		serverAddr = "localhost:48080"
	)

	// 统计变量
	var (
		totalRequests   int64
		successfulRequests int64
		failedRequests  int64
		totalTime       time.Duration
		mu              sync.Mutex
	)

	// 开始时间
	startTime := time.Now()

	// 并发测试
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// 每个goroutine复用连接
			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				atomic.AddInt64(&failedRequests, 1)
				return
			}
			defer conn.Close()

			for {
				currentReq := atomic.AddInt64(&totalRequests, 1)
				if currentReq > nRequests {
					atomic.AddInt64(&totalRequests, -1)
					break
				}

				// 发送请求
				start := time.Now()

				// 创建 ping 请求消息
				pingMsg := &protobuf.Message{
					Route:   "ping",
					Payload: make(map[string]string),
				}

				// 序列化消息
				data, err := proto.Marshal(pingMsg)
				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					continue
				}

				// 发送数据
				_, err = conn.Write(data)
				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					continue
				}

				// 接收响应
				buffer := make([]byte, 1024)
				_, err = conn.Read(buffer)
				requestTime := time.Since(start)

				if err == nil {
					atomic.AddInt64(&successfulRequests, 1)
					mu.Lock()
					totalTime += requestTime
					mu.Unlock()
				} else {
					atomic.AddInt64(&failedRequests, 1)
				}
			}
		}()
	}

	// 等待所有测试完成
	wg.Wait()

	// 计算结果
	totalDuration := time.Since(startTime)
	avgRequestTime := time.Duration(0)
	if successfulRequests > 0 {
		avgRequestTime = totalTime / time.Duration(successfulRequests)
	}
	qps := float64(successfulRequests) / totalDuration.Seconds()

	// 输出结果
	fmt.Printf("=== 极限性能测试结果 ===\n")
	fmt.Printf("总请求数: %d\n", nRequests)
	fmt.Printf("成功请求数: %d\n", successfulRequests)
	fmt.Printf("失败请求数: %d\n", failedRequests)
	fmt.Printf("成功率: %.2f%%\n", float64(successfulRequests)/float64(nRequests)*100)
	fmt.Printf("总耗时: %v\n", totalDuration)
	fmt.Printf("平均请求时间: %v\n", avgRequestTime)
	fmt.Printf("QPS: %.2f\n", qps)
}
