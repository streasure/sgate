package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/logic"
	"google.golang.org/protobuf/proto"
)

const (
	loginGateCmd     = int32(1000001)
	userCountDefault = 100
)

type client struct {
	conn     net.Conn
	userUUID string
}

func frame(cmd int32, body []byte) []byte {
	data, _ := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: body})
	result := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(result[:4], uint32(len(data)))
	copy(result[4:], data)
	return result
}

func readFrame(conn net.Conn) error {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > 16*1024*1024 {
		return fmt.Errorf("invalid frame size %d", n)
	}
	_, err := io.CopyN(io.Discard, conn, int64(n))
	return err
}

func connectClient(addr, rawUser string) (*client, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	userUUID := "logic-1:" + rawUser
	request, _ := proto.Marshal(&protoGw.LoginGateReq{ServerId: "logic-1", UserId: rawUser})
	if _, err = conn.Write(frame(loginGateCmd, request)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var header [4]byte
	if _, err = io.ReadFull(conn, header[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("login ack header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > 16*1024*1024 {
		conn.Close()
		return nil, fmt.Errorf("invalid login ack size %d", length)
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(conn, payload); err != nil {
		conn.Close()
		return nil, fmt.Errorf("login ack payload: %w", err)
	}
	var envelope protoGw.MessageFrame
	var ack protoGw.LoginGateAck
	if err = proto.Unmarshal(payload, &envelope); err != nil || envelope.Cmd != 1000002 || proto.Unmarshal(envelope.Body, &ack) != nil || ack.Code != 0 {
		conn.Close()
		return nil, fmt.Errorf("login gate rejected")
	}
	login, _ := proto.Marshal(&protoLogic.LoginReq{UserId: rawUser})
	if _, err = conn.Write(frame(1100001, login)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	return &client{conn: conn, userUUID: userUUID}, nil
}

func drainClient(c *client, received *atomic.Int64, done *sync.WaitGroup) {
	defer done.Done()
	for {
		if err := readFrameFrom(c.conn); err != nil {
			return
		}
		received.Add(1)
	}
}

func readFrameFrom(r io.Reader) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > 16*1024*1024 {
		return fmt.Errorf("invalid frame size %d", n)
	}
	_, err := io.CopyN(io.Discard, r, int64(n))
	return err
}

func waitForGateway(address string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("gateway did not become available: " + address)
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <gateway-addr> <logic-listen-addr> <mode> [clients] [duration] [events-per-second]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "mode: personal | group | broadcast")
		os.Exit(1)
	}
	gatewayAddr, logicAddr, mode := os.Args[1], os.Args[2], os.Args[3]
	clientsCount, duration := userCountDefault, 10
	if len(os.Args) > 4 {
		clientsCount, _ = strconv.Atoi(os.Args[4])
	}
	if len(os.Args) > 5 {
		duration, _ = strconv.Atoi(os.Args[5])
	}
	eventsPerSecond := 1000
	if len(os.Args) > 6 {
		eventsPerSecond, _ = strconv.Atoi(os.Args[6])
	}
	if clientsCount <= 0 || duration <= 0 || eventsPerSecond <= 0 {
		panic("clients, duration, and events-per-second must be positive")
	}

	port := logicAddr
	if host, p, err := net.SplitHostPort(logicAddr); err == nil {
		_ = host
		port = p
	}
	service := logic.NewService(logic.WithListenPort(port), logic.WithServiceID("logic-1"), logic.WithEtcd(""))
	if err := service.Start(); err != nil {
		panic(err)
	}
	defer service.StopImmediate()
	fmt.Printf("push driver logic listening on %s\n", logicAddr)
	waitForGateway(gatewayAddr)
	fmt.Printf("gateway reachable at %s\n", gatewayAddr)

	var received atomic.Int64
	var wg sync.WaitGroup
	users := make([]string, 0, clientsCount)
	connections := make([]*client, 0, clientsCount)
	for i := 0; i < clientsCount; i++ {
		user := fmt.Sprintf("push-user-%d", i)
		c, err := connectClient(gatewayAddr, user)
		if err != nil {
			panic(fmt.Errorf("connect client %d: %w", i, err))
		}
		connections = append(connections, c)
		users = append(users, c.userUUID)
		fmt.Printf("client %d logged in as %s\n", i, c.userUUID)
		wg.Add(1)
		go drainClient(c, &received, &wg)
	}

	// The gateway stream announces the real session ID. Wait briefly for the
	// logic-side user mapping to be populated before starting fan-out.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, user := range users {
			if _, ok := service.Server().GetConnectionIDByUser(user); !ok {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, user := range users {
		if _, ok := service.Server().GetConnectionIDByUser(user); !ok {
			panic(fmt.Errorf("user mapping not ready: %s", user))
		}
	}
	fmt.Printf("user mappings ready: %d\n", len(users))
	if mode == "group" {
		for _, user := range users {
			if err := service.Server().JoinGroupForUser(user, "push-group"); err != nil {
				panic(err)
			}
		}
	}
	start := time.Now()
	var sent int64
	stop := time.After(time.Duration(duration) * time.Second)
	const tickInterval = 10 * time.Millisecond
	perTick := (eventsPerSecond + 99) / 100
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			goto finished
		case <-ticker.C:
			for i := 0; i < perTick; i++ {
				var n int
				switch mode {
				case "personal":
					n = service.Server().SendToUser(users[0], 1100006, []byte("push"))
				case "group":
					n = service.Server().SendToGroup("push-group", 1100006, []byte("group"))
				case "broadcast":
					n = service.Server().Broadcast(1100006, []byte("broadcast"))
				default:
					panic("unknown mode: " + mode)
				}
				atomic.AddInt64(&sent, int64(n))
			}
		}
	}

finished:
	for _, c := range connections {
		_ = c.conn.Close()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	fmt.Printf("Push benchmark: mode=%s clients=%d duration=%.2fs sent=%d received=%d sendQPS=%.0f receiveQPS=%.0f\n", mode, clientsCount, elapsed, sent, received.Load(), float64(sent)/elapsed, float64(received.Load())/elapsed)
}
