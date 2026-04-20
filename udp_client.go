package main

import (
	"fmt"
	"net"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
)

func main() {
	// 服务器地址
	addr, err := net.ResolveUDPAddr("udp", "localhost:48081")
	if err != nil {
		fmt.Printf("解析地址失败: %v\n", err)
		return
	}

	// 创建 UDP 客户端
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		fmt.Printf("连接服务器失败: %v\n", err)
		return
	}
	defer conn.Close()

	// 创建 ping 请求消息
	pingMsg := &protobuf.Message{
		Route:   "ping",
		Payload: make(map[string]string),
	}

	// 序列化消息
	data, err := proto.Marshal(pingMsg)
	if err != nil {
		fmt.Printf("序列化消息失败: %v\n", err)
		return
	}

	// 发送 ping 请求
	_, err = conn.Write(data)
	if err != nil {
		fmt.Printf("发送请求失败: %v\n", err)
		return
	}

	// 设置超时
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 接收响应
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		fmt.Printf("接收响应失败: %v\n", err)
		return
	}

	// 反序列化响应
	responseMsg := &protobuf.Message{}
	err = proto.Unmarshal(buffer[:n], responseMsg)
	if err != nil {
		fmt.Printf("反序列化响应失败: %v\n", err)
		return
	}

	// 输出响应
	fmt.Printf("收到响应: %v\n", responseMsg)
}
