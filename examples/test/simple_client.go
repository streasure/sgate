package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

func main() {
	fmt.Println("开始测试...")

	// 连接到服务器
	fmt.Println("连接到服务器...")
	conn, err := net.Dial("tcp", "localhost:8083")
	if err != nil {
		fmt.Printf("连接服务器失败: %v\n", err)
		return
	}
	defer conn.Close()
	fmt.Println("成功连接到服务器")

	// 发送ping请求
	request := map[string]interface{}{
		"route": "ping",
		"payload": map[string]interface{}{
			"timestamp": time.Now().UnixMilli(),
		},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err := json.Marshal(request)
	if err != nil {
		fmt.Printf("序列化请求失败: %v\n", err)
		return
	}
	fmt.Printf("发送请求: %s\n", string(data))

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		fmt.Printf("发送请求失败: %v\n", err)
		return
	}
	fmt.Println("请求已发送")

	// 设置读取超时
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 读取响应
	fmt.Println("等待响应...")
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		fmt.Printf("读取响应失败: %v\n", err)
		return
	}
	fmt.Printf("收到响应: %s\n", string(buffer[:n]))

	// 解析响应
	var response map[string]interface{}
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		fmt.Printf("解析响应失败: %v, 响应: %s\n", err, string(buffer[:n]))
		return
	}
	fmt.Printf("解析响应成功: %v\n", response)

	fmt.Println("测试完成")
}
