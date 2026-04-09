package gateway

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

// MessageIntegrity 消息完整性管理
type MessageIntegrity struct {
	timeWindow  int64           // 时间窗口（毫秒）
	replayCache map[string]bool // 防重放缓存
	cacheMutex  sync.RWMutex    // 缓存互斥锁
}

// NewMessageIntegrity 创建消息完整性管理器
func NewMessageIntegrity(timeWindow int64) *MessageIntegrity {
	mi := &MessageIntegrity{
		timeWindow:  timeWindow,
		replayCache: make(map[string]bool),
	}

	// 启动缓存清理协程
	go mi.cleanupCache()

	return mi
}

// cleanupCache 清理过期的防重放缓存
func (mi *MessageIntegrity) cleanupCache() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		mi.cacheMutex.Lock()
		now := time.Now().UnixMilli()
		for key, _ := range mi.replayCache {
			// 简单的时间戳提取（假设key包含时间戳）
			// 实际实现中可能需要更复杂的缓存键管理
			if now-mi.timeWindow > 0 {
				delete(mi.replayCache, key)
			}
		}
		mi.cacheMutex.Unlock()
	}
}

// GenerateChecksum 生成消息校验和
func (mi *MessageIntegrity) GenerateChecksum(msg proto.Message) string {
	// 序列化消息
	data, err := proto.Marshal(msg)
	if err != nil {
		tlog.Error("生成校验和失败", "error", err)
		return ""
	}

	// 计算MD5校验和
	hash := md5.Sum(data)
	return hex.EncodeToString(hash[:])
}

// ValidateChecksum 验证消息校验和
func (mi *MessageIntegrity) ValidateChecksum(msg proto.Message, expectedChecksum string) bool {
	// 生成校验和
	actualChecksum := mi.GenerateChecksum(msg)
	if actualChecksum == "" {
		return false
	}

	// 比较校验和
	return actualChecksum == expectedChecksum
}

// ValidateTimestamp 验证时间戳（防重放）
func (mi *MessageIntegrity) ValidateTimestamp(timestamp int64) bool {
	now := time.Now().UnixMilli()

	// 检查时间戳是否在有效窗口内
	if timestamp < now-mi.timeWindow || timestamp > now+mi.timeWindow {
		return false
	}

	return true
}

// CheckReplay 检查是否为重放消息
func (mi *MessageIntegrity) CheckReplay(msgID string) bool {
	mi.cacheMutex.RLock()
	defer mi.cacheMutex.RUnlock()

	// 检查消息是否已处理过
	if _, exists := mi.replayCache[msgID]; exists {
		return true
	}

	return false
}

// MarkProcessed 标记消息已处理
func (mi *MessageIntegrity) MarkProcessed(msgID string) {
	mi.cacheMutex.Lock()
	defer mi.cacheMutex.Unlock()

	mi.replayCache[msgID] = true
}

// ProcessMessage 处理消息完整性
func (mi *MessageIntegrity) ProcessMessage(msg *protobuf.Message) error {
	// 生成消息ID（用于防重放）
	msgID := msg.ConnectionId + "-" + msg.UserUuid + "-" + msg.Route + "-" + fmt.Sprint(msg.Timestamp)

	// 检查是否重放
	if mi.CheckReplay(msgID) {
		return fmt.Errorf("replay attack detected")
	}

	// 验证时间戳
	if !mi.ValidateTimestamp(msg.Timestamp) {
		return fmt.Errorf("invalid timestamp")
	}

	// 验证校验和
	if !mi.ValidateChecksum(msg, msg.Checksum) {
		return fmt.Errorf("invalid checksum")
	}

	// 标记消息已处理
	mi.MarkProcessed(msgID)

	return nil
}

// PrepareMessage 准备消息（添加时间戳和校验和）
func (mi *MessageIntegrity) PrepareMessage(msg *protobuf.Message) {
	// 设置时间戳
	msg.Timestamp = time.Now().UnixMilli()

	// 清空校验和（避免影响计算）
	msg.Checksum = ""

	// 生成并设置校验和
	msg.Checksum = mi.GenerateChecksum(msg)

	// 设置协议版本
	if msg.ProtocolVersion == "" {
		msg.ProtocolVersion = "1.0.0"
	}
}

// ProcessErrorResponse 处理错误响应的完整性
func (mi *MessageIntegrity) ProcessErrorResponse(resp *protobuf.ErrorResponse) error {
	// 生成响应ID
	respID := fmt.Sprint(resp.Timestamp)

	// 检查是否重放
	if mi.CheckReplay(respID) {
		return fmt.Errorf("replay attack detected")
	}

	// 验证时间戳
	if !mi.ValidateTimestamp(resp.Timestamp) {
		return fmt.Errorf("invalid timestamp")
	}

	// 验证校验和
	if !mi.ValidateChecksum(resp, resp.Checksum) {
		return fmt.Errorf("invalid checksum")
	}

	// 标记响应已处理
	mi.MarkProcessed(respID)

	return nil
}

// PrepareErrorResponse 准备错误响应
func (mi *MessageIntegrity) PrepareErrorResponse(resp *protobuf.ErrorResponse) {
	// 设置时间戳
	resp.Timestamp = time.Now().UnixMilli()

	// 清空校验和
	resp.Checksum = ""

	// 生成并设置校验和
	resp.Checksum = mi.GenerateChecksum(resp)

	// 设置协议版本
	if resp.ProtocolVersion == "" {
		resp.ProtocolVersion = "1.0.0"
	}
}

// ProcessAcknowledgement 处理确认消息的完整性
func (mi *MessageIntegrity) ProcessAcknowledgement(ack *protobuf.Acknowledgement) error {
	// 生成确认ID
	ackID := fmt.Sprint(ack.Sequence) + "-" + fmt.Sprint(ack.Timestamp)

	// 检查是否重放
	if mi.CheckReplay(ackID) {
		return fmt.Errorf("replay attack detected")
	}

	// 验证时间戳
	if !mi.ValidateTimestamp(ack.Timestamp) {
		return fmt.Errorf("invalid timestamp")
	}

	// 验证校验和
	if !mi.ValidateChecksum(ack, ack.Checksum) {
		return fmt.Errorf("invalid checksum")
	}

	// 标记确认已处理
	mi.MarkProcessed(ackID)

	return nil
}

// PrepareAcknowledgement 准备确认消息
func (mi *MessageIntegrity) PrepareAcknowledgement(ack *protobuf.Acknowledgement) {
	// 设置时间戳
	ack.Timestamp = time.Now().UnixMilli()

	// 清空校验和
	ack.Checksum = ""

	// 生成并设置校验和
	ack.Checksum = mi.GenerateChecksum(ack)

	// 设置协议版本
	if ack.ProtocolVersion == "" {
		ack.ProtocolVersion = "1.0.0"
	}
}

// ProcessHandshake 处理握手消息的完整性
func (mi *MessageIntegrity) ProcessHandshake(handshake *protobuf.Handshake) error {
	// 生成握手ID
	handshakeID := handshake.DeviceId + "-" + fmt.Sprint(handshake.Timestamp)

	// 检查是否重放
	if mi.CheckReplay(handshakeID) {
		return fmt.Errorf("replay attack detected")
	}

	// 验证时间戳
	if !mi.ValidateTimestamp(handshake.Timestamp) {
		return fmt.Errorf("invalid timestamp")
	}

	// 验证校验和
	if !mi.ValidateChecksum(handshake, handshake.Checksum) {
		return fmt.Errorf("invalid checksum")
	}

	// 标记握手已处理
	mi.MarkProcessed(handshakeID)

	return nil
}

// PrepareHandshake 准备握手消息
func (mi *MessageIntegrity) PrepareHandshake(handshake *protobuf.Handshake) {
	// 设置时间戳
	handshake.Timestamp = time.Now().UnixMilli()

	// 清空校验和
	handshake.Checksum = ""

	// 生成并设置校验和
	handshake.Checksum = mi.GenerateChecksum(handshake)
}
