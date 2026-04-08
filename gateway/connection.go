package gateway

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/panjf2000/gnet/v2/pkg/pool/bytebuffer"
	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
	tlog "github.com/streasure/treasure-slog"
)

// ConnectionManager 连接管理器
// 功能: 管理所有网络连接，包括添加、移除、获取连接，以及广播消息
// 字段:
//   connections: 本地连接映射，使用 sync.Map 实现，key为连接ID，value为连接对象
//   userConnections: 用户连接映射，使用 sync.Map 实现，key为用户UUID，value为连接ID集合
//   groups: 推送组映射，使用 sync.Map 实现，key为组ID，value为用户UUID集合
//   count: 连接数量，使用原子操作进行更新，确保并发安全

type ConnectionManager struct {
	connections     sync.Map // 本地连接映射，使用 sync.Map 实现并发安全
	userConnections sync.Map // 用户连接映射，使用 sync.Map 实现，key为用户UUID，value为连接ID集合
	groups          sync.Map // 推送组映射，使用 sync.Map 实现，key为组ID，value为用户UUID集合
	count           int32    // 连接数量，使用原子操作进行更新
}

// connectionInfoPool 连接信息对象池
// 功能: 复用连接信息映射，减少内存分配
var connectionInfoPool = sync.Pool{
	New: func() interface{} {
		return map[string]interface{}{
			"connection_id": "",       // 连接ID
			"remote_addr":   "",       // 远程地址
			"local_addr":    "",       // 本地地址
			"created_at":    int64(0), // 创建时间
			"status":        "",       // 连接状态
		}
	},
}

// getConnectionInfo 从对象池获取连接信息
// 参数:
//
//	connectionID: 连接ID
//	remoteAddr: 远程地址
//	localAddr: 本地地址
//	status: 连接状态
//
// 返回值:
//
//	map[string]interface{}: 连接信息映射
func getConnectionInfo(connectionID, remoteAddr, localAddr string, status string) map[string]interface{} {
	info := connectionInfoPool.Get().(map[string]interface{})
	info["connection_id"] = connectionID
	info["remote_addr"] = remoteAddr
	info["local_addr"] = localAddr
	info["created_at"] = time.Now().UnixMilli()
	info["status"] = status
	return info
}

// putConnectionInfo 归还连接信息到对象池
// 参数:
//
//	info: 连接信息映射
func putConnectionInfo(info map[string]interface{}) {
	info["connection_id"] = ""
	info["remote_addr"] = ""
	info["local_addr"] = ""
	info["created_at"] = int64(0)
	info["status"] = ""
	connectionInfoPool.Put(info)
}

// broadcastDataPool 广播数据对象池
// 功能: 复用广播数据映射，减少内存分配
var broadcastDataPool = sync.Pool{
	New: func() interface{} {
		return map[string]interface{}{
			"message":   nil,      // 广播消息
			"timestamp": int64(0), // 时间戳
		}
	},
}

// getBroadcastData 从对象池获取广播数据
// 参数:
//
//	message: 广播消息
//
// 返回值:
//
//	map[string]interface{}: 广播数据映射
func getBroadcastData(message interface{}) map[string]interface{} {
	data := broadcastDataPool.Get().(map[string]interface{})
	data["message"] = message
	data["timestamp"] = time.Now().UnixMilli()
	return data
}

// putBroadcastData 归还广播数据到对象池
// 参数:
//
//	data: 广播数据映射
func putBroadcastData(data map[string]interface{}) {
	data["message"] = nil
	data["timestamp"] = int64(0)
	broadcastDataPool.Put(data)
}

// NewConnectionManager 创建连接管理器实例
// 返回值:
//
//	*ConnectionManager: 连接管理器实例
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{}
}

// AddConnection 添加连接
// 功能: 将新的网络连接添加到连接管理器中，并生成唯一的连接ID
// 参数:
//
//	conn: 网络连接
//	userUUID: 用户UUID
//
// 返回值:
//
//	string: 连接ID
func (cm *ConnectionManager) AddConnection(conn gnet.Conn, userUUID string) string {
	// 生成连接ID
	connectionID := generateConnectionID()

	// 存储连接信息到本地
	cm.connections.Store(connectionID, conn)
	atomic.AddInt32(&cm.count, 1)

	// 存储用户连接映射
	if userUUID != "" {
		// 获取用户的连接集合
		if connections, ok := cm.userConnections.Load(userUUID); ok {
			// 如果用户存在，添加新的连接ID
			connections.(map[string]bool)[connectionID] = true
		} else {
			// 如果用户不存在，创建新的连接集合
			cm.userConnections.Store(userUUID, map[string]bool{connectionID: true})
		}
	}

	// 输出调试日志
	tlog.Debug("连接已添加", "connectionID", connectionID, "userUUID", userUUID, "count", int(atomic.LoadInt32(&cm.count)))

	return connectionID
}

// RemoveConnection 移除连接
// 功能: 从连接管理器中移除指定的连接，并更新连接计数
// 参数:
//
//	connectionID: 连接ID
func (cm *ConnectionManager) RemoveConnection(connectionID string) {
	_, exists := cm.connections.LoadAndDelete(connectionID)
	if exists {
		atomic.AddInt32(&cm.count, -1)

		// 从用户连接映射中移除该连接ID
		cm.userConnections.Range(func(userUUID, connections interface{}) bool {
			connMap := connections.(map[string]bool)
			if connMap[connectionID] {
				delete(connMap, connectionID)
				// 如果用户没有连接了，从用户连接映射中移除该用户
				if len(connMap) == 0 {
					cm.userConnections.Delete(userUUID)
				}
			}
			return true
		})
	}

	// 输出调试日志
	if exists {
		tlog.Debug("连接已移除", "connectionID", connectionID, "count", int(atomic.LoadInt32(&cm.count)))
	}
}

// GetConnection 获取连接
// 功能: 根据连接ID获取对应的网络连接
// 参数:
//
//	connectionID: 连接ID
//
// 返回值:
//
//	gnet.Conn: 网络连接，如果连接不存在则返回nil
func (cm *ConnectionManager) GetConnection(connectionID string) gnet.Conn {
	if conn, ok := cm.connections.Load(connectionID); ok {
		return conn.(gnet.Conn)
	}
	return nil
}

// SendToConnection 发送消息到指定连接
// 功能: 向指定的连接发送消息，并处理连接关闭的情况
// 参数:
//
// connectionID: 连接ID
// message: 消息内容
//
// 返回值:
//
// bool: 是否发送成功
func (cm *ConnectionManager) SendToConnection(connectionID string, message interface{}) bool {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		// 输出警告日志
		tlog.Warn("连接不存在", "connectionID", connectionID)
		return false
	}

	// 检查连接是否已关闭
	// 尝试写入一个空字节，检查是否成功
	if _, err := conn.Write([]byte{}); err != nil {
		// 连接已关闭，从连接管理器中移除
		tlog.Warn("连接已关闭，从连接管理器中移除", "connectionID", connectionID)
		cm.RemoveConnection(connectionID)
		return false
	}

	// 序列化消息
	buf := bytebuffer.Get()
	defer bytebuffer.Put(buf)

	var responseData []byte
	var err error

	if protoMsg, ok := message.(*protobuf.Message); ok {
		// 使用Protocol Buffers序列化
		responseData, err = proto.Marshal(protoMsg)
	} else if errorMsg, ok := message.(*protobuf.ErrorResponse); ok {
		// 使用Protocol Buffers序列化错误消息
		responseData, err = proto.Marshal(errorMsg)
	} else {
		// 转换为Protocol Buffers消息
		if msgMap, ok := message.(map[string]string); ok {
			protoMsg := &protobuf.Message{
				Route:    "message",
				Payload:  msgMap,
			}
			responseData, err = proto.Marshal(protoMsg)
		} else {
			// 不支持的消息类型
			tlog.Error("不支持的消息类型", "type", message)
			return false
		}
	}

	if err != nil {
		// 输出错误日志
		tlog.Error("序列化消息失败", "error", err)
		return false
	}

	buf.Write(responseData)

	if _, err := conn.Write(buf.B); err != nil {
		// 输出错误日志
		tlog.Error("发送消息失败", "connectionID", connectionID, "error", err)
		// 连接可能已关闭，从连接管理器中移除
		cm.RemoveConnection(connectionID)
		return false
	}

	return true
}

// Broadcast 广播消息
// 功能: 向所有连接广播消息，使用并发方式提高性能
// 参数:
//
// message: 消息内容
func (cm *ConnectionManager) Broadcast(message interface{}) {
	// 先序列化消息，减少锁的持有时间
	var buf *bytebuffer.ByteBuffer
	
	// 处理不同类型的消息
	if protoMsg, ok := message.(*protobuf.Message); ok {
		// 使用Protocol Buffers序列化
		buf = bytebuffer.Get()
		responseData, marshalErr := proto.Marshal(protoMsg)
		if marshalErr != nil {
			tlog.Error("序列化广播消息失败", "error", marshalErr)
			bytebuffer.Put(buf)
			return
		}
		buf.Write(responseData)
	} else if errorMsg, ok := message.(*protobuf.ErrorResponse); ok {
		// 使用Protocol Buffers序列化错误消息
		buf = bytebuffer.Get()
		responseData, marshalErr := proto.Marshal(errorMsg)
		if marshalErr != nil {
			tlog.Error("序列化广播消息失败", "error", marshalErr)
			bytebuffer.Put(buf)
			return
		}
		buf.Write(responseData)
	} else {
		// 转换为Protocol Buffers消息
		buf = bytebuffer.Get()
		var responseData []byte
		var marshalErr error
		
		if msgMap, ok := message.(map[string]string); ok {
			// 如果是map[string]string，直接创建Protocol Buffers消息
			protoMsg := &protobuf.Message{
				Route:    "broadcast",
				Payload:  msgMap,
			}
			responseData, marshalErr = proto.Marshal(protoMsg)
		} else if msgStr, ok := message.(string); ok {
			// 如果是字符串，包装成Protocol Buffers消息
			protoMsg := &protobuf.Message{
				Route: "broadcast",
				Payload: map[string]string{
					"data": msgStr,
				},
			}
			responseData, marshalErr = proto.Marshal(protoMsg)
		} else {
			// 不支持的消息类型
			tlog.Error("不支持的消息类型", "type", message)
			bytebuffer.Put(buf)
			return
		}
		
		if marshalErr != nil {
			// 输出错误日志
			tlog.Error("序列化广播消息失败", "error", marshalErr)
			bytebuffer.Put(buf)
			return
		}
		buf.Write(responseData)
	}

	// 定义连接信息结构
	type connInfo struct {
		id   string    // 连接ID
		conn gnet.Conn // 网络连接
	}

	// 预先分配切片，减少内存分配
	var connections []connInfo
	cm.connections.Range(func(key, value interface{}) bool {
		connections = append(connections, connInfo{
			id:   key.(string),
			conn: value.(gnet.Conn),
		})
		return true
	})

	// 使用并发发送消息
	successCount := int32(0) // 成功发送计数
	errorCount := int32(0)   // 失败发送计数
	var wg sync.WaitGroup

	// 动态调整并发数，根据连接数自动调整
	concurrencyLimit := 200
	if len(connections) < 1000 {
		concurrencyLimit = 50
	} else if len(connections) > 10000 {
		concurrencyLimit = 500
	}
	
	semaphore := make(chan struct{}, concurrencyLimit)

	for _, info := range connections {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(id string, c gnet.Conn) {
			defer func() {
				wg.Done()
				<-semaphore // 释放信号量
			}()

			if _, err := c.Write(buf.B); err != nil {
				// 输出错误日志
				tlog.Error("广播消息失败", "connectionID", id, "error", err)
				atomic.AddInt32(&errorCount, 1)
			} else {
				atomic.AddInt32(&successCount, 1)
			}
		}(info.id, info.conn)
	}

	// 等待所有发送完成
	wg.Wait()

	bytebuffer.Put(buf)
	// 输出调试日志
	tlog.Debug("广播完成", "success", atomic.LoadInt32(&successCount), "error", atomic.LoadInt32(&errorCount))
}

// GetConnectionCount 获取连接数
// 功能: 获取当前连接管理器中的连接数量
// 返回值:
//
//	int: 连接数
func (cm *ConnectionManager) GetConnectionCount() int {
	return int(atomic.LoadInt32(&cm.count))
}

// CloseAllConnections 关闭所有连接
// 功能: 关闭所有连接并清空连接管理器
func (cm *ConnectionManager) CloseAllConnections() {
	// 遍历所有连接并关闭
	cm.connections.Range(func(key, value interface{}) bool {
		connectionID := key.(string)
		conn := value.(gnet.Conn)
		if err := conn.Close(); err != nil {
			tlog.Error("关闭连接失败", "connectionID", connectionID, "error", err)
		}
		cm.connections.Delete(key)
		return true
	})

	// 重置连接数
	atomic.StoreInt32(&cm.count, 0)

	tlog.Info("所有连接已关闭")
}

// generateConnectionID 生成连接ID
// 功能: 生成唯一的连接ID，格式为时间戳-随机字符串
// 返回值:
//
//	string: 连接ID
func generateConnectionID() string {
	return time.Now().Format("20060102150405") + "-" + randomString(8)
}

// randomString 生成随机字符串
// 功能: 生成指定长度的随机字符串
// 参数:
//
//	length: 字符串长度
//
// 返回值:
//
//	string: 随机字符串
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(result)
}

// CreateGroup 创建推送组
// 功能: 创建一个新的推送组
// 参数:
//
//	groupID: 组ID
//	groupName: 组名称
func (cm *ConnectionManager) CreateGroup(groupID string, groupName string) {
	// 检查组是否已存在
	if _, ok := cm.groups.Load(groupID); ok {
		tlog.Warn("组已存在", "groupID", groupID)
		return
	}

	// 创建新组
	cm.groups.Store(groupID, map[string]bool{})
	// 输出调试日志
	tlog.Debug("推送组已创建", "groupID", groupID, "groupName", groupName)
}

// DeleteGroup 删除推送组
// 功能: 删除一个推送组
// 参数:
//
//	groupID: 组ID
func (cm *ConnectionManager) DeleteGroup(groupID string) {
	// 检查组是否存在
	if _, ok := cm.groups.Load(groupID); !ok {
		tlog.Warn("组不存在", "groupID", groupID)
		return
	}

	// 删除组
	cm.groups.Delete(groupID)
	// 输出调试日志
	tlog.Debug("推送组已删除", "groupID", groupID)
}

// AddUserToGroup 添加用户到推送组
// 功能: 将用户添加到指定的推送组
// 参数:
//
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) AddUserToGroup(groupID string, userUUID string) {
	// 检查组是否存在
	if groupUsers, ok := cm.groups.Load(groupID); ok {
		// 添加用户到组
		groupUsers.(map[string]bool)[userUUID] = true
		// 输出调试日志
		tlog.Debug("用户已添加到推送组", "groupID", groupID, "userUUID", userUUID)
	} else {
		tlog.Warn("组不存在", "groupID", groupID)
	}
}

// RemoveUserFromGroup 从推送组中移除用户
// 功能: 将用户从指定的推送组中移除
// 参数:
//
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) RemoveUserFromGroup(groupID string, userUUID string) {
	// 检查组是否存在
	if groupUsers, ok := cm.groups.Load(groupID); ok {
		// 从组中移除用户
		delete(groupUsers.(map[string]bool), userUUID)
		// 输出调试日志
		tlog.Debug("用户已从推送组中移除", "groupID", groupID, "userUUID", userUUID)
	} else {
		tlog.Warn("组不存在", "groupID", groupID)
	}
}

// SendToUser 发送消息到指定用户
// 功能: 向指定用户的所有连接发送消息
// 参数:
//
//	userUUID: 用户UUID
//	message: 消息内容
//
// 返回值:
//
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToUser(userUUID string, message interface{}) bool {
	// 检查用户是否存在
	connections, ok := cm.userConnections.Load(userUUID)
	if !ok {
		tlog.Warn("用户不存在", "userUUID", userUUID)
		return false
	}

	// 向用户的所有连接发送消息
	success := false
	for connectionID := range connections.(map[string]bool) {
		if cm.SendToConnection(connectionID, message) {
			success = true
		}
	}

	return success
}

// SendToGroup 发送消息到指定推送组
// 功能: 向指定推送组的所有用户发送消息
// 参数:
//
//	groupID: 组ID
//	message: 消息内容
//
// 返回值:
//
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToGroup(groupID string, message interface{}) bool {
	// 检查组是否存在
	groupUsers, ok := cm.groups.Load(groupID)
	if !ok {
		tlog.Warn("组不存在", "groupID", groupID)
		return false
	}

	// 向组中的所有用户发送消息
	success := false
	for userUUID := range groupUsers.(map[string]bool) {
		if cm.SendToUser(userUUID, message) {
			success = true
		}
	}

	return success
}

// GetUserConnections 获取用户的所有连接
// 功能: 获取指定用户的所有连接ID
// 参数:
//
//	userUUID: 用户UUID
//
// 返回值:
//
//	[]string: 连接ID列表
func (cm *ConnectionManager) GetUserConnections(userUUID string) []string {
	// 检查用户是否存在
	connections, ok := cm.userConnections.Load(userUUID)
	if !ok {
		return []string{}
	}

	// 提取连接ID列表
	connMap := connections.(map[string]bool)
	connIDs := make([]string, 0, len(connMap))
	for connID := range connMap {
		connIDs = append(connIDs, connID)
	}

	return connIDs
}

// GetGroupUsers 获取推送组的所有用户
// 功能: 获取指定推送组的所有用户UUID
// 参数:
//
//	groupID: 组ID
//
// 返回值:
//
//	[]string: 用户UUID列表
func (cm *ConnectionManager) GetGroupUsers(groupID string) []string {
	// 检查组是否存在
	groupUsers, ok := cm.groups.Load(groupID)
	if !ok {
		return []string{}
	}

	// 提取用户UUID列表
	userMap := groupUsers.(map[string]bool)
	userUUIDs := make([]string, 0, len(userMap))
	for userUUID := range userMap {
		userUUIDs = append(userUUIDs, userUUID)
	}

	return userUUIDs
}

// UpdateUserConnection 更新连接的用户映射
// 功能: 更新指定连接的用户UUID映射
// 参数:
//
//	connectionID: 连接ID
//	oldUserUUID: 旧用户UUID
//	newUserUUID: 新用户UUID
func (cm *ConnectionManager) UpdateUserConnection(connectionID string, oldUserUUID string, newUserUUID string) {
	// 从旧用户的连接集合中移除连接ID
	if oldUserUUID != "" {
		if connections, ok := cm.userConnections.Load(oldUserUUID); ok {
			connMap := connections.(map[string]bool)
			delete(connMap, connectionID)
			// 如果旧用户没有连接了，从用户连接映射中移除该用户
			if len(connMap) == 0 {
				cm.userConnections.Delete(oldUserUUID)
			}
		}
	}

	// 向新用户的连接集合中添加连接ID
	if newUserUUID != "" {
		if connections, ok := cm.userConnections.Load(newUserUUID); ok {
			// 如果新用户存在，添加新的连接ID
			connections.(map[string]bool)[connectionID] = true
		} else {
			// 如果新用户不存在，创建新的连接集合
			cm.userConnections.Store(newUserUUID, map[string]bool{connectionID: true})
		}
	}

	// 输出调试日志
	tlog.Debug("连接用户映射已更新", "connectionID", connectionID, "oldUserUUID", oldUserUUID, "newUserUUID", newUserUUID)
}
