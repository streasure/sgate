package config

import (
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 配置结构
// 字段:
//   Port: 服务端口
//   LogLevel: 日志级别
//   Redis: Redis配置
//   Transports: 支持的协议列表
//   Network: 网络配置
//   WorkerPool: 工作池配置
//   RateLimiter: 速率限制配置
//   Alerts: 告警配置
//   Security: 安全配置

type Config struct {
	Port        int            `yaml:"port"`        // 服务端口
	LogLevel    string         `yaml:"logLevel"`    // 日志级别
	Redis       RedisConfig    `yaml:"redis"`       // Redis配置
	Transports  []Transport    `yaml:"transports"`  // 支持的协议列表
	Network     NetworkConfig  `yaml:"network"`     // 网络配置
	WorkerPool  WorkerPoolConfig `yaml:"workerPool"` // 工作池配置
	RateLimiter RateLimiterConfig `yaml:"rateLimiter"` // 速率限制配置
	Alerts      AlertConfig    `yaml:"alerts"`      // 告警配置
	Security    SecurityConfig `yaml:"security"`    // 安全配置
}

// RedisConfig Redis配置结构
// 字段:
//   Addr: Redis地址
//   Password: Redis密码
//   DB: Redis数据库
//   PoolSize: 连接池大小
//   MinIdleConns: 最小空闲连接数
//   MaxIdleConns: 最大空闲连接数

type RedisConfig struct {
	Addr         string `yaml:"addr"`         // Redis地址
	Password     string `yaml:"password"`     // Redis密码
	DB           int    `yaml:"db"`           // Redis数据库
	PoolSize     int    `yaml:"poolSize"`     // 连接池大小
	MinIdleConns int    `yaml:"minIdleConns"` // 最小空闲连接数
	MaxIdleConns int    `yaml:"maxIdleConns"` // 最大空闲连接数
}

// NetworkConfig 网络配置结构
// 字段:
//   EventLoopCount: 事件循环数量
//   ReadBufferSize: 读取缓冲区大小
//   WriteBufferSize: 写入缓冲区大小
//   TCPKeepAlive: TCP保活时间

type NetworkConfig struct {
	EventLoopCount   int           `yaml:"eventLoopCount"`   // 事件循环数量
	ReadBufferSize   int           `yaml:"readBufferSize"`   // 读取缓冲区大小
	WriteBufferSize  int           `yaml:"writeBufferSize"`  // 写入缓冲区大小
	TCPKeepAlive     time.Duration `yaml:"tcpKeepAlive"`     // TCP保活时间
}

// WorkerPoolConfig 工作池配置结构
// 字段:
//   MinWorkers: 最小工作线程数
//   MaxWorkers: 最大工作线程数
//   QueueSize: 工作队列大小
//   QueueSizeThreshold: 队列大小阈值

type WorkerPoolConfig struct {
	MinWorkers         int `yaml:"minWorkers"`         // 最小工作线程数
	MaxWorkers         int `yaml:"maxWorkers"`         // 最大工作线程数
	QueueSize          int `yaml:"queueSize"`          // 工作队列大小
	QueueSizeThreshold int `yaml:"queueSizeThreshold"` // 队列大小阈值
}

// RateLimiterConfig 速率限制配置结构
// 字段:
//   Rate: 速率限制（每秒请求数）
//   Burst: 突发限制
//   Window: 时间窗口

type RateLimiterConfig struct {
	Rate   int           `yaml:"rate"`   // 速率限制（每秒请求数）
	Burst  int           `yaml:"burst"`  // 突发限制
	Window time.Duration `yaml:"window"` // 时间窗口
}

// AlertConfig 告警配置结构
// 字段:
//   ActiveConnectionsThreshold: 活跃连接数阈值
//   FailedMessagesThreshold: 失败消息数阈值
//   ProcessingTimeThreshold: 处理时间阈值
//   RedisErrorsThreshold: Redis错误数阈值
//   QueueLengthThreshold: 消息队列长度阈值

type AlertConfig struct {
	ActiveConnectionsThreshold int64 `yaml:"activeConnectionsThreshold"` // 活跃连接数阈值
	FailedMessagesThreshold    int64 `yaml:"failedMessagesThreshold"`    // 失败消息数阈值
	ProcessingTimeThreshold    int64 `yaml:"processingTimeThreshold"`    // 处理时间阈值
	RedisErrorsThreshold       int64 `yaml:"redisErrorsThreshold"`       // Redis错误数阈值
	QueueLengthThreshold       int64 `yaml:"queueLengthThreshold"`       // 消息队列长度阈值
}

// SecurityConfig 安全配置结构
// 字段:
//   AuthSecret: 认证密钥
//   EnableIPWhitelist: 是否启用IP白名单
//   EnableIPBlacklist: 是否启用IP黑名单
//   DefaultWhitelist: 默认白名单
//   DefaultBlacklist: 默认黑名单
//   AuthRoutes: 需要认证的路由列表

type SecurityConfig struct {
	AuthSecret        string   `yaml:"authSecret"`        // 认证密钥
	EnableIPWhitelist bool     `yaml:"enableIPWhitelist"` // 是否启用IP白名单
	EnableIPBlacklist bool     `yaml:"enableIPBlacklist"` // 是否启用IP黑名单
	DefaultWhitelist  []string `yaml:"defaultWhitelist"`  // 默认白名单
	DefaultBlacklist  []string `yaml:"defaultBlacklist"`  // 默认黑名单
	AuthRoutes        []string `yaml:"authRoutes"`        // 需要认证的路由列表
}

// Transport 传输协议配置
// 字段:
//   Protocol: 协议类型: tcp, udp
//   Port: 端口号
//   Type: 类型: websocket (可选)

type Transport struct {
	Protocol string `yaml:"protocol"` // 协议类型: tcp, udp
	Port     int    `yaml:"port"`     // 端口号
	Type     string `yaml:"type"`     // 类型: websocket (可选)
}

// LoadConfig 加载配置
// 返回值:
//
//	*Config: 配置实例
//	error: 错误信息
func LoadConfig() (*Config, error) {
	// 从配置文件读取配置
	configFile := "config/config.yaml"
	// 尝试在当前目录查找
	if _, err := os.Stat(configFile); err != nil {
		// 尝试在上一级目录查找
		configFile = "../config/config.yaml"
		if _, err := os.Stat(configFile); err != nil {
			// 尝试在更上一级目录查找
			configFile = "../../config/config.yaml"
			if _, err := os.Stat(configFile); err != nil {
				// 如果配置文件不存在，使用默认配置
				return loadDefaultConfig(), nil
			}
		}
	}

	file, err := os.Open(configFile)
	if err != nil {
		// 如果配置文件打开失败，使用默认配置
		return loadDefaultConfig(), nil
	}
	defer file.Close()

	var cfg Config
	if err := yaml.NewDecoder(file).Decode(&cfg); err != nil {
		// 如果配置文件解析失败，使用默认配置
		return loadDefaultConfig(), nil
	}

	return &cfg, nil
}

// loadDefaultConfig 加载默认配置
// 返回值:
//
//	*Config: 默认配置实例
func loadDefaultConfig() *Config {
	// 从环境变量读取配置
	port := getEnvInt("PORT", 8080)
	logLevel := getEnvString("LOG_LEVEL", "info")
	redisAddr := getEnvString("REDIS_ADDR", "127.0.0.1:6379")
	redisPassword := getEnvString("REDIS_PASSWORD", "")
	redisDB := getEnvInt("REDIS_DB", 10)
	redisPoolSize := getEnvInt("REDIS_POOL_SIZE", 10)
	redisMinIdleConns := getEnvInt("REDIS_MIN_IDLE_CONNS", 5)
	redisMaxIdleConns := getEnvInt("REDIS_MAX_IDLE_CONNS", 10)

	// 默认支持的协议
	defaultTransports := []Transport{
		{Protocol: "tcp", Port: 8080},
		{Protocol: "udp", Port: 8081},
		{Protocol: "tcp", Port: 8082, Type: "websocket"},
	}

	return &Config{
		Port:     port,
		LogLevel: logLevel,
		Redis: RedisConfig{
			Addr:         redisAddr,
			Password:     redisPassword,
			DB:           redisDB,
			PoolSize:     redisPoolSize,
			MinIdleConns: redisMinIdleConns,
			MaxIdleConns: redisMaxIdleConns,
		},
		Transports: defaultTransports,
		Network: NetworkConfig{
			EventLoopCount:   0, // 0表示使用CPU核心数
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
			TCPKeepAlive:     30 * time.Second,
		},
		WorkerPool: WorkerPoolConfig{
			MinWorkers:         16, // 默认最小工作线程数
			MaxWorkers:         128, // 默认最大工作线程数
			QueueSize:          1000000, // 默认队列大小
			QueueSizeThreshold: 1000, // 默认队列大小阈值
		},
		RateLimiter: RateLimiterConfig{
			Rate:   1000, // 默认速率限制
			Burst:  2000, // 默认突发限制
			Window: 1 * time.Second, // 默认时间窗口
		},
		Alerts: AlertConfig{
			ActiveConnectionsThreshold: 1000, // 活跃连接数阈值
			FailedMessagesThreshold:    100,  // 失败消息数阈值
			ProcessingTimeThreshold:    100,  // 处理时间阈值
			RedisErrorsThreshold:       10,   // Redis错误数阈值
			QueueLengthThreshold:       10000, // 消息队列长度阈值
		},
		Security: SecurityConfig{
			AuthSecret:        "default_secret", // 默认认证密钥
			EnableIPWhitelist: false, // 默认不启用IP白名单
			EnableIPBlacklist: true,  // 默认启用IP黑名单
			DefaultWhitelist:  []string{"127.0.0.1", "::1"}, // 默认白名单
			DefaultBlacklist:  []string{}, // 默认黑名单
			AuthRoutes:        []string{"getConnections", "broadcast", "ws.broadcast"}, // 默认需要认证的路由
		},
	}
}

// getEnvString 获取环境变量（字符串）
// 参数:
//
//	key: 环境变量键名
//	defaultValue: 默认值
//
// 返回值:
//
//	string: 环境变量值或默认值
func getEnvString(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// getEnvInt 获取环境变量（整数）
// 参数:
//
//	key: 环境变量键名
//	defaultValue: 默认值
//
// 返回值:
//
//	int: 环境变量值或默认值
func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}