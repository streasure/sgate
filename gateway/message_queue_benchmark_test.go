package gateway

import (
	"testing"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
)

// BenchmarkMessageQueueEnqueue 测试消息入队性能
func BenchmarkMessageQueueEnqueue(b *testing.B) {
	// 创建消息队列
	mq := NewMessageQueue(5*time.Second, 3)

	// 准备测试消息
	message := &protobuf.Message{
		ConnectionId: "test_connection",
		UserUuid:     "test_user",
		Route:        "test_route",
		Payload: map[string]string{
			"key": "value",
		},
		Sequence: 1,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.Enqueue(message, messageID)
	}
}

// BenchmarkMessageQueueDequeue 测试消息出队性能
func BenchmarkMessageQueueDequeue(b *testing.B) {
	// 创建消息队列
	mq := NewMessageQueue(5*time.Second, 3)

	// 准备测试消息
	message := &protobuf.Message{
		ConnectionId: "test_connection",
		UserUuid:     "test_user",
		Route:        "test_route",
		Payload: map[string]string{
			"key": "value",
		},
		Sequence: 1,
	}

	// 预先入队消息
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.Enqueue(message, messageID)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.Dequeue(messageID)
	}
}

// BenchmarkMessageQueueComplete 测试消息完成性能
func BenchmarkMessageQueueComplete(b *testing.B) {
	// 创建消息队列
	mq := NewMessageQueue(5*time.Second, 3)

	// 准备测试消息
	message := &protobuf.Message{
		ConnectionId: "test_connection",
		UserUuid:     "test_user",
		Route:        "test_route",
		Payload: map[string]string{
			"key": "value",
		},
		Sequence: 1,
	}

	// 预先入队消息
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.Enqueue(message, messageID)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.CompleteMessage(messageID)
	}
}

// BenchmarkMessageQueueFail 测试消息失败性能
func BenchmarkMessageQueueFail(b *testing.B) {
	// 创建消息队列
	mq := NewMessageQueue(5*time.Second, 3)

	// 准备测试消息
	message := &protobuf.Message{
		ConnectionId: "test_connection",
		UserUuid:     "test_user",
		Route:        "test_route",
		Payload: map[string]string{
			"key": "value",
		},
		Sequence: 1,
	}

	// 预先入队消息
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.Enqueue(message, messageID)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messageID := "test_message_" + string(rune(i))
		mq.FailMessage(messageID, "test error")
	}
}
