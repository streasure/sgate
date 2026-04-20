package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

type StressConfig struct {
	TCPConcurrency  int
	UDPConcurrency  int
	WSConcurrency   int
	RequestsPerConn int
	ServerAddr      string
	Timeout         time.Duration
	MessageSize     int
}

type StressResult struct {
	Protocol           string
	TotalRequests      int64
	SuccessfulRequests int64
	FailedRequests     int64
	TotalTime         time.Duration
	QPS               float64
	AvgLatency        time.Duration
	P99Latency        time.Duration
	P95Latency        time.Duration
	MinLatency        time.Duration
	MaxLatency        time.Duration
	BytesSent         int64
	BytesReceived     int64
	SuccessRate       float64
}

func main() {
	// 输出系统信息
	fmt.Printf("========================================\n")
	fmt.Printf("  SGate 全协议全功能全链路压测工具\n")
	fmt.Printf("========================================\n")
	fmt.Printf("系统信息:\n")
	fmt.Printf("  CPU 核心数: %d\n", runtime.NumCPU())
	fmt.Printf("  GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	config := &StressConfig{
		TCPConcurrency:  100,              // TCP 并发连接数
		UDPConcurrency:  100,              // UDP 并发连接数
		WSConcurrency:   100,              // WebSocket 并发连接数
		RequestsPerConn: 100,              // 每个连接的请求数
		ServerAddr:     "localhost",        // 服务器地址
		Timeout:        10 * time.Second,   // 请求超时时间
		MessageSize:    100,                // 消息大小
	}

	fmt.Printf("压测配置:\n")
	fmt.Printf("  TCP 并发连接数: %d\n", config.TCPConcurrency)
	fmt.Printf("  UDP 并发连接数: %d\n", config.UDPConcurrency)
	fmt.Printf("  WebSocket 并发连接数: %d\n", config.WSConcurrency)
	fmt.Printf("  每个连接请求数: %d\n", config.RequestsPerConn)
	fmt.Printf("  服务器地址: %s\n", config.ServerAddr)
	fmt.Printf("  请求超时时间: %v\n", config.Timeout)
	fmt.Printf("  消息大小: %d 字节\n", config.MessageSize)
	fmt.Printf("========================================\n")
	fmt.Printf("\n")

	var allResults []*StressResult
	var wg sync.WaitGroup

	// TCP 压测
	if config.TCPConcurrency > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println("【开始 TCP 协议压测】")
			result := runTCPStressTest(config)
			allResults = append(allResults, result)
			printResult(result)
		}()
	}

	// UDP 压测
	if config.UDPConcurrency > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println("【开始 UDP 协议压测】")
			result := runUDPStressTest(config)
			allResults = append(allResults, result)
			printResult(result)
		}()
	}

	// WebSocket 压测
	if config.WSConcurrency > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println("【开始 WebSocket 协议压测】")
			result := runWSStressTest(config)
			allResults = append(allResults, result)
			printResult(result)
		}()
	}

	wg.Wait()
	printSummary(allResults)
}

func runTCPStressTest(config *StressConfig) *StressResult {
	result := &StressResult{Protocol: "TCP"}
	startTime := time.Now()

	var wg sync.WaitGroup
	wg.Add(config.TCPConcurrency)

	latencies := make([]time.Duration, 0, config.TCPConcurrency*config.RequestsPerConn)
	var latencyMutex sync.Mutex

	for i := 0; i < config.TCPConcurrency; i++ {
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", fmt.Sprintf("%s:48080", config.ServerAddr))
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, int64(config.RequestsPerConn))
				return
			}
			defer conn.Close()

			for j := 0; j < config.RequestsPerConn; j++ {
				reqStart := time.Now()

				// 创建 ping 消息
				msg := &protobuf.Message{
					ConnectionId: fmt.Sprintf("tcp-%d-%d", connID, j),
					Route: "ping",
					Payload: map[string]string{
						"message":   fmt.Sprintf("ping-%d-%d-%d", connID, j, time.Now().UnixNano()),
						"timestamp": fmt.Sprintf("%d", time.Now().UnixNano()),
						"seq":       fmt.Sprintf("%d", j),
					},
				}

				// 计算校验和
				data, _ := proto.Marshal(msg)
				hash := md5.Sum(data)
				msg.Checksum = hex.EncodeToString(hash[:])

				// 重新序列化包含校验和的消息
				data, err = proto.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				// 发送消息
				_, err = conn.Write(data)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}
				atomic.AddInt64(&result.BytesSent, int64(len(data)))

				// 设置读取超时
				conn.SetReadDeadline(time.Now().Add(config.Timeout))

				// 读取响应
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}
				atomic.AddInt64(&result.BytesReceived, int64(n))

				// 解析响应
				var response protobuf.Message
				err = proto.Unmarshal(buf[:n], &response)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				// 检查是否是错误响应
				if response.Route == "error" {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				atomic.AddInt64(&result.SuccessfulRequests, 1)
				latency := time.Since(reqStart)
				latencyMutex.Lock()
				latencies = append(latencies, latency)
				latencyMutex.Unlock()
			}
		}(i)
	}

	wg.Wait()

	result.TotalRequests = result.SuccessfulRequests + result.FailedRequests
	result.TotalTime = time.Since(startTime)
	if result.TotalRequests > 0 && result.TotalTime.Seconds() > 0 {
		result.QPS = float64(result.SuccessfulRequests) / result.TotalTime.Seconds()
		result.SuccessRate = float64(result.SuccessfulRequests) / float64(result.TotalRequests) * 100
	}
	if len(latencies) > 0 {
		result.AvgLatency = calculateAverage(latencies)
		result.P95Latency = calculatePercentile(latencies, 0.95)
		result.P99Latency = calculatePercentile(latencies, 0.99)
		result.MinLatency = calculateMin(latencies)
		result.MaxLatency = calculateMax(latencies)
	}

	return result
}

func runUDPStressTest(config *StressConfig) *StressResult {
	result := &StressResult{Protocol: "UDP"}
	startTime := time.Now()

	var wg sync.WaitGroup
	wg.Add(config.UDPConcurrency)

	latencies := make([]time.Duration, 0, config.UDPConcurrency*config.RequestsPerConn)
	var latencyMutex sync.Mutex

	for i := 0; i < config.UDPConcurrency; i++ {
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("udp", fmt.Sprintf("%s:48081", config.ServerAddr))
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, int64(config.RequestsPerConn))
				return
			}
			defer conn.Close()

			for j := 0; j < config.RequestsPerConn; j++ {
				reqStart := time.Now()

				// 创建 ping 消息
				msg := &protobuf.Message{
					ConnectionId: fmt.Sprintf("udp-%d-%d", connID, j),
					Route: "ping",
					Payload: map[string]string{
						"message":   fmt.Sprintf("ping-%d-%d-%d", connID, j, time.Now().UnixNano()),
						"timestamp": fmt.Sprintf("%d", time.Now().UnixNano()),
						"seq":       fmt.Sprintf("%d", j),
					},
				}

				// 计算校验和
				data, _ := proto.Marshal(msg)
				hash := md5.Sum(data)
				msg.Checksum = hex.EncodeToString(hash[:])

				// 重新序列化包含校验和的消息
				data, err = proto.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				// 发送消息
				_, err = conn.Write(data)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}
				atomic.AddInt64(&result.BytesSent, int64(len(data)))

				// 设置读取超时
				conn.SetReadDeadline(time.Now().Add(config.Timeout))

				// 读取响应
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					// UDP 可能丢失数据包，忽略超时错误
					continue
				}
				atomic.AddInt64(&result.BytesReceived, int64(n))

				// 解析响应
				var response protobuf.Message
				err = proto.Unmarshal(buf[:n], &response)
				if err != nil {
					continue
				}

				// 检查是否是错误响应
				if response.Route == "error" {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				atomic.AddInt64(&result.SuccessfulRequests, 1)
				latency := time.Since(reqStart)
				latencyMutex.Lock()
				latencies = append(latencies, latency)
				latencyMutex.Unlock()
			}
		}(i)
	}

	wg.Wait()

	result.TotalRequests = result.SuccessfulRequests + result.FailedRequests
	result.TotalTime = time.Since(startTime)
	if result.TotalRequests > 0 && result.TotalTime.Seconds() > 0 {
		result.QPS = float64(result.SuccessfulRequests) / result.TotalTime.Seconds()
		result.SuccessRate = float64(result.SuccessfulRequests) / float64(result.TotalRequests) * 100
	}
	if len(latencies) > 0 {
		result.AvgLatency = calculateAverage(latencies)
		result.P95Latency = calculatePercentile(latencies, 0.95)
		result.P99Latency = calculatePercentile(latencies, 0.99)
		result.MinLatency = calculateMin(latencies)
		result.MaxLatency = calculateMax(latencies)
	}

	return result
}

func runWSStressTest(config *StressConfig) *StressResult {
	result := &StressResult{Protocol: "WebSocket"}
	startTime := time.Now()

	var wg sync.WaitGroup
	wg.Add(config.WSConcurrency)

	latencies := make([]time.Duration, 0, config.WSConcurrency*config.RequestsPerConn)
	var latencyMutex sync.Mutex

	for i := 0; i < config.WSConcurrency; i++ {
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", fmt.Sprintf("%s:48082", config.ServerAddr))
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, int64(config.RequestsPerConn))
				return
			}
			defer conn.Close()

			// WebSocket 握手
			handshakeReq := "GET /ws HTTP/1.1\r\n" +
				"Host: localhost:48082\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
				"Sec-WebSocket-Version: 13\r\n\r\n"

			_, err = conn.Write([]byte(handshakeReq))
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, int64(config.RequestsPerConn))
				return
			}

			// 读取握手响应
			buf := make([]byte, 4096)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = conn.Read(buf)
			if err != nil {
				atomic.AddInt64(&result.FailedRequests, int64(config.RequestsPerConn))
				return
			}

			// 发送消息循环
			for j := 0; j < config.RequestsPerConn; j++ {
				reqStart := time.Now()

				// 创建 ping 消息
				msg := &protobuf.Message{
					ConnectionId: fmt.Sprintf("ws-%d-%d", connID, j),
					Route: "ping",
					Payload: map[string]string{
						"message":   fmt.Sprintf("ping-%d-%d-%d", connID, j, time.Now().UnixNano()),
						"timestamp": fmt.Sprintf("%d", time.Now().UnixNano()),
						"seq":       fmt.Sprintf("%d", j),
					},
				}

				// 计算校验和
				data, _ := proto.Marshal(msg)
				hash := md5.Sum(data)
				msg.Checksum = hex.EncodeToString(hash[:])

				// 重新序列化包含校验和的消息
				data, err = proto.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				// 封装为 WebSocket 帧
				wsFrame := createWebSocketFrame(data)

				// 发送消息
				_, err = conn.Write(wsFrame)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}
				atomic.AddInt64(&result.BytesSent, int64(len(wsFrame)))

				// 设置读取超时
				conn.SetReadDeadline(time.Now().Add(config.Timeout))

				// 读取响应
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}
				atomic.AddInt64(&result.BytesReceived, int64(n))

				// 解析 WebSocket 帧
				responseData := parseWebSocketFrame(buf[:n])

				// 解析响应
				var response protobuf.Message
				err = proto.Unmarshal(responseData, &response)
				if err != nil {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				// 检查是否是错误响应
				if response.Route == "error" {
					atomic.AddInt64(&result.FailedRequests, 1)
					continue
				}

				atomic.AddInt64(&result.SuccessfulRequests, 1)
				latency := time.Since(reqStart)
				latencyMutex.Lock()
				latencies = append(latencies, latency)
				latencyMutex.Unlock()
			}
		}(i)
	}

	wg.Wait()

	result.TotalRequests = result.SuccessfulRequests + result.FailedRequests
	result.TotalTime = time.Since(startTime)
	if result.TotalRequests > 0 && result.TotalTime.Seconds() > 0 {
		result.QPS = float64(result.SuccessfulRequests) / result.TotalTime.Seconds()
		result.SuccessRate = float64(result.SuccessfulRequests) / float64(result.TotalRequests) * 100
	}
	if len(latencies) > 0 {
		result.AvgLatency = calculateAverage(latencies)
		result.P95Latency = calculatePercentile(latencies, 0.95)
		result.P99Latency = calculatePercentile(latencies, 0.99)
		result.MinLatency = calculateMin(latencies)
		result.MaxLatency = calculateMax(latencies)
	}

	return result
}

func createWebSocketFrame(data []byte) []byte {
	frame := make([]byte, 0, len(data)+10)
	frame = append(frame, 0x81)

	if len(data) < 126 {
		frame = append(frame, byte(len(data)))
	} else if len(data) < 65536 {
		frame = append(frame, 126)
		frame = append(frame, byte(len(data)>>8), byte(len(data)))
	}

	frame = append(frame, data...)
	return frame
}

func parseWebSocketFrame(data []byte) []byte {
	if len(data) < 2 {
		return data
	}

	opcode := data[0] & 0x0F
	if opcode != 1 && opcode != 2 {
		return data
	}

	length := int(data[1] & 0x7F)
	offset := 2

	if length == 126 {
		if len(data) < 4 {
			return data
		}
		length = int(data[2])<<8 | int(data[3])
		offset = 4
	} else if length == 127 {
		if len(data) < 10 {
			return data
		}
		length = 0
		for i := 0; i < 8; i++ {
			length = length<<8 | int(data[2+i])
		}
		offset = 10
	}

	if data[1]&0x80 != 0 {
		offset += 4
	}

	if offset+length > len(data) {
		return data[offset:]
	}
	return data[offset : offset+length]
}

func calculateAverage(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	var total time.Duration
	for _, latency := range latencies {
		total += latency
	}
	return total / time.Duration(len(latencies))
}

func calculatePercentile(latencies []time.Duration, percentile float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	index := int(float64(len(sorted)) * percentile)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}

	return sorted[index]
}

func calculateMin(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	min := latencies[0]
	for _, latency := range latencies[1:] {
		if latency < min {
			min = latency
		}
	}
	return min
}

func calculateMax(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	max := latencies[0]
	for _, latency := range latencies[1:] {
		if latency > max {
			max = latency
		}
	}
	return max
}

func printResult(result *StressResult) {
	fmt.Printf("\n【%s 协议压测结果】\n", result.Protocol)
	fmt.Printf("  总请求数: %d\n", result.TotalRequests)
	fmt.Printf("  成功请求数: %d\n", result.SuccessfulRequests)
	fmt.Printf("  失败请求数: %d\n", result.FailedRequests)
	fmt.Printf("  成功率: %.2f%%\n", result.SuccessRate)
	fmt.Printf("  总时间: %v\n", result.TotalTime)
	fmt.Printf("  QPS: %.2f\n", result.QPS)
	fmt.Printf("  平均延迟: %v\n", result.AvgLatency)
	fmt.Printf("  最小延迟: %v\n", result.MinLatency)
	fmt.Printf("  最大延迟: %v\n", result.MaxLatency)
	fmt.Printf("  P95 延迟: %v\n", result.P95Latency)
	fmt.Printf("  P99 延迟: %v\n", result.P99Latency)
	fmt.Printf("  发送字节数: %d KB\n", result.BytesSent/1024)
	fmt.Printf("  接收字节数: %d KB\n", result.BytesReceived/1024)
}

func printSummary(results []*StressResult) {
	fmt.Println("\n========================================")
	fmt.Println("           压测汇总报告")
	fmt.Println("========================================")

	var totalQPS float64
	var totalRequests int64
	var totalSuccessful int64
	var totalFailed int64

	for _, result := range results {
		totalQPS += result.QPS
		totalRequests += result.TotalRequests
		totalSuccessful += result.SuccessfulRequests
		totalFailed += result.FailedRequests
	}

	fmt.Printf("总请求数: %d\n", totalRequests)
	fmt.Printf("总成功请求数: %d\n", totalSuccessful)
	fmt.Printf("总失败请求数: %d\n", totalFailed)
	if totalRequests > 0 {
		fmt.Printf("总成功率: %.2f%%\n", float64(totalSuccessful)/float64(totalRequests)*100)
	}
	fmt.Printf("总 QPS: %.2f\n", totalQPS)
	fmt.Println("========================================")
	fmt.Println("压测完成！")
}
