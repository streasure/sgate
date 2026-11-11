package connection

import (
	"strings"
	"testing"
)

// withGatewayID 在 fn 执行期间设置网关身份，结束后恢复原值（包级 atomic.Value）。
func withGatewayID(t *testing.T, id string) {
	t.Helper()
	prev := currentSessionGatewayID()
	SetSessionGatewayID(id)
	t.Cleanup(func() { SetSessionGatewayID(prev) })
}

// TestGenerateConnectionID_EmbedsGatewayIdentity 验证 ID 段嵌入网关身份，
// 且不同 serverID 生成的 ID 必不相同（跨网关重号修复，roadmap P3-1）。
func TestGenerateConnectionID_EmbedsGatewayIdentity(t *testing.T) {
	withGatewayID(t, "gateway-a")
	a := GenerateConnectionID()

	withGatewayID(t, "gateway-b")
	b := GenerateConnectionID()

	if !strings.HasPrefix(a, "conn_gateway-a_") {
		t.Fatalf("a=%q want prefix conn_gateway-a_", a)
	}
	if !strings.HasPrefix(b, "conn_gateway-b_") {
		t.Fatalf("b=%q want prefix conn_gateway-b_", b)
	}
	if a == b {
		t.Fatalf("IDs from different gateways collided: %q", a)
	}
}

// TestGenerateConnectionID_UniqueInProcess 验证进程内序号段不重号。
func TestGenerateConnectionID_UniqueInProcess(t *testing.T) {
	withGatewayID(t, "gw-1")
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		id := GenerateConnectionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestParseSessionGateway_Roundtrip 验证生成→解析往返一致。
func TestParseSessionGateway_Roundtrip(t *testing.T) {
	cases := []string{
		"gateway-1",   // 普通
		"gw_1",        // serverID 含下划线
		"node-007",    // 含数字结尾
		"a_1_2",       // 多段下划线
		"sgate-zh-01", // 带 zone 风格
	}
	for _, gw := range cases {
		withGatewayID(t, gw)
		id := GenerateConnectionID()
		got := ParseSessionGateway(id)
		if got != gw {
			t.Fatalf("ParseSessionGateway(%q)=%q want %q", id, got, gw)
		}
	}
}

// TestParseSessionGateway_OldFormatAndInvalid 验证旧格式与非法输入返回空不 panic。
func TestParseSessionGateway_OldFormatAndInvalid(t *testing.T) {
	cases := []string{
		"conn_1700000000000000000_42", // 旧格式：ts、counter 之间无 gateway 段
		"conn_42",                     // 只有一段
		"conn_",                       // 空尾
		"conn_gw_abc_42",              // ts 段非纯数字
		"conn_gw_123_x",               // counter 段非纯数字
		"conn_gw_123_",                // counter 段为空
		"temp_1700000000000000000_1",  // 非 conn_ 前缀
		"",                            // 空串
		"conn__123_45",                // gateway 段为空（非生成路径，非法）
	}
	for _, id := range cases {
		if got := ParseSessionGateway(id); got != "" {
			t.Fatalf("ParseSessionGateway(%q)=%q want \"\"", id, got)
		}
	}
}

// TestGenerateConnectionID_NoGatewayFallback 验证未设置网关身份时退化旧格式、
// 解析返回空（测试/工具进程零破坏兼容，决策点 4）。
func TestGenerateConnectionID_NoGatewayFallback(t *testing.T) {
	withGatewayID(t, "")
	id := GenerateConnectionID()
	if !strings.HasPrefix(id, "conn_") {
		t.Fatalf("id=%q want conn_ prefix", id)
	}
	// 退化格式仍有 ts_counter 两段纯数字 → 无 gateway 段可解析
	if got := ParseSessionGateway(id); got != "" {
		t.Fatalf("ParseSessionGateway(%q)=%q want \"\" for legacy format", id, got)
	}
	// 旧格式字面量同样返回空
	if got := ParseSessionGateway("conn_1700000000000000000_1"); got != "" {
		t.Fatalf("legacy literal parse=%q want \"\"", got)
	}
}
