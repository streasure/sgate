package config

import (
	"os"
	"path/filepath"
	"testing"
)

// findRepoRoot 从当前测试目录向上找到 go.mod 所在的仓库根。
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from package dir")
		}
		dir = parent
	}
}

// TestLoad_RepoGatewayYaml 仓库根 config/gateway.yaml 必须能 Load+Validate，
// 且关键值与文件内容一致（防 yaml 与 Config 结构体漂移）。
func TestLoad_RepoGatewayYaml(t *testing.T) {
	p := filepath.Join(findRepoRoot(t), "config", "gateway.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("config/gateway.yaml missing: %v", err)
	}

	c, err := Load(p) // Load 内部已调 ApplyRuntimeDefaults + Validate
	if err != nil {
		t.Fatalf("Load(config/gateway.yaml) error = %v", err)
	}
	if Path() != p {
		t.Errorf("Path() = %q, want %q", Path(), p)
	}

	if c.HttpPort != 8081 {
		t.Errorf("httpPort = %d, want 8081 (from config/gateway.yaml)", c.HttpPort)
	}
	if c.ServerID != "gateway-1" {
		t.Errorf("serverId = %q, want gateway-1", c.ServerID)
	}
	if c.ServerType != "Gateway" {
		t.Errorf("serverType = %q, want Gateway", c.ServerType)
	}
	if c.Belong != "default" || c.Zone != "default" {
		t.Errorf("belong/zone = %q/%q, want default/default", c.Belong, c.Zone)
	}
	if len(c.Transports) != 2 {
		t.Fatalf("len(transports) = %d, want 2", len(c.Transports))
	}
	if c.Transports[0].Port != 48080 || c.Transports[0].Type != "" {
		t.Errorf("transports[0] = %+v, want {48080 tcp-default}", c.Transports[0])
	}
	if c.Transports[1].Port != 48081 || c.Transports[1].Type != "websocket" {
		t.Errorf("transports[1] = %+v, want {48081 websocket}", c.Transports[1])
	}
	if !c.Discovery.Enabled || !c.Discovery.RegisterSelf {
		t.Errorf("discovery enabled/registerSelf = %v/%v, want true/true", c.Discovery.Enabled, c.Discovery.RegisterSelf)
	}
	if c.GRPC.Port != 50051 {
		t.Errorf("grpc.port = %d, want 50051", c.GRPC.Port)
	}
	if c.LoginValidation.Enabled {
		t.Error("loginValidation.enabled = true in default gateway.yaml, want false")
	}
	if !c.Security.Enabled {
		t.Error("security.enabled = false in gateway.yaml, want true")
	}
	if c.Protection.CPUThreshold != 92 {
		t.Errorf("protection.cpuThreshold = %v, want 92", c.Protection.CPUThreshold)
	}
	if !c.Protection.DropOnOverload {
		t.Error("protection.dropOnOverload = false in gateway.yaml, want true")
	}
}

// TestLoad_RepoBenchGatewayYamls 逐个加载 config/bench/*.yaml（网关压测配置）。
// 断言 Load+Validate 通过、压测铁律（loginValidation 必须 false）以及各文件差异值。
// 注：bench/*/**.yaml 下的 log.yaml 是 logic 压测端日志配置（非网关配置），不在覆盖范围。
func TestLoad_RepoBenchGatewayYamls(t *testing.T) {
	root := findRepoRoot(t)
	cases := []struct {
		file  string
		check func(t *testing.T, c *Config)
	}{
		{
			file: "config/bench/1m.yaml",
			check: func(t *testing.T, c *Config) {
				if c.ServerID != "gateway-1m" {
					t.Errorf("serverId = %q, want gateway-1m", c.ServerID)
				}
				if len(c.Transports) != 53 {
					t.Errorf("len(transports) = %d, want 53 (2 + 49000..49050)", len(c.Transports))
				}
				if c.Protection.MaxConnections != 1200000 {
					t.Errorf("maxConnections = %d, want 1200000", c.Protection.MaxConnections)
				}
				if c.Protection.MaxFrameBufSize != 65536 {
					t.Errorf("maxFrameBufSize = %d, want 65536", c.Protection.MaxFrameBufSize)
				}
				if c.Stream.ShardCount != 128 {
					t.Errorf("stream.shardCount = %d, want 128", c.Stream.ShardCount)
				}
				if c.Security.Enabled {
					t.Error("security.enabled = true in 1m bench config, want false")
				}
			},
		},
		{
			file: "config/bench/batch-on.yaml",
			check: func(t *testing.T, c *Config) {
				if !c.Stream.BatchPush {
					t.Error("stream.batchPush = false in batch-on.yaml, want true")
				}
				if !c.Protection.BenchMode {
					t.Error("protection.benchMode = false in batch-on.yaml, want true")
				}
				if c.Security.RateLimit.Enabled {
					t.Error("rateLimit.enabled = true in batch-on.yaml, want false")
				}
			},
		},
		{
			file: "config/bench/batch-off.yaml",
			check: func(t *testing.T, c *Config) {
				if c.Stream.BatchPush {
					t.Error("stream.batchPush = true in batch-off.yaml, want false")
				}
				if !c.Protection.BenchMode {
					t.Error("protection.benchMode = false in batch-off.yaml, want true")
				}
			},
		},
		{
			file: "config/bench/rate-limit.yaml",
			check: func(t *testing.T, c *Config) {
				if !c.Security.RateLimit.Enabled {
					t.Error("rateLimit.enabled = false in rate-limit.yaml, want true")
				}
				if c.Security.RateLimit.MaxTokens != 2000000 {
					t.Errorf("rateLimit.maxTokens = %d, want 2000000", c.Security.RateLimit.MaxTokens)
				}
				if !c.Protection.BenchMode {
					t.Error("protection.benchMode = false in rate-limit.yaml, want true")
				}
			},
		},
	}

	if _, err := os.Stat(filepath.Join(root, "config", "bench")); err != nil {
		t.Skipf("config/bench missing: %v", err)
	}
	for _, tt := range cases {
		t.Run(tt.file, func(t *testing.T) {
			p := filepath.Join(root, filepath.FromSlash(tt.file))
			if _, err := os.Stat(p); err != nil {
				t.Skipf("%s missing: %v", tt.file, err)
			}
			c, err := Load(p)
			if err != nil {
				t.Fatalf("Load(%s) error = %v", tt.file, err)
			}
			if Path() != p {
				t.Errorf("Path() = %q, want %q", Path(), p)
			}
			if c.HttpPort != 8081 {
				t.Errorf("httpPort = %d, want 8081", c.HttpPort)
			}
			if c.LoginValidation.Enabled {
				t.Errorf("%s: loginValidation.enabled = true, bench configs must be false (AGENTS.md)", tt.file)
			}
			if len(c.Transports) == 0 {
				t.Error("transports is empty")
			}
			tt.check(t, c)
		})
	}
}
