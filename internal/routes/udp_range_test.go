package routes

import (
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
)

// TestIsUDPCmdBoundary 号段区间边界：五点判定（下界前/下界/上界/上界后/远端批量命令）。
// 单边界实现（cmd >= Start）会把 CmdPushBatch=9000002 误判进 UDP，此处锁定区间语义。
func TestIsUDPCmdBoundary(t *testing.T) {
	cases := []struct {
		cmd  int32
		want bool
	}{
		{1199999, false}, // 段前一位
		{1200000, true},  // 下界（含）
		{1299999, true},  // 上界（含）
		{1300000, false}, // 段后一位
		{9000002, false}, // CmdPushBatch：单边界实现的回归点
		{protoGw.CmdPushBatch, false},
	}
	for _, c := range cases {
		if got := IsUDPCmd(c.cmd); got != c.want {
			t.Errorf("IsUDPCmd(%d) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestReliableCmdsOutsideUDPRange lint 守卫：已知可靠命令不得落入 UDP 号段。
// 新增可靠命令时若误分配进 1,200,000-1,299,999，此测试立即失败（可靠消息
// 落进 UDP 号段 = 可能被分流到不可靠通道）。
func TestReliableCmdsOutsideUDPRange(t *testing.T) {
	reliable := map[string]int32{
		"CmdError":         protoGw.CmdError,
		"CmdLoginGate":     protoGw.CmdLoginGate,
		"CmdLoginGateAck":  protoGw.CmdLoginGateAck,
		"CmdLogoutGate":    protoGw.CmdLogoutGate,
		"CmdLogoutGateAck": protoGw.CmdLogoutGateAck,
		"CmdLogicLoginReq": protoGw.CmdLogicLoginReq,
		"CmdLogicLoginAck": protoGw.CmdLogicLoginAck,
		"CmdKickNotify":    protoGw.CmdKickNotify,
		"CmdHeartbeatReq":  protoGw.CmdHeartbeatReq,
		"CmdUserOffline":   protoGw.CmdUserOffline,
		"CmdMsgAck":        protoGw.CmdMsgAck,
		"CmdPushBatch":     protoGw.CmdPushBatch,
	}
	for name, cmd := range reliable {
		if IsUDPCmd(cmd) {
			t.Errorf("%s=%d falls inside UDP range [%d,%d]; reliable commands must stay below CmdUDPRangeStart",
				name, cmd, protoGw.CmdUDPRangeStart, protoGw.CmdUDPRangeEnd)
		}
	}
}

// TestUDPCmdsInsideRange 传输控制命令必须全部落在号段内（全系统只有一条
// IsUDPCmd 规则，例外列表是 bug 温床）。
func TestUDPCmdsInsideRange(t *testing.T) {
	udps := map[string]int32{
		"CmdUdpAssociate":    protoGw.CmdUdpAssociate,
		"CmdUdpAssociateAck": protoGw.CmdUdpAssociateAck,
		"CmdUdpAction":       protoGw.CmdUdpAction,
		"CmdUdpState":        protoGw.CmdUdpState,
		"CmdUdpPing":         protoGw.CmdUdpPing,
		"CmdUdpPong":         protoGw.CmdUdpPong,
	}
	for name, cmd := range udps {
		if !IsUDPCmd(cmd) {
			t.Errorf("%s=%d outside UDP range [%d,%d]", name, cmd, protoGw.CmdUDPRangeStart, protoGw.CmdUDPRangeEnd)
		}
	}
}
