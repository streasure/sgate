package gateway

import (
	"sync"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

// MessageQueue 消息队列
// 功能: 管理消息队列，支持消息重试
// 字段:
//
//	messages: 消息映射
//	mutex: 互斥锁
//	retryInterval: 重试间隔
//	maxRetries: 最大重试次数
//	cleanupInterval: 清理间隔
type MessageQueue struct {
	messages        map[string]*QueuedMessage // 消息映射
	mutex           sync.RWMutex              // 互斥锁
	retryInterval   time.Duration             // 重试间隔
	maxRetries      int                       // 最大重试次数
	cleanupInterval time.Duration             // 清理间隔
}

// QueuedMessage 队列消息
// 字段:
//
//	message: 消息
//	createdAt: 创建时间
//	retryCount: 重试次数
//	lastRetry: 最后重试时间
//	status: 状态（pending, processing, completed, failed）
type QueuedMessage struct {
	message    *protobuf.Message // 消息
	createdAt  time.Time         // 创建时间
	retryCount int               // 重试次数
	lastRetry  time.Time         // 最后重试时间
	status     string            // 状态
	messageID  string            // 消息ID
}

// NewMessageQueue 创建消息队列
// 参数:
//
//	retryInterval: 重试间隔
//	maxRetries: 最大重试次数
//
// 返回值:
//
//	*MessageQueue: 消息队列实例
func NewMessageQueue(retryInterval time.Duration, maxRetries int) *MessageQueue {
	if retryInterval == 0 {
		retryInterval = 5 * time.Second // 默认重试间隔
	}
	if maxRetries == 0 {
		maxRetries = 3 // 默认最大重试次数
	}

	mq := &MessageQueue{
		messages:        make(map[string]*QueuedMessage),
		retryInterval:   retryInterval,
		maxRetries:      maxRetries,
		cleanupInterval: 5 * time.Minute, // 默认清理间隔
	}

	// 启动清理任务
	go mq.cleanup()

	// 启动重试任务
	go mq.retryProcessor()

	return mq
}

// Enqueue 入队消息
// 参数:
//
//	message: 消息
//	messageID: 消息ID
//
// 返回值:
//
//	error: 错误信息
func (mq *MessageQueue) Enqueue(message *protobuf.Message, messageID string) error {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	queuedMsg := &QueuedMessage{
		message:    message,
		createdAt:  time.Now(),
		retryCount: 0,
		lastRetry:  time.Now(),
		status:     "pending",
		messageID:  messageID,
	}

	mq.messages[messageID] = queuedMsg

	tlog.Debug("消息入队成功", "messageID", messageID, "route", message.Route)
	return nil
}

// Dequeue 出队消息
// 参数:
//
//	messageID: 消息ID
//
// 返回值:
//
//	*QueuedMessage: 队列消息
//	error: 错误信息
func (mq *MessageQueue) Dequeue(messageID string) (*QueuedMessage, error) {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	message, exists := mq.messages[messageID]
	if !exists {
		return nil, nil
	}

	// 更新状态为处理中
	message.status = "processing"
	message.lastRetry = time.Now()

	return message, nil
}

// CompleteMessage 完成消息
// 参数:
//
//	messageID: 消息ID
//
// 返回值:
//
//	error: 错误信息
func (mq *MessageQueue) CompleteMessage(messageID string) error {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	message, exists := mq.messages[messageID]
	if !exists {
		return nil
	}

	// 更新状态为完成
	message.status = "completed"

	// 从内存中移除
	delete(mq.messages, messageID)

	tlog.Debug("消息处理完成", "messageID", messageID)
	return nil
}

// FailMessage 失败消息
// 参数:
//
//	messageID: 消息ID
//	errorMsg: 错误信息
//
// 返回值:
//
//	error: 错误信息
func (mq *MessageQueue) FailMessage(messageID string, errorMsg string) error {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	message, exists := mq.messages[messageID]
	if !exists {
		return nil
	}

	// 更新状态为失败
	message.status = "failed"
	message.retryCount++

	// 如果重试次数超过最大值，从内存中移除
	if message.retryCount >= mq.maxRetries {
		delete(mq.messages, messageID)
		tlog.Warn("消息处理失败，达到最大重试次数", "messageID", messageID, "retryCount", message.retryCount)
	} else {
		tlog.Warn("消息处理失败，将重试", "messageID", messageID, "retryCount", message.retryCount, "error", errorMsg)
	}

	return nil
}

// GetMessage 获取消息
// 参数:
//
//	messageID: 消息ID
//
// 返回值:
//
//	*QueuedMessage: 队列消息
//	bool: 是否存在
func (mq *MessageQueue) GetMessage(messageID string) (*QueuedMessage, bool) {
	mq.mutex.RLock()
	defer mq.mutex.RUnlock()

	message, exists := mq.messages[messageID]
	return message, exists
}

// GetPendingMessages 获取待处理消息
// 返回值:
//
//	[]*QueuedMessage: 待处理消息列表
func (mq *MessageQueue) GetPendingMessages() []*QueuedMessage {
	mq.mutex.RLock()
	defer mq.mutex.RUnlock()

	var pendingMessages []*QueuedMessage
	for _, message := range mq.messages {
		if message.status == "pending" {
			pendingMessages = append(pendingMessages, message)
		}
	}

	return pendingMessages
}

// GetFailedMessages 获取失败消息
// 返回值:
//
//	[]*QueuedMessage: 失败消息列表
func (mq *MessageQueue) GetFailedMessages() []*QueuedMessage {
	mq.mutex.RLock()
	defer mq.mutex.RUnlock()

	var failedMessages []*QueuedMessage
	for _, message := range mq.messages {
		if message.status == "failed" {
			failedMessages = append(failedMessages, message)
		}
	}

	return failedMessages
}

// retryProcessor 重试处理器
func (mq *MessageQueue) retryProcessor() {
	ticker := time.NewTicker(mq.retryInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		mq.processRetries()
	}
}

// processRetries 处理重试
func (mq *MessageQueue) processRetries() {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	now := time.Now()
	for messageID, message := range mq.messages {
		if (message.status == "failed" || message.status == "pending") &&
			now.Sub(message.lastRetry) >= mq.retryInterval &&
			message.retryCount < mq.maxRetries {

			// 重置状态为待处理
			message.status = "pending"
			message.lastRetry = now

			tlog.Info("消息重新入队", "messageID", messageID, "retryCount", message.retryCount)
		}
	}
}

// cleanup 清理过期消息
func (mq *MessageQueue) cleanup() {
	ticker := time.NewTicker(mq.cleanupInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		mq.cleanupExpiredMessages()
	}
}

// cleanupExpiredMessages 清理过期消息
func (mq *MessageQueue) cleanupExpiredMessages() {
	mq.mutex.Lock()
	defer mq.mutex.Unlock()

	now := time.Now()
	for messageID, message := range mq.messages {
		// 清理24小时前的已完成消息
		if message.status == "completed" && now.Sub(message.createdAt) > 24*time.Hour {
			delete(mq.messages, messageID)
			tlog.Debug("清理过期消息", "messageID", messageID)
		}
		// 清理达到最大重试次数的失败消息
		if message.status == "failed" && message.retryCount >= mq.maxRetries {
			delete(mq.messages, messageID)
			tlog.Debug("清理失败消息", "messageID", messageID)
		}
	}
}

// GetStats 获取队列统计信息
// 返回值:
//
//	map[string]int: 统计信息
func (mq *MessageQueue) GetStats() map[string]int {
	mq.mutex.RLock()
	defer mq.mutex.RUnlock()

	stats := map[string]int{
		"total":      len(mq.messages),
		"pending":    0,
		"processing": 0,
		"completed":  0,
		"failed":     0,
	}

	for _, message := range mq.messages {
		switch message.status {
		case "pending":
			stats["pending"]++
		case "processing":
			stats["processing"]++
		case "completed":
			stats["completed"]++
		case "failed":
			stats["failed"]++
		}
	}

	return stats
}
