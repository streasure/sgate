package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

// MessageACK 消息确认管理
type MessageACK struct {
	// 序列号管理
	sequenceMap   sync.Map // key: connectionID, value: *SequenceManager
	sequenceMutex sync.RWMutex

	// 待确认消息
	pendingMessages sync.Map // key: sequenceID, value: *PendingMessage
	pendingMutex    sync.RWMutex

	// 重传配置
	maxRetries    int
	retryInterval time.Duration
	timeout       time.Duration

	// 停止通道
	stopChan chan struct{}
}

// SequenceManager 序列号管理器
type SequenceManager struct {
	connectionID string
	nextSequence int64
	mutex        sync.Mutex
}

// PendingMessage 待确认消息
type PendingMessage struct {
	connectionID string
	sequence     int64
	message      *protobuf.Message
	sentAt       time.Time
	retryCount   int
	callback     func(*protobuf.Acknowledgement)
}

// NewMessageACK 创建消息确认管理器
func NewMessageACK(maxRetries int, retryInterval, timeout time.Duration) *MessageACK {
	ack := &MessageACK{
		maxRetries:    maxRetries,
		retryInterval: retryInterval,
		timeout:       timeout,
		stopChan:      make(chan struct{}),
	}

	// 启动超时检查协程
	go ack.checkTimeouts()

	return ack
}

// Start 启动消息确认管理器
func (ack *MessageACK) Start() {
	// 已经在 NewMessageACK 中启动了超时检查协程
}

// Stop 停止消息确认管理器
func (ack *MessageACK) Stop() {
	close(ack.stopChan)
}

// GetSequence 获取下一个序列号
func (ack *MessageACK) GetSequence(connectionID string) int64 {
	ack.sequenceMutex.RLock()
	seqManager, ok := ack.sequenceMap.Load(connectionID)
	ack.sequenceMutex.RUnlock()

	if !ok {
		// 创建新的序列号管理器
		newSeqManager := &SequenceManager{
			connectionID: connectionID,
			nextSequence: 1,
		}

		ack.sequenceMutex.Lock()
		// 再次检查，避免并发问题
		if seqManager, ok = ack.sequenceMap.Load(connectionID); !ok {
			ack.sequenceMap.Store(connectionID, newSeqManager)
			seqManager = newSeqManager
		}
		ack.sequenceMutex.Unlock()
	}

	// 获取并递增序列号
	sm := seqManager.(*SequenceManager)
	sm.mutex.Lock()
	sequence := sm.nextSequence
	sm.nextSequence++
	sm.mutex.Unlock()

	return sequence
}

// TrackMessage 跟踪待确认的消息
func (ack *MessageACK) TrackMessage(connectionID string, sequence int64, message *protobuf.Message, callback func(*protobuf.Acknowledgement)) {
	pendingMsg := &PendingMessage{
		connectionID: connectionID,
		sequence:     sequence,
		message:      message,
		sentAt:       time.Now(),
		retryCount:   0,
		callback:     callback,
	}

	// 存储待确认消息
	sequenceID := connectionID + "-" + fmt.Sprint(sequence)
	ack.pendingMutex.Lock()
	ack.pendingMessages.Store(sequenceID, pendingMsg)
	ack.pendingMutex.Unlock()

	// 启动重传定时器
	go ack.scheduleRetry(sequenceID)
}

// ProcessAcknowledgement 处理确认消息
func (ack *MessageACK) ProcessAcknowledgement(connectionID string, sequence int64, status string, errorMsg string) {
	sequenceID := connectionID + "-" + fmt.Sprint(sequence)

	// 查找待确认消息
	ack.pendingMutex.Lock()
	pendingMsg, ok := ack.pendingMessages.Load(sequenceID)
	if ok {
		ack.pendingMessages.Delete(sequenceID)
	}
	ack.pendingMutex.Unlock()

	if !ok {
		tlog.Warn("收到未知的ACK消息", "connectionID", connectionID, "sequence", sequence)
		return
	}

	// 调用回调函数
	pm := pendingMsg.(*PendingMessage)
	if pm.callback != nil {
		ack := &protobuf.Acknowledgement{
			Sequence:   sequence,
			Status:     status,
			Error:      errorMsg,
			ReceivedAt: time.Now().UnixMilli(),
		}
		pm.callback(ack)
	}

	tlog.Debug("消息确认处理完成", "connectionID", connectionID, "sequence", sequence, "status", status)
}

// scheduleRetry 安排消息重传
func (ack *MessageACK) scheduleRetry(sequenceID string) {
	timer := time.NewTimer(ack.retryInterval)
	defer timer.Stop()

	select {
	case <-timer.C:
		ack.retryMessage(sequenceID)
	case <-ack.stopChan:
		return
	}
}

// retryMessage 重传消息
func (ack *MessageACK) retryMessage(sequenceID string) {
	// 查找待确认消息
	ack.pendingMutex.RLock()
	pendingMsg, ok := ack.pendingMessages.Load(sequenceID)
	ack.pendingMutex.RUnlock()

	if !ok {
		// 消息已经被确认，不需要重传
		return
	}

	pm := pendingMsg.(*PendingMessage)

	// 检查是否超过最大重试次数
	if pm.retryCount >= ack.maxRetries {
		// 超过最大重试次数，删除消息并调用回调
		ack.pendingMutex.Lock()
		ack.pendingMessages.Delete(sequenceID)
		ack.pendingMutex.Unlock()

		if pm.callback != nil {
			ack := &protobuf.Acknowledgement{
				Sequence:   pm.sequence,
				Status:     "timeout",
				Error:      "message timeout after max retries",
				ReceivedAt: time.Now().UnixMilli(),
			}
			pm.callback(ack)
		}

		tlog.Warn("消息超时", "connectionID", pm.connectionID, "sequence", pm.sequence, "retryCount", pm.retryCount)
		return
	}

	// 检查是否超时
	if time.Since(pm.sentAt) > ack.timeout {
		// 超时，删除消息并调用回调
		ack.pendingMutex.Lock()
		ack.pendingMessages.Delete(sequenceID)
		ack.pendingMutex.Unlock()

		if pm.callback != nil {
			ack := &protobuf.Acknowledgement{
				Sequence:   pm.sequence,
				Status:     "timeout",
				Error:      "message timeout",
				ReceivedAt: time.Now().UnixMilli(),
			}
			pm.callback(ack)
		}

		tlog.Warn("消息超时", "connectionID", pm.connectionID, "sequence", pm.sequence, "sentAt", pm.sentAt)
		return
	}

	// 增加重试计数
	pm.retryCount++

	// 重传消息
	// 这里需要获取连接并发送消息
	// 暂时使用日志记录，实际实现需要调用 connectionManager.SendToConnection
	tlog.Debug("重传消息", "connectionID", pm.connectionID, "sequence", pm.sequence, "retryCount", pm.retryCount)

	// 安排下一次重传
	go ack.scheduleRetry(sequenceID)
}

// checkTimeouts 检查超时消息
func (ack *MessageACK) checkTimeouts() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ack.checkPendingMessages()
		case <-ack.stopChan:
			return
		}
	}
}

// checkPendingMessages 检查待确认消息
func (ack *MessageACK) checkPendingMessages() {
	now := time.Now()

	ack.pendingMutex.RLock()
	var timedoutMessages []string

	ack.pendingMessages.Range(func(key, value interface{}) bool {
		sequenceID := key.(string)
		pendingMsg := value.(*PendingMessage)

		// 检查是否超时
		if now.Sub(pendingMsg.sentAt) > ack.timeout {
			timedoutMessages = append(timedoutMessages, sequenceID)
		}

		return true
	})
	ack.pendingMutex.RUnlock()

	// 处理超时消息
	for _, sequenceID := range timedoutMessages {
		ack.retryMessage(sequenceID)
	}
}

// GetPendingMessageCount 获取待确认消息数量
func (ack *MessageACK) GetPendingMessageCount() int {
	count := 0
	ack.pendingMessages.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// CleanupConnection 清理连接相关的资源
func (ack *MessageACK) CleanupConnection(connectionID string) {
	// 删除序列号管理器
	ack.sequenceMutex.Lock()
	ack.sequenceMap.Delete(connectionID)
	ack.sequenceMutex.Unlock()

	// 删除该连接的所有待确认消息
	var toDelete []string
	ack.pendingMessages.Range(func(key, value interface{}) bool {
		sequenceID := key.(string)
		if len(sequenceID) > len(connectionID) && sequenceID[:len(connectionID)] == connectionID {
			toDelete = append(toDelete, sequenceID)
		}
		return true
	})

	for _, sequenceID := range toDelete {
		ack.pendingMessages.Delete(sequenceID)
	}

	tlog.Debug("清理连接资源", "connectionID", connectionID, "deletedMessages", len(toDelete))
}
