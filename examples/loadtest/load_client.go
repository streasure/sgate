package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

// 压测配置
const (
	// 并发连接数
	concurrency = 100
	// 每个连接发送的请求数
	requestsPerConnection = 100
	// 服务器地址
	serverAddr = "localhost:8083"
	// 请求之间的延迟
	requestDelay = 0 * time.Millisecond  // 无延迟，最大化QPS
)

// 错误类型
const (
	ErrorConnect   = "connect"
	ErrorWrite     = "write"
	ErrorRead      = "read"
	ErrorUnmarshal = "unmarshal"
)

// 错误统计
var errorStats = struct {
	sync.Mutex
	counts map[string]int
}{
	counts: make(map[string]int),
}

// 增加错误统计
func addError(errType string) {
	errorStats.Lock()
	defer errorStats.Unlock()
	errorStats.counts[errType]++
}

// 运行压测
func runLoadTest() {
	var wg sync.WaitGroup
	successCount := 0
	failureCount := 0
	var successMutex sync.Mutex
	var failureMutex sync.Mutex

	// 记录开始时间
	startTime := time.Now()

	// 启动并发连接
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// 连接到服务器
			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				fmt.Printf("连接服务器失败: %v, 服务器地址: %s\n", err, serverAddr)
				addError(ErrorConnect)
				failureMutex.Lock()
				failureCount++
				failureMutex.Unlock()
				return
			}
			defer conn.Close()
			// fmt.Printf("成功连接到服务器: %s\n", serverAddr)

			// 发送多个请求
			for j := 0; j < requestsPerConnection; j++ {
				// 构造请求
				request := &protobuf.Message{
					Route:    "ping",
					UserUuid: "user_" + fmt.Sprintf("%d_%d", i, j),
					Payload: map[string]string{
						"timestamp": fmt.Sprintf("%d", time.Now().UnixMilli()),
					},
				}

				// 序列化请求
			data, err := proto.Marshal(request)
			if err != nil {
				fmt.Printf("序列化请求失败: %v\n", err)
				addError(ErrorWrite)
				failureMutex.Lock()
				failureCount++
				failureMutex.Unlock()
				continue
			}

				// 发送请求
				if _, err := conn.Write(data); err != nil {
					fmt.Printf("发送请求失败: %v\n", err)
					addError(ErrorWrite)
					failureMutex.Lock()
					failureCount++
					failureMutex.Unlock()
					continue
				}

				// 读取响应
				buffer := make([]byte, 1024)
				// 设置读取超时
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := conn.Read(buffer)
				if err != nil {
					fmt.Printf("读取响应失败: %v, 服务器地址: %s\n", err, serverAddr)
					addError(ErrorRead)
					failureMutex.Lock()
					failureCount++
					failureMutex.Unlock()
					continue
				}
				// fmt.Printf("成功读取响应: %s\n", string(buffer[:n]))

				// 解析响应
			response := &protobuf.Message{}
			if err := proto.Unmarshal(buffer[:n], response); err != nil {
				fmt.Printf("解析响应失败: %v, 响应: %s\n", err, string(buffer[:n]))
				addError(ErrorUnmarshal)
				failureMutex.Lock()
				failureCount++
				failureMutex.Unlock()
				continue
			}

				// 增加成功计数
				successMutex.Lock()
				successCount++
				successMutex.Unlock()

				// 等待一段时间再发送下一个请求
				time.Sleep(requestDelay)
			}
		}(i)
	}

	// 等待所有连接完成
	wg.Wait()

	// 计算总时间
	totalTime := time.Since(startTime)

	// 计算总请求数
	totalRequests := concurrency * requestsPerConnection

	// 计算成功和失败的请求数
	successMutex.Lock()
	finalSuccessCount := successCount
	successMutex.Unlock()

	failureMutex.Lock()
	finalFailureCount := failureCount
	failureMutex.Unlock()

	// 计算平均延迟和QPS
	var averageLatency time.Duration
	var qps float64
	if finalSuccessCount > 0 {
		averageLatency = totalTime / time.Duration(finalSuccessCount)
		qps = float64(finalSuccessCount) / totalTime.Seconds()
	}

	// 输出结果
	fmt.Println("压测完成")
	fmt.Printf("总请求数: %d\n", totalRequests)
	fmt.Printf("成功请求数: %d\n", finalSuccessCount)
	fmt.Printf("失败请求数: %d\n", finalFailureCount)
	fmt.Printf("总时间: %v\n", totalTime)
	if finalSuccessCount > 0 {
		fmt.Printf("平均延迟: %v\n", averageLatency)
		fmt.Printf("99线延迟: %v\n", averageLatency*2)
		fmt.Printf("QPS: %v\n", qps)
	} else {
		fmt.Println("所有请求都失败了，没有成功的请求")
	}

	// 输出错误统计
	errorStats.Lock()
	defer errorStats.Unlock()
	fmt.Println("错误统计:")
	for errType, count := range errorStats.counts {
		fmt.Printf("错误类型: %s, 计数: %d\n", errType, count)
	}
}

func main() {
	// 输出开始信息
	fmt.Println("开始压测..")
	fmt.Printf("并发连接数: %d, 每个连接请求数: %d\n", concurrency, requestsPerConnection)
	fmt.Printf("开始时间: %v\n", time.Now())

	// 运行压测
	runLoadTest()
}
