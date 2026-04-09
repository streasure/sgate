package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

func main() {
	// 配置日志
	tlog.SetLevel(tlog.Info)

	// 连接到服务器
	conn, err := net.Dial("tcp", "localhost:8083")
	if err != nil {
		tlog.Error("连接服务器失败", "error", err)
		return
	}
	defer conn.Close()

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
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	var response map[string]interface{}
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	// 发送getConnections请求
	request = map[string]interface{}{
		"route": "getConnections",
		"payload": map[string]interface{}{
			"token": "test_token",
		},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err = json.Marshal(request)
	if err != nil {
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	n, err = conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	// 发送broadcast请求
	request = map[string]interface{}{
		"route": "broadcast",
		"payload": map[string]interface{}{
			"token":   "test_token",
			"message": "Hello from test client",
		},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err = json.Marshal(request)
	if err != nil {
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	n, err = conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	// 发送health请求
	request = map[string]interface{}{
		"route": "health",
		"payload": map[string]interface{}{},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err = json.Marshal(request)
	if err != nil {
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	n, err = conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	// 发送version请求
	request = map[string]interface{}{
		"route": "version",
		"payload": map[string]interface{}{},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err = json.Marshal(request)
	if err != nil {
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	n, err = conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	// 发送api-docs请求
	request = map[string]interface{}{
		"route": "api-docs",
		"payload": map[string]interface{}{},
		"user_uuid": "test_user_123",
	}

	// 序列化请求
	data, err = json.Marshal(request)
	if err != nil {
		tlog.Error("序列化请求失败", "error", err)
		return
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		tlog.Error("发送请求失败", "error", err)
		return
	}

	// 读取响应
	buffer = make([]byte, 4096) // 增加缓冲区大小
	n, err = conn.Read(buffer)
	if err != nil {
		tlog.Error("读取响应失败", "error", err)
		return
	}

	// 解析响应
	if err := json.Unmarshal(buffer[:n], &response); err != nil {
		tlog.Error("解析响应失败", "error", err, "response", string(buffer[:n]))
		return
	}

	// 输出响应
	tlog.Info("收到响应", "response", response)

	fmt.Println("测试完成")
}
