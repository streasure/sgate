package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	runtime.GOMAXPROCS(4)

	numClients := 4
	if len(os.Args) > 1 {
		fmt.Sscanf(os.Args[1], "%d", &numClients)
	}

	connsPerClient := 200
	reqsPerConn := 1000
	pipeline := 128
	addr := "localhost:8083"
	serverID := "S1"

	var totalQPS int64
	var totalSuccess int64
	var totalFailed int64

	var wg sync.WaitGroup
	results := make([]string, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			args := []string{
				"e:\\sgate\\fastloadtest.exe",
				fmt.Sprintf("%d", connsPerClient),
				fmt.Sprintf("%d", reqsPerConn),
				fmt.Sprintf("%d", pipeline),
				addr,
				serverID,
			}
			cmd := exec.Command(args[0], args[1:]...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				results[idx] = fmt.Sprintf("Client %d error: %v output: %s", idx, err, string(output))
				return
			}

			lines := strings.Split(string(output), "\n")
			var qps float64
			var success int64
			var failed int64
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "QPS=") {
					fmt.Sscanf(line, "QPS=%f", &qps)
				} else if strings.HasPrefix(line, "Success=") {
					fmt.Sscanf(line, "Success=%d", &success)
				} else if strings.HasPrefix(line, "Failed=") {
					fmt.Sscanf(line, "Failed=%d", &failed)
				}
			}
			atomic.AddInt64(&totalQPS, int64(qps))
			atomic.AddInt64(&totalSuccess, success)
			atomic.AddInt64(&totalFailed, failed)
			results[idx] = fmt.Sprintf("Client %d: QPS=%.0f, Success=%d, Failed=%d", idx, qps, success, failed)
		}(i)
	}

	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)

	fmt.Println("========================================")
	fmt.Println("  Multi-Client Fast Load Test Results")
	fmt.Println("========================================")
	for _, r := range results {
		fmt.Println(r)
	}
	fmt.Println("----------------------------------------")
	fmt.Printf("Total QPS: %d\n", totalQPS)
	fmt.Printf("Total Success: %d\n", totalSuccess)
	fmt.Printf("Total Failed: %d\n", totalFailed)
	fmt.Printf("Wall Time: %v\n", elapsed)
	fmt.Println("========================================")
}
