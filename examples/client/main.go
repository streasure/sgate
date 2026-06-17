package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/protobuf"
	"google.golang.org/protobuf/proto"
)

type SgateClient struct {
	conn              net.Conn
	addr              string
	serverID          string
	userID            string
	token             string
	protocolVersion   string
	connectionID      string
	userUUID          string
	negotiatedVersion string

	sequence        int64
	mu              sync.Mutex
	pendingRequests map[string]chan *protobuf.Message
	routeHandlers   map[string]func(*protobuf.Message)
	onDisconnected  func()

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type ClientOptions struct {
	Addr            string
	ServerID        string
	UserID          string
	Token           string
	ProtocolVersion string
}

func NewSgateClient(opts ClientOptions) *SgateClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &SgateClient{
		addr:            opts.Addr,
		serverID:        opts.ServerID,
		userID:          opts.UserID,
		token:           opts.Token,
		protocolVersion: opts.ProtocolVersion,
		pendingRequests: make(map[string]chan *protobuf.Message),
		routeHandlers:   make(map[string]func(*protobuf.Message)),
		ctx:             ctx,
		cancel:          cancel,
	}
}

func (c *SgateClient) Connect() error {
	conn, err := net.DialTimeout("tcp", c.addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	c.conn = conn
	c.wg.Add(1)
	go c.receiveLoop()
	return nil
}

func (c *SgateClient) Handshake() error {
	handshake := &protobuf.Handshake{
		ProtocolVersion: c.protocolVersion,
		ClientType:      "cli",
		ClientVersion:   "1.0.0",
		Timestamp:       time.Now().UnixMilli(),
	}
	handshake.SupportedVersions = append(handshake.SupportedVersions, c.protocolVersion)

	handshakeData, err := proto.Marshal(handshake)
	if err != nil {
		return fmt.Errorf("marshal handshake: %w", err)
	}

	handshakeBase64 := encodeBase64(handshakeData)

	msg := &protobuf.Message{
		Route:           protobuf.RouteHandshake,
		ProtocolVersion: c.protocolVersion,
		Payload: map[string]string{
			"handshake_data": handshakeBase64,
			"version":        c.protocolVersion,
			"timestamp":      strconv.FormatInt(time.Now().UnixMilli(), 10),
			"serverId":       c.serverID,
		},
	}

	resp, err := c.sendAndWait(msg, protobuf.RouteHandshakeResponse, 0, 10*time.Second)
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	if v, ok := resp.Payload["negotiated_version"]; ok {
		c.negotiatedVersion = v
	}
	return nil
}

func (c *SgateClient) Login() error {
	msg := &protobuf.Message{
		Route: protobuf.RouteLogin,
		Payload: map[string]string{
			"userId":   c.userID,
			"token":    c.token,
			"serverId": c.serverID,
		},
	}

	resp, err := c.sendAndWait(msg, protobuf.RoutePlayerLogin, 0, 10*time.Second)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	if code, ok := resp.Payload["code"]; ok && code != "200" {
		errMsg := "login failed"
		if m, ok := resp.Payload["message"]; ok {
			errMsg = m
		}
		return fmt.Errorf("%s (code=%s)", errMsg, code)
	}

	c.connectionID = resp.ConnectionId
	c.userUUID = resp.UserUuid
	return nil
}

func (c *SgateClient) ConnectAndLogin() error {
	if err := c.Connect(); err != nil {
		return err
	}
	if err := c.Handshake(); err != nil {
		return err
	}
	if err := c.Login(); err != nil {
		return err
	}
	return nil
}

func (c *SgateClient) Send(route string, payload map[string]string) (*protobuf.Message, error) {
	msg := &protobuf.Message{
		Route:     route,
		Timestamp: time.Now().UnixMilli(),
		Sequence:  atomic.AddInt64(&c.sequence, 1),
		Payload:   payload,
	}
	expectedRoute := route
	if mapped, ok := responseRouteMap[route]; ok {
		expectedRoute = mapped
	}
	return c.sendAndWait(msg, expectedRoute, 0, 30*time.Second)
}

func (c *SgateClient) SendProto(route string, req proto.Message) (*protobuf.Message, error) {
	cmd, respCmd := protobuf.CmdFromProto(route, req)
	data, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	msg := &protobuf.Message{
		Route:     route,
		Cmd:       cmd,
		Data:      data,
		Timestamp: time.Now().UnixMilli(),
		Sequence:  atomic.AddInt64(&c.sequence, 1),
	}
	return c.sendAndWait(msg, route, respCmd, 30*time.Second)
}

func (c *SgateClient) On(route string, handler func(*protobuf.Message)) {
	c.routeHandlers[route] = handler
}

func (c *SgateClient) Close() {
	c.cancel()
	if c.conn != nil {
		c.conn.Close()
	}
	c.wg.Wait()
}

var responseRouteMap = map[string]string{
	protobuf.RoutePing: protobuf.RoutePong,
}

func (c *SgateClient) sendAndWait(msg *protobuf.Message, expectedRoute string, expectedCmd int32, timeout time.Duration) (*protobuf.Message, error) {
	prepareMessage(msg)

	ch := make(chan *protobuf.Message, 1)
	var key string
	if expectedCmd != 0 {
		key = fmt.Sprintf("%s:%d", expectedRoute, expectedCmd)
	} else {
		key = expectedRoute
	}

	c.mu.Lock()
	c.pendingRequests[key] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pendingRequests, key)
		c.mu.Unlock()
	}()

	if err := c.writeMessage(msg); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response to route '%s' cmd=%d", msg.Route, msg.Cmd)
	case <-c.ctx.Done():
		return nil, fmt.Errorf("client closed")
	}
}

func (c *SgateClient) writeMessage(msg *protobuf.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	_, err = c.conn.Write(frame)
	return err
}

func (c *SgateClient) receiveLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.conn.SetReadDeadline(time.Now().Add(35 * time.Second))
		header := make([]byte, 4)
		if _, err := io.ReadFull(c.conn, header); err != nil {
			if c.ctx.Err() != nil {
				return
			}
			if err == io.EOF {
				if c.onDisconnected != nil {
					c.onDisconnected()
				}
				return
			}
			continue
		}

		payloadSize := binary.BigEndian.Uint32(header)
		if payloadSize == 0 || payloadSize > 16*1024*1024 {
			continue
		}

		payload := make([]byte, payloadSize)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			continue
		}

		msg := &protobuf.Message{}
		if err := proto.Unmarshal(payload, msg); err != nil {
			continue
		}

		c.dispatchMessage(msg)
	}
}

func (c *SgateClient) dispatchMessage(msg *protobuf.Message) {
	c.mu.Lock()

	var keys []string
	if msg.Cmd != 0 {
		keys = []string{
			fmt.Sprintf("%s:%d", msg.Route, msg.Cmd),
			msg.Route,
		}
	} else {
		keys = []string{msg.Route}
	}

	for _, key := range keys {
		if ch, ok := c.pendingRequests[key]; ok {
			delete(c.pendingRequests, key)
			c.mu.Unlock()
			ch <- msg
			return
		}
	}

	handler, hasHandler := c.routeHandlers[msg.Route]
	c.mu.Unlock()

	if hasHandler {
		handler(msg)
	}
}

func prepareMessage(msg *protobuf.Message) {
	msg.Timestamp = time.Now().UnixMilli()
	if msg.ProtocolVersion == "" {
		msg.ProtocolVersion = "2.0.0"
	}
	msg.Checksum = generateChecksum(msg)
}

func generateChecksum(msg *protobuf.Message) string {
	var buf strings.Builder
	buf.WriteString(msg.ConnectionId)
	buf.WriteString("|")
	buf.WriteString(msg.UserUuid)
	buf.WriteString("|")
	buf.WriteString(msg.Route)
	buf.WriteString("|")
	buf.WriteString(strconv.FormatInt(int64(msg.Cmd), 10))
	buf.WriteString("|")

	keys := make([]string, 0, len(msg.Payload))
	for k := range msg.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteString("=")
		buf.WriteString(msg.Payload[k])
		buf.WriteString("|")
	}

	buf.WriteString(strconv.FormatInt(msg.Timestamp, 10))
	buf.WriteString("|")
	buf.WriteString(strconv.FormatInt(msg.Sequence, 10))
	buf.WriteString("|")
	buf.WriteString(msg.ProtocolVersion)

	hash := md5.Sum([]byte(buf.String()))
	return hex.EncodeToString(hash[:])
}

func encodeBase64(data []byte) string {
	const base64Table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	var result []byte
	for i := 0; i < len(data); i += 3 {
		var n uint32
		remaining := len(data) - i
		if remaining >= 3 {
			n = uint32(data[i])<<16 | uint32(data[i+1])<<8 | uint32(data[i+2])
			result = append(result, base64Table[n>>18&0x3F], base64Table[n>>12&0x3F], base64Table[n>>6&0x3F], base64Table[n&0x3F])
		} else if remaining == 2 {
			n = uint32(data[i])<<16 | uint32(data[i+1])<<8
			result = append(result, base64Table[n>>18&0x3F], base64Table[n>>12&0x3F], base64Table[n>>6&0x3F], '=')
		} else {
			n = uint32(data[i]) << 16
			result = append(result, base64Table[n>>18&0x3F], base64Table[n>>12&0x3F], '=', '=')
		}
	}
	return string(result)
}

func formatPayload(msg *protobuf.Message) string {
	parts := make([]string, 0, len(msg.Payload))
	for k, v := range msg.Payload {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	if len(msg.Data) > 0 {
		parts = append(parts, fmt.Sprintf("data=%dbytes", len(msg.Data)))
	}
	return fmt.Sprintf("route=%s cmd=%d [%s]", msg.Route, msg.Cmd, strings.Join(parts, ", "))
}

func main() {
	addr := "127.0.0.1:48080"
	if len(os.Args) >= 2 {
		addr = os.Args[1]
	}
	serverID := "S1"
	if len(os.Args) >= 3 {
		serverID = os.Args[2]
	}
	userID := "go_client_user"
	if len(os.Args) >= 4 {
		userID = os.Args[3]
	}

	client := NewSgateClient(ClientOptions{
		Addr:            addr,
		ServerID:        serverID,
		UserID:          userID,
		Token:           "test-token",
		ProtocolVersion: "2.0.0",
	})

	client.On(protobuf.RouteServerKick, func(msg *protobuf.Message) {
		fmt.Printf("[KICKED] %v\n", msg.Payload)
	})

	client.On(protobuf.RouteServerAnnounce, func(msg *protobuf.Message) {
		fmt.Printf("[ANNOUNCEMENT] %v\n", msg.Payload)
	})

	client.On(protobuf.RouteServerChat, func(msg *protobuf.Message) {
		fmt.Printf("[CHAT] %v\n", msg.Payload)
	})

	defer client.Close()

	fmt.Printf("Connecting to %s ...\n", addr)
	if err := client.Connect(); err != nil {
		fmt.Printf("Connect failed: %v\n", err)
		return
	}
	fmt.Println("Connected!")

	fmt.Println("Performing handshake...")
	if err := client.Handshake(); err != nil {
		fmt.Printf("Handshake failed: %v\n", err)
		return
	}
	fmt.Printf("Handshake OK, negotiated version: %s\n", client.negotiatedVersion)

	fmt.Println("Logging in...")
	if err := client.Login(); err != nil {
		fmt.Printf("Login failed: %v\n", err)
		fmt.Println("Continuing without login...")
	} else {
		fmt.Printf("Login OK, connectionId: %s, userUUID: %s\n", client.connectionID, client.userUUID)
	}

	fmt.Println("\n--- Test 1: Ping ---")
	start := time.Now()
	resp, err := client.Send(protobuf.RoutePing, nil)
	if err != nil {
		fmt.Printf("Ping failed: %v\n", err)
	} else {
		fmt.Printf("Ping OK: %s latency=%v\n", formatPayload(resp), time.Since(start))
	}

	fmt.Println("\n--- Test 2: Test ---")
	start = time.Now()
	resp, err = client.Send(protobuf.RouteTest, map[string]string{
		"data": "Go client test payload",
	})
	if err != nil {
		fmt.Printf("Test failed: %v\n", err)
	} else {
		fmt.Printf("Test OK: %s latency=%v\n", formatPayload(resp), time.Since(start))
	}

	fmt.Println("\n--- Test 3: Game Login (struct payload) ---")
	start = time.Now()
	loginResp, err := client.SendProto(protobuf.RouteGame, &protobuf.LoginReq{
		UserId:      userID,
		LoginKey:    "test-key",
		ServerIndex: 1,
		Channel:     1,
		IsReconnect: false,
		AreaId:      1,
	})
	if err != nil {
		fmt.Printf("Game Login failed: %v\n", err)
	} else {
		loginAck := &protobuf.LoginAck{}
		if err := proto.Unmarshal(loginResp.Data, loginAck); err != nil {
			fmt.Printf("Decode LoginAck failed: %v\n", err)
		} else {
			fmt.Printf("Game Login OK: user=%s serverTime=%d version=%s latency=%v\n",
				loginAck.User.GetUuid(), loginAck.ServerTime, loginAck.Version, time.Since(start))
		}
	}

	fmt.Println("\n--- Test 4: Game Logout (struct payload) ---")
	start = time.Now()
	logoutResp, err := client.SendProto(protobuf.RouteGame, &protobuf.LogoutReq{
		UserId: userID,
		AreaId: 1,
	})
	if err != nil {
		fmt.Printf("Game Logout failed: %v\n", err)
	} else {
		logoutAck := &protobuf.LogoutAck{}
		if err := proto.Unmarshal(logoutResp.Data, logoutAck); err != nil {
			fmt.Printf("Decode LogoutAck failed: %v\n", err)
		} else {
			fmt.Printf("Game Logout OK: userId=%s code=%d message=%s latency=%v\n",
				logoutAck.UserId, logoutAck.Code, logoutAck.Message, time.Since(start))
		}
	}

	fmt.Println("\nAll tests completed!")
}
