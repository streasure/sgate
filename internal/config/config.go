package config

import (
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port       int              `yaml:"port"`
	LogLevel   string           `yaml:"logLevel"`
	Zone       string           `yaml:"zone"`
	Redis      RedisConfig      `yaml:"redis"`
	Discovery  DiscoveryConfig  `yaml:"discovery"`
	Transports []Transport      `yaml:"transports"`
	GRPC       GRPCConfig       `yaml:"grpc"`
	Stream     StreamConfig     `yaml:"stream"`
	Protection ProtectionConfig `yaml:"protection"`
}

type RedisConfig struct {
	Addr         string `yaml:"addr"`
	Password     string `yaml:"password"`
	DB           int    `yaml:"db"`
	PoolSize     int    `yaml:"poolSize"`
	MinIdleConns int    `yaml:"minIdleConns"`
}

type DiscoveryConfig struct {
	Enabled           bool          `yaml:"enabled"`
	ServiceName       string        `yaml:"serviceName"`
	Zone              string        `yaml:"zone"`
	HeartbeatInterval time.Duration `yaml:"heartbeatInterval"`
	HeartbeatTTL      time.Duration `yaml:"heartbeatTTL"`
	DeregisterDelay   time.Duration `yaml:"deregisterDelay"`
	ScanInterval      time.Duration `yaml:"scanInterval"`
}

type GRPCConfig struct {
	Port           int `yaml:"port"`
	WindowSize     int `yaml:"windowSize"`
	MaxMessageSize int `yaml:"maxMessageSize"`
}

type StreamConfig struct {
	ShardCount       int `yaml:"shardCount"`
	SendChannelSize  int `yaml:"sendChannelSize"`
	ReceiveBatchSize int `yaml:"receiveBatchSize"`
}

type ProtectionConfig struct {
	MaxFrameSize       int     `yaml:"maxFrameSize"`
	MaxFrameBufSize    int     `yaml:"maxFrameBufSize"`
	MaxWSFrameSize     int     `yaml:"maxWSFrameSize"`
	MaxWSBufferSize    int     `yaml:"maxWSBufferSize"`
	MemoryThreshold    float64 `yaml:"memoryThreshold"`
	CPUThreshold       float64 `yaml:"cpuThreshold"`
	DropOnOverload     bool    `yaml:"dropOnOverload"`
	CheckIntervalMs    int     `yaml:"checkIntervalMs"`
	WSHeartbeatTimeout int     `yaml:"wsHeartbeatTimeout"`
	WSCheckInterval    int     `yaml:"wsCheckInterval"`
	ConnCheckInterval  string  `yaml:"connCheckInterval"`
	ConnIdleTimeout    string  `yaml:"connIdleTimeout"`
}

type Transport struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	Type     string `yaml:"type"`
}

func LoadConfig() (*Config, error) {
	configFile := "config/config.yaml"
	if _, err := os.Stat(configFile); err != nil {
		configFile = "../config/config.yaml"
		if _, err := os.Stat(configFile); err != nil {
			configFile = "../../config/config.yaml"
			if _, err := os.Stat(configFile); err != nil {
				return loadDefaultConfig(), nil
			}
		}
	}

	file, err := os.Open(configFile)
	if err != nil {
		return loadDefaultConfig(), nil
	}
	defer file.Close()

	var cfg Config
	if err := yaml.NewDecoder(file).Decode(&cfg); err != nil {
		return loadDefaultConfig(), nil
	}

	return &cfg, nil
}

func loadDefaultConfig() *Config {
	port := getEnvInt("PORT", 8080)
	logLevel := getEnvString("LOG_LEVEL", "info")
	redisAddr := getEnvString("REDIS_ADDR", "127.0.0.1:6379")
	redisPassword := getEnvString("REDIS_PASSWORD", "")
	redisDB := getEnvInt("REDIS_DB", 10)
	redisPoolSize := getEnvInt("REDIS_POOL_SIZE", 10)
	redisMinIdleConns := getEnvInt("REDIS_MIN_IDLE_CONNS", 5)

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
		},
		Discovery: DiscoveryConfig{
			Enabled:           true,
			ServiceName:       "logic",
			HeartbeatInterval: 3 * time.Second,
			HeartbeatTTL:      10 * time.Second,
			DeregisterDelay:   5 * time.Second,
			ScanInterval:      10 * time.Second,
		},
		Transports: defaultTransports,
		GRPC: GRPCConfig{
			Port:           50051,
			WindowSize:     524288,
			MaxMessageSize: 4 * 1024 * 1024,
		},
		Stream: StreamConfig{
			ShardCount:       0,
			SendChannelSize:  65536,
			ReceiveBatchSize: 64,
		},
		Protection: ProtectionConfig{
			MaxFrameSize:       4 * 1024 * 1024,
			MaxFrameBufSize:    4 * 1024 * 1024,
			MaxWSFrameSize:     4 * 1024 * 1024,
			MaxWSBufferSize:    4 * 1024 * 1024,
			MemoryThreshold:    90.0,
			CPUThreshold:       90.0,
			DropOnOverload:     true,
			CheckIntervalMs:    200,
			WSHeartbeatTimeout: 60,
			WSCheckInterval:    30,
			ConnCheckInterval:  "5m",
			ConnIdleTimeout:    "30s",
		},
	}
}

func getEnvString(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}
