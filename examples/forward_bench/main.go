package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/gateway"
	"google.golang.org/protobuf/proto"
)

type stats struct {
	Received          int64 `json:"received"`
	Forwarded         int64 `json:"forwarded"`
	DroppedTotal      int64 `json:"droppedTotal"`
	DroppedFull       int64 `json:"droppedFull"`
	DroppedOverload   int64 `json:"droppedOverload"`
	ActiveConnections int64 `json:"activeConnections"`
}

func makeFrame(cmd int32, body []byte) []byte {
	data, _ := proto.Marshal(&gateway.MessageFrame{Cmd: cmd, Body: body})
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}

func readFrame(conn net.Conn) error {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > 16*1024*1024 {
		return fmt.Errorf("invalid frame length %d", n)
	}
	_, err := io.CopyN(io.Discard, conn, int64(n))
	return err
}

func connect(addr string, id int) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	user := fmt.Sprintf("forward-%d", id)
	login, _ := proto.Marshal(&gateway.LoginGateReq{ServerId: "logic-1", UserId: user})
	if _, err = conn.Write(makeFrame(gateway.CmdLoginGate, login)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err = readFrame(conn); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	// Send the logic login so the gateway exercises the normal authenticated
	// forwarding path. The no-op logic intentionally sends no response.
	loginBody, _ := proto.Marshal(&protoLogic.LoginReq{UserId: user})
	if _, err = conn.Write(makeFrame(gateway.CmdLogicLoginReq, loginBody)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func queryStats(addr string) (stats, error) {
	resp, err := http.Get("http://" + addr + "/stats")
	if err != nil {
		return stats{}, err
	}
	defer resp.Body.Close()
	var result stats
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result, err
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <gateway-addr> <connections> [duration] [stats-addr] [messages-per-second]\n", os.Args[0])
		os.Exit(1)
	}
	connections, _ := strconv.Atoi(os.Args[2])
	duration := 10
	if len(os.Args) > 3 {
		duration, _ = strconv.Atoi(os.Args[3])
	}
	statsAddr := "127.0.0.1:8081"
	if len(os.Args) > 4 {
		statsAddr = os.Args[4]
	}
	rate := int64(100000)
	if len(os.Args) > 5 {
		rate, _ = strconv.ParseInt(os.Args[5], 10, 64)
	}
	if rate <= 0 {
		panic("messages-per-second must be positive")
	}

	var sent atomic.Int64
	var connected atomic.Int64
	stop := make(chan struct{})
	active := make([]net.Conn, 0, connections)
	var wg sync.WaitGroup
	for i := 0; i < connections; i++ {
		conn, err := connect(os.Args[1], i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect %d failed: %v\n", i, err)
			continue
		}
		connected.Add(1)
		active = append(active, conn)
		wg.Add(1)
		go func(conn net.Conn, id int) {
			defer wg.Done()
			defer conn.Close()
			body, _ := proto.Marshal(&protoLogic.HeartbeatReq{ClientTime: time.Now().UnixMilli()})
			payload := makeFrame(gateway.CmdHeartbeatReq, body)
			const tickInterval = 10 * time.Millisecond
			perTick := int((rate*tickInterval.Nanoseconds() + int64(connections)*int64(time.Second) - 1) / (int64(connections) * int64(time.Second)))
			if perTick < 1 {
				perTick = 1
			}
			ticker := time.NewTicker(tickInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
				}
				for i := 0; i < perTick; i++ {
					if _, err := conn.Write(payload); err != nil {
						return
					}
					sent.Add(1)
				}
			}
		}(conn, i)
		time.Sleep(20 * time.Microsecond)
	}

	start := time.Now()
	time.Sleep(time.Duration(duration) * time.Second)
	close(stop)
	for _, conn := range active {
		_ = conn.Close()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	result, err := queryStats(statsAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query stats failed: %v\n", err)
	}
	fmt.Printf("Forward benchmark: connections=%d connected=%d duration=%.2fs attempted=%d attemptedQPS=%.0f\n", connections, connected.Load(), elapsed, sent.Load(), float64(sent.Load())/elapsed)
	fmt.Printf("sgate stats: received=%d forwarded=%d dropped=%d droppedFull=%d droppedOverload=%d activeConnections=%d forwardedQPS=%.0f\n", result.Received, result.Forwarded, result.DroppedTotal, result.DroppedFull, result.DroppedOverload, result.ActiveConnections, float64(result.Forwarded)/elapsed)
}
