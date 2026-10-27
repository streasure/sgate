package routes

import (
	"testing"

	"github.com/streasure/protocol/enums"
)

// TestCmdSymmetryWithEnums 命令常量双表对称守卫：
// routes.Cmd*（sgate 内消费单源）与 protocol/enums（协议权威枚举）是两份独立定义，
// 同一 wire 值此前仅靠人肉对齐——本测试锁定交集数值，任一侧漂移立即失败。
//
// 两侧差异现状（不在守卫范围，改动时请同步评估）：
//   - routes 独有：CmdError=3、CmdLogoutGate/Ack=1000003/4、CmdPushBatch=9000002
//   - enums 独有：CMD_LOGOUT_REQ/ACK=1100003/4、BAN_INFO、PUSH_NOTIFY、
//     ANNOUNCEMENT、CHAT_MSG、HEARTBEAT_ACK
//
// 名称不同但同值：CmdLogicLoginReq ↔ CMD_LOGIN_REQ（1100001）。
func TestCmdSymmetryWithEnums(t *testing.T) {
	cases := []struct {
		name   string
		routes int32
		enums  enums.Cmd
	}{
		{"login_gate_req", CmdLoginGate, enums.Cmd_CMD_LOGIN_GATE_REQ},
		{"login_gate_ack", CmdLoginGateAck, enums.Cmd_CMD_LOGIN_GATE_ACK},
		{"logic_login_req", CmdLogicLoginReq, enums.Cmd_CMD_LOGIN_REQ},
		{"logic_login_ack", CmdLogicLoginAck, enums.Cmd_CMD_LOGIN_ACK},
		{"kick_notify", CmdKickNotify, enums.Cmd_CMD_KICK_NOTIFY},
		{"heartbeat_req", CmdHeartbeatReq, enums.Cmd_CMD_HEARTBEAT_REQ},
		{"user_offline", CmdUserOffline, enums.Cmd_CMD_USER_OFFLINE_NTF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := c.routes, int32(c.enums); got != want {
				t.Errorf("双表数值漂移: routes=%d enums=%d（改一侧须同步另一侧或协议仓）", got, want)
			}
		})
	}
}
