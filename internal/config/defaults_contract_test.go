package config

import (
	"testing"

	protocol "github.com/streasure/protocol/gateway"
)

// TestDefaultPreAuthCommands_MatchesProtocol 钉住默认预鉴权命令与 protocol 仓
// wire 常量相等：config 是叶子包（构建期刻意零 protocol 依赖），等值关系只能由
// 契约测试守——协议仓改号而默认值没跟上会静默放行/拒绝 LoginGate。
func TestDefaultPreAuthCommands_MatchesProtocol(t *testing.T) {
	if got := int32(DefaultPreAuthCommands); got != protocol.CmdLoginGate {
		t.Fatalf("DefaultPreAuthCommands = %d, want protocol.CmdLoginGate = %d",
			got, protocol.CmdLoginGate)
	}
}
