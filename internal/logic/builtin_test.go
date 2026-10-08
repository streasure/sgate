package logic

import (
	"testing"

	protocol "github.com/streasure/protocol/gateway"
)

// TestBuiltinUserOfflineCleansSession 回归：网关 CmdUserOffline 通知曾因
// 未注册处理器被丢弃（"received unregistered cmd"），导致 Server.Offline
// 从不执行，会话/分组/用户映射泄漏，旧会话持续被推送（pushDroppedNoConn 暴涨）。
func TestBuiltinUserOfflineCleansSession(t *testing.T) {
	s := NewServer(WithServerID("logic1"))
	fs := &fakeStream{}
	conn := newStreamConn(fs, 4, "gw-1", false)
	defer conn.Close()

	const sessionID = "conn_test_1"
	const userUUID = "user-1"

	s.sessions.Store(sessionID, conn)
	conn.bindSession(sessionID)
	s.RegisterUser(userUUID, sessionID)
	if got := s.JoinGroup("bench_group", sessionID); got != 1 {
		t.Fatalf("group members = %d, want 1", got)
	}

	s.dispatchMessage(&protocol.StreamData{
		Cmd:       1100012,
		SessionId: sessionID,
		UserKey:   userUUID,
	}, func(*protocol.StreamData) {})

	if _, ok := s.sessions.Load(sessionID); ok {
		t.Fatal("session not removed after CmdUserOffline")
	}
	if n := s.GetGroupCount("bench_group"); n != 0 {
		t.Fatalf("group members = %d, want 0", n)
	}
	if _, ok := s.GetConnectionIDByUser(userUUID); ok {
		t.Fatal("user mapping not removed after CmdUserOffline")
	}
}

// TestBuiltinHeartbeatConsumed 验证网关健康检查心跳 cmd=1100010 被内置消费，
// 不产生 "received unregistered cmd" 告警（即不再走 Warn 分支）。
func TestBuiltinHeartbeatConsumed(t *testing.T) {
	s := NewServer(WithServerID("logic1"))
	if !s.builtinCommand(&protocol.StreamData{Cmd: 1100010}) {
		t.Fatal("heartbeat cmd should be consumed by builtin handler")
	}
	if !s.builtinCommand(&protocol.StreamData{Cmd: 1100012, SessionId: "s1"}) {
		t.Fatal("offline cmd should be consumed by builtin handler")
	}
	if s.builtinCommand(&protocol.StreamData{Cmd: 1234567}) {
		t.Fatal("unknown cmd should not be consumed")
	}
}
