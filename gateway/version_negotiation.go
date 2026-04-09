package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/gateway/protobuf"
	tlog "github.com/streasure/treasure-slog"
)

// VersionNegotiation 版本协商管理器
// 功能: 管理协议版本协商，处理握手消息，维护客户端版本映射
// 字段:
//   supportedVersions: 支持的协议版本列表
//   clientVersions: 客户端版本映射
//   mutex: 互斥锁
//   handshakeTimeout: 握手超时时间
type VersionNegotiation struct {
	supportedVersions []string           // 支持的协议版本列表
	clientVersions    map[string]string  // 客户端版本映射 (connectionID -> version)
	mutex             sync.RWMutex       // 互斥锁
	handshakeTimeout  time.Duration      // 握手超时时间
}

// NewVersionNegotiation 创建版本协商管理器
// 参数:
//   supportedVersions: 支持的协议版本列表
//   handshakeTimeout: 握手超时时间
// 返回值:
//   *VersionNegotiation: 版本协商管理器实例
func NewVersionNegotiation(supportedVersions []string, handshakeTimeout time.Duration) *VersionNegotiation {
	if len(supportedVersions) == 0 {
		supportedVersions = []string{"1.0.0"} // 默认支持 1.0.0 版本
	}

	if handshakeTimeout == 0 {
		handshakeTimeout = 10 * time.Second // 默认 10 秒超时
	}

	return &VersionNegotiation{
		supportedVersions: supportedVersions,
		clientVersions:    make(map[string]string),
		handshakeTimeout:  handshakeTimeout,
	}
}

// ProcessHandshake 处理握手消息
// 参数:
//   connectionID: 连接ID
//   handshake: 握手消息
// 返回值:
//   string: 协商后的协议版本
//   error: 错误信息
func (vn *VersionNegotiation) ProcessHandshake(connectionID string, handshake *protobuf.Handshake) (string, error) {
	// 验证握手消息
	if handshake == nil {
		return "", fmt.Errorf("handshake message is nil")
	}

	if handshake.Timestamp == 0 {
		return "", fmt.Errorf("missing timestamp in handshake")
	}

	// 检查时间戳是否过期
	if time.Since(time.Unix(handshake.Timestamp/1000, 0)) > vn.handshakeTimeout {
		return "", fmt.Errorf("handshake timestamp expired")
	}

	// 协商协议版本
	negotiatedVersion := vn.negotiateVersion(handshake.SupportedVersions, handshake.ProtocolVersion)
	if negotiatedVersion == "" {
		return "", fmt.Errorf("no compatible protocol version found")
	}

	// 存储客户端版本
	vn.mutex.Lock()
	vn.clientVersions[connectionID] = negotiatedVersion
	vn.mutex.Unlock()

	tlog.Info("Handshake successful", 
		"connectionID", connectionID,
		"negotiatedVersion", negotiatedVersion,
		"clientProtocolVersion", handshake.ProtocolVersion,
		"clientSupportedVersions", handshake.SupportedVersions,
		"clientType", handshake.ClientType,
		"clientVersion", handshake.ClientVersion,
		"deviceID", handshake.DeviceId,
	)

	return negotiatedVersion, nil
}

// GetClientVersion 获取客户端协议版本
// 参数:
//   connectionID: 连接ID
// 返回值:
//   string: 客户端协议版本
//   bool: 是否存在
func (vn *VersionNegotiation) GetClientVersion(connectionID string) (string, bool) {
	vn.mutex.RLock()
	defer vn.mutex.RUnlock()

	version, exists := vn.clientVersions[connectionID]
	return version, exists
}

// RemoveClientVersion 移除客户端版本映射
// 参数:
//   connectionID: 连接ID
func (vn *VersionNegotiation) RemoveClientVersion(connectionID string) {
	vn.mutex.Lock()
	defer vn.mutex.Unlock()

	delete(vn.clientVersions, connectionID)
}

// negotiateVersion 协商协议版本
// 参数:
//   clientVersions: 客户端支持的版本列表
//   clientPreferredVersion: 客户端首选版本
// 返回值:
//   string: 协商后的版本
func (vn *VersionNegotiation) negotiateVersion(clientVersions []string, clientPreferredVersion string) string {
	// 首先检查客户端首选版本是否被支持
	if vn.isVersionSupported(clientPreferredVersion) {
		return clientPreferredVersion
	}

	// 检查客户端支持的版本列表
	for _, clientVersion := range clientVersions {
		if vn.isVersionSupported(clientVersion) {
			return clientVersion
		}
	}

	// 返回最高版本的支持版本
	if len(vn.supportedVersions) > 0 {
		return vn.supportedVersions[0] // 假设第一个版本是最高版本
	}

	return ""
}

// isVersionSupported 检查版本是否被支持
// 参数:
//   version: 版本字符串
// 返回值:
//   bool: 是否支持
func (vn *VersionNegotiation) isVersionSupported(version string) bool {
	for _, supportedVersion := range vn.supportedVersions {
		if supportedVersion == version {
			return true
		}
	}
	return false
}

// GetSupportedVersions 获取支持的版本列表
// 返回值:
//   []string: 支持的版本列表
func (vn *VersionNegotiation) GetSupportedVersions() []string {
	return vn.supportedVersions
}

// SetSupportedVersions 设置支持的版本列表
// 参数:
//   versions: 版本列表
func (vn *VersionNegotiation) SetSupportedVersions(versions []string) {
	vn.mutex.Lock()
	defer vn.mutex.Unlock()

	vn.supportedVersions = versions
}

// GenerateHandshakeResponse 生成握手响应
// 参数:
//   negotiatedVersion: 协商后的版本
//   supportedVersions: 支持的版本列表
// 返回值:
//   *protobuf.Message: 握手响应消息
func (vn *VersionNegotiation) GenerateHandshakeResponse(negotiatedVersion string) *protobuf.Message {
	payload := make(map[string]string)
	payload["negotiated_version"] = negotiatedVersion
	payload["supported_versions"] = fmt.Sprintf("%v", vn.supportedVersions)

	return &protobuf.Message{
		Route:           "handshake_response",
		Payload:         payload,
		ProtocolVersion: negotiatedVersion,
		Timestamp:       time.Now().UnixMilli(),
	}
}
