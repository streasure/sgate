package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	tlog "github.com/streasure/treasure-slog"
)

// Redis Redis管理器
type Redis struct {
	client         *redis.Client
	ctx            context.Context
	cancel         context.CancelFunc
	config         RedisConfig
	isConnected    bool
	reconnectCount int
}

// RedisConfig Redis配置
type RedisConfig struct {
	Host         string
	Port         int
	Password     string
	DB           int
	MaxRetries   int
	RetryInterval time.Duration
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
	MinIdleConns int
}

// NewRedis 创建Redis管理器
func NewRedis(config RedisConfig) *Redis {
	ctx, cancel := context.WithCancel(context.Background())

	r := &Redis{
		ctx:     ctx,
		cancel:  cancel,
		config:  config,
	}

	// 初始化Redis连接
	go r.connect()

	return r
}

// connect 连接Redis
func (r *Redis) connect() {
	r.client = redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%d", r.config.Host, r.config.Port),
		Password:     r.config.Password,
		DB:           r.config.DB,
		MaxRetries:   r.config.MaxRetries,
		MinRetryBackoff: r.config.RetryInterval,
		MaxRetryBackoff: r.config.RetryInterval * 5,
		DialTimeout:  r.config.DialTimeout,
		ReadTimeout:  r.config.ReadTimeout,
		WriteTimeout: r.config.WriteTimeout,
		PoolSize:     r.config.PoolSize,
		MinIdleConns: r.config.MinIdleConns,
	})

	// 测试连接
	for i := 0; i < r.config.MaxRetries; i++ {
		_, err := r.client.Ping(r.ctx).Result()
		if err == nil {
			r.isConnected = true
			r.reconnectCount = 0
			tlog.Info("Redis连接成功", "host", r.config.Host, "port", r.config.Port, "db", r.config.DB)
			return
		}

		tlog.Error("Redis连接失败", "error", err, "retry", i+1, "maxRetries", r.config.MaxRetries)
		time.Sleep(r.config.RetryInterval)
	}

	tlog.Error("Redis连接失败，达到最大重试次数")
}

// Close 关闭Redis连接
func (r *Redis) Close() error {
	r.cancel()
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

// IsConnected 检查Redis连接状态
func (r *Redis) IsConnected() bool {
	if r.client == nil {
		return false
	}

	_, err := r.client.Ping(r.ctx).Result()
	if err != nil {
		r.isConnected = false
		tlog.Error("Redis连接已断开", "error", err)
		// 尝试重连
		go r.connect()
		return false
	}

	r.isConnected = true
	return true
}

// Set 设置键值对
func (r *Redis) Set(key string, value interface{}, expiration time.Duration) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	// 序列化值
	var serializedValue interface{}
	switch v := value.(type) {
	case string:
		serializedValue = v
	case int, int64, float64, bool:
		serializedValue = v
	default:
		// 对于复杂类型，序列化为JSON
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		serializedValue = string(data)
	}

	return r.client.Set(r.ctx, key, serializedValue, expiration).Err()
}

// Get 获取值
func (r *Redis) Get(key string, dest interface{}) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	val, err := r.client.Get(r.ctx, key).Result()
	if err == redis.Nil {
		return fmt.Errorf("key not found")
	}
	if err != nil {
		return err
	}

	// 反序列化值
	switch d := dest.(type) {
	case *string:
		*d = val
	case *int:
		var i int
		if _, err := fmt.Sscanf(val, "%d", &i); err != nil {
			return err
		}
		*d = i
	case *int64:
		var i int64
		if _, err := fmt.Sscanf(val, "%d", &i); err != nil {
			return err
		}
		*d = i
	case *float64:
		var f float64
		if _, err := fmt.Sscanf(val, "%f", &f); err != nil {
			return err
		}
		*d = f
	case *bool:
		var b bool
		if _, err := fmt.Sscanf(val, "%t", &b); err != nil {
			return err
		}
		*d = b
	default:
		// 对于复杂类型，从JSON反序列化
		return json.Unmarshal([]byte(val), dest)
	}

	return nil
}

// Delete 删除键
func (r *Redis) Delete(key string) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	return r.client.Del(r.ctx, key).Err()
}

// Exists 检查键是否存在
func (r *Redis) Exists(key string) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	result, err := r.client.Exists(r.ctx, key).Result()
	if err != nil {
		return false, err
	}

	return result > 0, nil
}

// Expire 设置键过期时间
func (r *Redis) Expire(key string, expiration time.Duration) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	return r.client.Expire(r.ctx, key, expiration).Err()
}

// GetTTL 获取键剩余过期时间
func (r *Redis) GetTTL(key string) (time.Duration, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.TTL(r.ctx, key).Result()
}

// Increment 递增计数器
func (r *Redis) Increment(key string) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.Incr(r.ctx, key).Result()
}

// Decrement 递减计数器
func (r *Redis) Decrement(key string) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.Decr(r.ctx, key).Result()
}

// HashSet 设置哈希表字段
func (r *Redis) HashSet(key, field string, value interface{}) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	// 序列化值
	var serializedValue interface{}
	switch v := value.(type) {
	case string:
		serializedValue = v
	case int, int64, float64, bool:
		serializedValue = v
	default:
		// 对于复杂类型，序列化为JSON
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		serializedValue = string(data)
	}

	return r.client.HSet(r.ctx, key, field, serializedValue).Err()
}

// HashGet 获取哈希表字段
func (r *Redis) HashGet(key, field string, dest interface{}) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	val, err := r.client.HGet(r.ctx, key, field).Result()
	if err == redis.Nil {
		return fmt.Errorf("field not found")
	}
	if err != nil {
		return err
	}

	// 反序列化值
	switch d := dest.(type) {
	case *string:
		*d = val
	case *int:
		var i int
		if _, err := fmt.Sscanf(val, "%d", &i); err != nil {
			return err
		}
		*d = i
	case *int64:
		var i int64
		if _, err := fmt.Sscanf(val, "%d", &i); err != nil {
			return err
		}
		*d = i
	case *float64:
		var f float64
		if _, err := fmt.Sscanf(val, "%f", &f); err != nil {
			return err
		}
		*d = f
	case *bool:
		var b bool
		if _, err := fmt.Sscanf(val, "%t", &b); err != nil {
			return err
		}
		*d = b
	default:
		// 对于复杂类型，从JSON反序列化
		return json.Unmarshal([]byte(val), dest)
	}

	return nil
}

// HashDelete 删除哈希表字段
func (r *Redis) HashDelete(key string, fields ...string) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	return r.client.HDel(r.ctx, key, fields...).Err()
}

// HashExists 检查哈希表字段是否存在
func (r *Redis) HashExists(key, field string) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	return r.client.HExists(r.ctx, key, field).Result()
}

// ListPush 向列表尾部添加元素
func (r *Redis) ListPush(key string, values ...interface{}) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.RPush(r.ctx, key, values...).Result()
}

// ListPop 从列表头部弹出元素
func (r *Redis) ListPop(key string) (string, error) {
	if !r.IsConnected() {
		return "", fmt.Errorf("redis not connected")
	}

	return r.client.LPop(r.ctx, key).Result()
}

// ListLen 获取列表长度
func (r *Redis) ListLen(key string) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.LLen(r.ctx, key).Result()
}

// SetAdd 向集合添加元素
func (r *Redis) SetAdd(key string, members ...interface{}) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.SAdd(r.ctx, key, members...).Result()
}

// SetRemove 从集合移除元素
func (r *Redis) SetRemove(key string, members ...interface{}) (int64, error) {
	if !r.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	return r.client.SRem(r.ctx, key, members...).Result()
}

// SetMembers 获取集合所有元素
func (r *Redis) SetMembers(key string) ([]string, error) {
	if !r.IsConnected() {
		return nil, fmt.Errorf("redis not connected")
	}

	return r.client.SMembers(r.ctx, key).Result()
}

// SetContains 检查集合是否包含元素
func (r *Redis) SetContains(key string, member interface{}) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	return r.client.SIsMember(r.ctx, key, member).Result()
}

// AcquireLock 获取分布式锁
func (r *Redis) AcquireLock(key string, value string, expiration time.Duration) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	// 使用SET NX EX命令获取锁
	result, err := r.client.SetNX(r.ctx, key, value, expiration).Result()
	if err != nil {
		return false, err
	}

	return result, nil
}

// ReleaseLock 释放分布式锁
func (r *Redis) ReleaseLock(key string, value string) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	// 使用Lua脚本释放锁，确保原子性
	luaScript := `
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1])
		else
			return 0
		end
	`

	result, err := r.client.Eval(r.ctx, luaScript, []string{key}, value).Result()
	if err != nil {
		return false, err
	}

	return result.(int64) > 0, nil
}

// Publish 发布消息
func (r *Redis) Publish(channel string, message interface{}) error {
	if !r.IsConnected() {
		return fmt.Errorf("redis not connected")
	}

	// 序列化消息
	var serializedMessage interface{}
	switch m := message.(type) {
	case string:
		serializedMessage = m
	default:
		data, err := json.Marshal(message)
		if err != nil {
			return err
		}
		serializedMessage = string(data)
	}

	return r.client.Publish(r.ctx, channel, serializedMessage).Err()
}

// Subscribe 订阅频道
func (r *Redis) Subscribe(channel string, callback func(string)) {
	if !r.IsConnected() {
		tlog.Error("Redis not connected, cannot subscribe")
		return
	}

	pubsub := r.client.Subscribe(r.ctx, channel)
	defer pubsub.Close()

	ch := pubsub.Channel()
	for msg := range ch {
		callback(msg.Payload)
	}
}

// RateLimit 速率限制
func (r *Redis) RateLimit(key string, limit int64, window time.Duration) (bool, error) {
	if !r.IsConnected() {
		return false, fmt.Errorf("redis not connected")
	}

	// 使用滑动窗口限流
	now := time.Now().UnixMilli()
	windowStart := now - int64(window.Milliseconds())

	// 移除窗口外的记录
	_, err := r.client.ZRemRangeByScore(r.ctx, key, "0", fmt.Sprintf("%d", windowStart)).Result()
	if err != nil {
		return false, err
	}

	// 获取当前窗口内的请求数
	count, err := r.client.ZCard(r.ctx, key).Result()
	if err != nil {
		return false, err
	}

	// 检查是否超过限制
	if count >= limit {
		return false, nil
	}

	// 添加当前请求
	_, err = r.client.ZAdd(r.ctx, key, &redis.Z{
		Score:  float64(now),
		Member: fmt.Sprintf("%d", now),
	}).Result()
	if err != nil {
		return false, err
	}

	// 设置过期时间
	_, err = r.client.Expire(r.ctx, key, window).Result()
	if err != nil {
		return false, err
	}

	return true, nil
}

// CacheUser 缓存用户信息
func (r *Redis) CacheUser(user *User) error {
	key := fmt.Sprintf("user:%s", user.UserUUID)
	return r.Set(key, user, 24*time.Hour)
}

// GetCachedUser 获取缓存的用户信息
func (r *Redis) GetCachedUser(userUUID string) (*User, error) {
	key := fmt.Sprintf("user:%s", userUUID)
	var user User
	err := r.Get(key, &user)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// CacheConnection 缓存连接信息
func (r *Redis) CacheConnection(conn *ConnectionRecord) error {
	key := fmt.Sprintf("connection:%s", conn.ConnectionID)
	return r.Set(key, conn, 24*time.Hour)
}

// GetCachedConnection 获取缓存的连接信息
func (r *Redis) GetCachedConnection(connectionID string) (*ConnectionRecord, error) {
	key := fmt.Sprintf("connection:%s", connectionID)
	var conn ConnectionRecord
	err := r.Get(key, &conn)
	if err != nil {
		return nil, err
	}
	return &conn, nil
}

// CacheGroup 缓存组信息
func (r *Redis) CacheGroup(group *GroupRecord) error {
	key := fmt.Sprintf("group:%s", group.GroupID)
	return r.Set(key, group, 24*time.Hour)
}

// GetCachedGroup 获取缓存的组信息
func (r *Redis) GetCachedGroup(groupID string) (*GroupRecord, error) {
	key := fmt.Sprintf("group:%s", groupID)
	var group GroupRecord
	err := r.Get(key, &group)
	if err != nil {
		return nil, err
	}
	return &group, nil
}
