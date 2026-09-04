package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/gateway"
	"google.golang.org/protobuf/proto"
)

const maxInflight = 8192

var sent, received, authFailed int64

func rawFrame(cmd int32, body []byte) []byte {
	data, _ := proto.Marshal(&gateway.MessageFrame{Cmd: cmd, Body: body})
	return data
}

func writeFrame(conn net.Conn, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x82)
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xffff:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(n))
		header = append(header, length[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(masked)
	return err
}

func readFrame(conn net.Conn) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7f)
	if length == 126 {
		var data [2]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(data[:]))
	} else if length == 127 {
		var data [8]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(data[:])
	}
	if length > 4*1024*1024 {
		return nil, fmt.Errorf("oversized WebSocket frame: %d", length)
	}
	payload := make([]byte, int(length))
	_, err := io.ReadFull(conn, payload)
	return payload, err
}

func connect(rawURL string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "ws" || u.Host == "" {
		return nil, fmt.Errorf("invalid ws URL %q", rawURL)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var keyData [16]byte
	_, _ = rand.Read(keyData[:])
	key := base64.StdEncoding.EncodeToString(keyData[:])
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, u.Host, key)
	if _, err := conn.Write([]byte(request)); err != nil {
		conn.Close()
		return nil, err
	}
	response := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for !bytes.Contains(response, []byte("\r\n\r\n")) {
		n, readErr := conn.Read(buf)
		response = append(response, buf[:n]...)
		if readErr != nil {
			conn.Close()
			return nil, readErr
		}
		if len(response) > 16*1024 {
			conn.Close()
			return nil, fmt.Errorf("websocket handshake response too large")
		}
	}
	text := string(response)
	end := strings.Index(text, "\r\n\r\n")
	if !strings.HasPrefix(text, "HTTP/1.1 101 Switching Protocols\r\n") {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %q", text)
	}
	accept := ""
	for _, line := range strings.Split(text[:end], "\r\n")[1:] {
		const prefix = "Sec-WebSocket-Accept: "
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			accept = line[len(prefix):]
		}
	}
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if accept != base64.StdEncoding.EncodeToString(hash[:]) {
		conn.Close()
		return nil, fmt.Errorf("invalid websocket accept")
	}
	return conn, nil
}

func run(wsURL string, id int, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	conn, err := connect(wsURL)
	if err != nil {
		atomic.AddInt64(&authFailed, 1)
		return
	}
	defer conn.Close()
	gateBody, _ := proto.Marshal(&gateway.LoginGateReq{ServerId: "logic-1", UserId: fmt.Sprintf("ws_%d", id)})
	if err := writeFrame(conn, rawFrame(gateway.CmdLoginGate, gateBody)); err != nil {
		atomic.AddInt64(&authFailed, 1)
		return
	}
	if _, err := readFrame(conn); err != nil {
		atomic.AddInt64(&authFailed, 1)
		return
	}
	loginBody, _ := proto.Marshal(&protoLogic.LoginReq{UserId: fmt.Sprintf("ws_%d", id)})
	if err := writeFrame(conn, rawFrame(gateway.CmdLogicLoginReq, loginBody)); err != nil {
		atomic.AddInt64(&authFailed, 1)
		return
	}
	if _, err := readFrame(conn); err != nil {
		atomic.AddInt64(&authFailed, 1)
		return
	}
	heartbeat, _ := proto.Marshal(&protoLogic.HeartbeatReq{ClientTime: time.Now().UnixMilli()})
	payload := rawFrame(gateway.CmdHeartbeatReq, heartbeat)
	var inflight atomic.Int64
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			if _, err := readFrame(conn); err != nil {
				return
			}
			atomic.AddInt64(&received, 1)
			inflight.Add(-1)
		}
	}()
	for {
		select {
		case <-stop:
			conn.Close()
			<-recvDone
			return
		default:
		}
		if inflight.Load() >= maxInflight {
			time.Sleep(time.Millisecond)
			continue
		}
		if err := writeFrame(conn, payload); err != nil {
			return
		}
		atomic.AddInt64(&sent, 1)
		inflight.Add(1)
	}
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <ws-url> <connections> <duration-seconds>\n", os.Args[0])
		os.Exit(1)
	}
	var connections, seconds int
	fmt.Sscanf(os.Args[2], "%d", &connections)
	fmt.Sscanf(os.Args[3], "%d", &seconds)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < connections; i++ {
		wg.Add(1)
		go run(os.Args[1], i, stop, &wg)
	}
	start := time.Now()
	time.Sleep(time.Duration(seconds) * time.Second)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	fmt.Printf("WebSocket benchmark: connections=%d duration=%.2fs sent=%d received=%d recvQPS=%.0f authFailed=%d\n", connections, elapsed, sent, received, float64(received)/elapsed, authFailed)
}
