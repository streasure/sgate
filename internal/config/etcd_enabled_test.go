package config

import (
	"testing"
)

// TestEtcdEnabled_NilDefaultsTrue 缺省（yaml 未写 enabled）→ 启用。
func TestEtcdEnabled_NilDefaultsTrue(t *testing.T) {
	var e EtcdConfig
	if !e.IsEnabled() {
		t.Fatal("nil Enabled should default to true")
	}
}

// TestEtcdEnabled_ExplicitFalse yaml `etcd.enabled: false` → 关闭。
func TestEtcdEnabled_ExplicitFalse(t *testing.T) {
	f := false
	e := EtcdConfig{Enabled: &f}
	if e.IsEnabled() {
		t.Fatal("explicit false should disable etcd")
	}
}

// TestEtcdEnabled_ExplicitTrue 显式 true → 启用。
func TestEtcdEnabled_ExplicitTrue(t *testing.T) {
	tr := true
	e := EtcdConfig{Enabled: &tr}
	if !e.IsEnabled() {
		t.Fatal("explicit true should enable etcd")
	}
}

// TestLoadYAML_EtcdEnabledFalse 从 yaml 文本解析 enabled: false。
// uconfig.Load 需要文件，这里直接用 yaml 语义的等价构造验证零值路径。
func TestApplyRuntimeDefaults_DoesNotTouchEtcdEnabled(t *testing.T) {
	f := false
	c := &Config{}
	c.Etcd.Enabled = &f
	c.HttpPort = 8081
	c.Belong = "b"
	c.ServerID = "s"
	c.ServerType = "Gateway"
	c.Zone = "z"
	c.Transports = []Transport{{Protocol: "tcp", Port: 1}}
	c.ApplyRuntimeDefaults()
	if c.Etcd.IsEnabled() {
		t.Fatal("ApplyRuntimeDefaults must not override explicit etcd.enabled=false")
	}
}
