package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

func main() {
	serverAddr := "localhost:8083"
	if len(os.Args) >= 2 {
		serverAddr = os.Args[1]
	}

	fmt.Printf("Connecting to %s ...\n", serverAddr)
	conn, err := net.DialTimeout("tcp", serverAddr, 10*time.Second)
	if err != nil {
		fmt.Printf("Connect failed: %v\n", err)
		return
	}
	defer conn.Close()
	fmt.Println("Connected!")

	routes := []struct {
		name string
		msg  *protobuf.Message
	}{
		{
			name: "ping",
			msg: &protobuf.Message{
				Route:   "ping",
				Payload: map[string]string{},
			},
		},
		{
			name: "echo",
			msg: &protobuf.Message{
				Route: "echo",
				Payload: map[string]string{
					"message": "hello from client",
				},
			},
		},
		{
			name: "test",
			msg: &protobuf.Message{
				Route: "test",
				Payload: map[string]string{
					"data": "test payload",
				},
			},
		},
		{
			name: "getConnections",
			msg: &protobuf.Message{
				Route:   "getConnections",
				Payload: map[string]string{},
			},
		},
	}

	for _, r := range routes {
		fmt.Printf("\n--- Sending %s ---\n", r.name)

		data, err := proto.Marshal(r.msg)
		if err != nil {
			fmt.Printf("Marshal error: %v\n", err)
			continue
		}

		buf := make([]byte, 4+len(data))
		binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
		copy(buf[4:], data)

		start := time.Now()
		_, err = conn.Write(buf)
		if err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}

		readBuf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := conn.Read(readBuf)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			return
		}
		latency := time.Since(start)

		resp := &protobuf.Message{}
		if err := proto.Unmarshal(readBuf[:n], resp); err != nil {
			errResp := &protobuf.ErrorResponse{}
			if err2 := proto.Unmarshal(readBuf[:n], errResp); err2 == nil {
				fmt.Printf("ErrorResponse: code=%s message=%s latency=%v\n", errResp.Error.Code, errResp.Error.Message, latency)
			} else {
				fmt.Printf("Unmarshal error: %v (raw %d bytes)\n", err, n)
			}
			continue
		}
		fmt.Printf("Response: route=%s payload=%v latency=%v\n", resp.Route, resp.Payload, latency)

		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("\nDone!")
}
