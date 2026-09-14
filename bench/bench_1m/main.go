// bench_1m 百万连接压测程序
// 测试 sgate 网关在大量并发 TCP 连接下的承载能力。
// 多端口分发：本地每端口最多 ~6K 连接（Windows 临时端口限制）。
// 用法：
//   go run .\bench\bench_1m -addr 127.0.0.1:48080 -connections 1000000
//   go run .\bench\bench_1m -addr 127.0.0.1 -ports 48080-49050 -connections 1000000
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

var (
	addr      = flag.String("addr", "127.0.0.1", "网关 IP 地址")
	ports     = flag.String("ports", "48080", "端口列表，逗号分隔或 start-end 范围")
	total     = flag.Int("connections", 1000000, "目标连接数")
	batchSize = flag.Int("batch", 2000, "每批创建连接数")
	interval  = flag.Duration("interval", 100*time.Millisecond, "每批间隔")
	stayOpen  = flag.Duration("stay", 30*time.Second, "连接保持时间")
)

func main() {
	flag.Parse()
	runtime.GOMAXPROCS(runtime.NumCPU())

	// 解析端口列表
	portList := parsePorts(*ports)
	fmt.Printf("=== 百万 TCP 连接压测 ===\n")
	fmt.Printf("地址: %s\n", *addr)
	fmt.Printf("端口: %v (%d 个)\n", portList, len(portList))
	fmt.Printf("目标连接数: %d\n", *total)
	fmt.Printf("每批: %d, 间隔: %v\n", *batchSize, *interval)
	fmt.Printf("保持时间: %v\n\n", *stayOpen)

	var successCount, failCount atomic.Int64
	var mu sync.Mutex
	conns := make([]net.Conn, 0, *total)

	startTime := time.Now()
	remaining := *total
	batchNum := 0

	for remaining > 0 {
		n := *batchSize
		if n > remaining {
			n = remaining
		}
		batchNum++

		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 随机选一个端口
				port := portList[rand.Intn(len(portList))]
				target := fmt.Sprintf("%s:%d", *addr, port)

				conn, err := net.DialTimeout("tcp", target, 3*time.Second)
				if err != nil {
					failCount.Add(1)
					return
				}
				// 发送 LoginGate 请求激活连接
				sendLoginGate(conn)
				successCount.Add(1)
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
			}()
		}
		wg.Wait()

		remaining -= n
		elapsed := time.Since(startTime)
		cur := successCount.Load()
		rate := float64(cur) / elapsed.Seconds()

		fmt.Printf("\r[%s] 批次 %d | 连接: %d/%d | 失败: %d | 速率: %.0f/s | 内存: %dMB | Goroutines: %d",
			time.Now().Format("15:04:05"),
			batchNum, cur, *total, failCount.Load(), rate, getMemMB(), runtime.NumGoroutine())

		if remaining > 0 {
			time.Sleep(*interval)
		}
	}

	elapsed := time.Since(startTime)
	fmt.Printf("\n\n=== 连接完成 ===\n")
	fmt.Printf("成功: %d\n", successCount.Load())
	fmt.Printf("失败: %d\n", failCount.Load())
	fmt.Printf("耗时: %v\n", elapsed)
	fmt.Printf("速率: %.0f conn/s\n", float64(successCount.Load())/elapsed.Seconds())
	fmt.Printf("内存: %dMB\n", getMemMB())

	// 打印端口分布
	fmt.Printf("\n保持连接 %v... (Ctrl+C 退出)\n\n", *stayOpen)
	<-time.After(*stayOpen)

	// 关闭
	fmt.Printf("关闭 %d 个连接...\n", len(conns))
	for _, c := range conns {
		c.Close()
	}
	fmt.Printf("完成\n")
}

func parsePorts(s string) []int {
	var ports []int
	// 支持 "48080" 和 "49000-49050" 格式
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if idx := strings.Index(part, "-"); idx >= 0 {
			start, _ := strconv.Atoi(part[:idx])
			end, _ := strconv.Atoi(part[idx+1:])
			for i := start; i <= end; i++ {
				ports = append(ports, i)
			}
		} else {
			p, _ := strconv.Atoi(part)
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		ports = []int{48080}
	}
	return ports
}

func sendLoginGate(conn net.Conn) {
	req := &gateway.LoginGateReq{
		UserId:   "bench_user",
		ServerId: "logic1",
	}
	data, _ := proto.Marshal(req)
	frame := &gateway.MessageFrame{
		Cmd:  1000001,
		Body: data,
	}
	frameData, _ := proto.Marshal(frame)
	buf := make([]byte, 4+len(frameData))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(frameData)))
	copy(buf[4:], frameData)
	conn.Write(buf)
}

func getMemMB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc / 1024 / 1024
}
