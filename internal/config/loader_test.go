package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	Name string `mapstructure:"name"`
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(path, []byte("host: localhost\nport: 8080\nname: test\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFiles[testConfig](path)
	if err != nil {
		t.Fatalf("loadFiles error: %v", err)
	}

	if cfg.Host != "localhost" {
		t.Fatalf("Host: got %q, want %q", cfg.Host, "localhost")
	}
	if cfg.Port != 8080 {
		t.Fatalf("Port: got %d, want 8080", cfg.Port)
	}
	if cfg.Name != "test" {
		t.Fatalf("Name: got %q, want %q", cfg.Name, "test")
	}
}

func TestLoadMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	path1 := filepath.Join(dir, "config1.yaml")
	err := os.WriteFile(path1, []byte("host: host1\nport: 1111\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	path2 := filepath.Join(dir, "config2.yaml")
	err = os.WriteFile(path2, []byte("name: from_file2\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFiles[testConfig](path1, path2)
	if err != nil {
		t.Fatalf("loadFiles error: %v", err)
	}

	if cfg.Host != "host1" {
		t.Fatalf("Host: got %q, want %q", cfg.Host, "host1")
	}
	if cfg.Port != 1111 {
		t.Fatalf("Port: got %d, want 1111", cfg.Port)
	}
	if cfg.Name != "from_file2" {
		t.Fatalf("Name: got %q, want %q", cfg.Name, "from_file2")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := loadFiles[testConfig]("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := loadFiles[testConfig](dir)
	if err == nil {
		t.Fatal("expected error for directory path")
	}
}

func TestLoadNoExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	err := os.WriteFile(path, []byte("host: localhost\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = loadFiles[testConfig](path)
	if err == nil {
		t.Fatal("expected error for file without extension")
	}
}

type redisSection struct {
	Host     string `yaml:"host"`
	Password string `yaml:"password"`
}

type deepTestConfig struct {
	Redis redisSection `yaml:"redis"`
	Name  string       `yaml:"name"`
}

func TestLoadDeepMerge(t *testing.T) {
	dir := t.TempDir()

	path1 := filepath.Join(dir, "base.yaml")
	err := os.WriteFile(path1, []byte("redis:\n  host: h1\n  password: p1\nname: base\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	path2 := filepath.Join(dir, "override.yaml")
	err = os.WriteFile(path2, []byte("redis:\n  host: h2\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFiles[deepTestConfig](path1, path2)
	if err != nil {
		t.Fatalf("loadFiles error: %v", err)
	}

	if cfg.Redis.Host != "h2" {
		t.Errorf("Redis.Host = %q, want h2 (later file should override)", cfg.Redis.Host)
	}
	if cfg.Redis.Password != "p1" {
		t.Errorf("Redis.Password = %q, want p1 (deep merge must not drop sibling keys)", cfg.Redis.Password)
	}
	if cfg.Name != "base" {
		t.Errorf("Name = %q, want base", cfg.Name)
	}
}

func TestLoadUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	err := os.WriteFile(path, []byte("{\"host\": \"localhost\"}"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = loadFiles[testConfig](path)
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention unsupported extension, got: %v", err)
	}
}

func TestLoadYmlExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	err := os.WriteFile(path, []byte("host: localhost\nport: 80\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFiles[testConfig](path)
	if err != nil {
		t.Fatalf("load .yml should work, got error: %v", err)
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", cfg.Host)
	}
}
