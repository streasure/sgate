package logiclink

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/dataplane"
	"google.golang.org/protobuf/proto"
)

// captureConn 捕获 AsyncWritev 写入帧（TCP 路径 Connection.Send 走 AsyncWritev）。
type captureConn struct {
	gnet.Conn
	mu     sync.Mutex
	frames [][]byte
}

func (c *captureConn) RemoteAddr() net.Addr { return nil }

func (c *captureConn) AsyncWritev(bs [][]byte, _ gnet.AsyncCallback) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var joined []byte
	for _, b := range bs {
		joined = append(joined, b...)
	}
	c.frames = append(c.frames, joined)
	return nil
}

func (c *captureConn) frameCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// addGroupMember 建连并完成 group 成员所需的全部映射：
// 连接 → serverID → serverUser 映射 → group 成员。
func addGroupMember(t *testing.T, cm *dataplane.ConnectionManager, groupID, serverID, userUUID string) (*captureConn, string) {
	t.Helper()
	cc := &captureConn{}
	id := cm.AddConnection(cc, userUUID, false)
	cm.SetConnectionServerID(id, serverID)
	cm.UpdateConnectionUserUUID(id, userUUID)
	cm.AddUserToGroup(groupID, serverID, userUUID)
	return cc, id
}

// TestBroadcastGroup_ParallelSendsToAllMembers 回归：broadcastGroup 分片并发后
// 仍向全组成员各发送一帧，且帧内容与 encodePushMessage 一致。
func TestBroadcastGroup_ParallelSendsToAllMembers(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")

	const members = 100 // 超过 32 worker 单 chunk，覆盖多分片路径
	conns := make([]*captureConn, 0, members)
	for i := range members {
		cc, _ := addGroupMember(t, gw.cm, "g1", "srv-1", "user-"+strconv.Itoa(i))
		conns = append(conns, cc)
	}

	sent, err := srv.Broadcast(context.Background(), &protoGw.BroadcastReq{
		GroupId: []string{"g1"},
		Cmd:     42,
		Data:    []byte("payload"),
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if sent == nil {
		t.Fatal("nil ack")
	}

	want, err := encodePushMessage(42, []byte("payload"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i, cc := range conns {
		if cc.frameCount() != 1 {
			t.Fatalf("conn %d frames=%d, want 1", i, cc.frameCount())
		}
		cc.mu.Lock()
		frame := cc.frames[0]
		cc.mu.Unlock()
		// TCP 帧 = 4B 大端长度前缀 + MessageFrame
		if len(frame) < 4 {
			t.Fatalf("conn %d frame too short: %d", i, len(frame))
		}
		body := frame[4:]
		if string(body) != string(want) {
			var mf protoGw.MessageFrame
			if err := proto.Unmarshal(body, &mf); err != nil {
				t.Fatalf("conn %d unmarshal: %v", i, err)
			}
			t.Fatalf("conn %d frame mismatch cmd=%d body=%q", i, mf.Cmd, mf.Body)
		}
	}
}

// TestBroadcastGroup_PartialFailureCountsFailed 回归：组内 session 已消失时
// 计入 failed，已连接成员仍收到消息（部分成功不整体失败）。
//
// 孤儿 session 构造：serverUserConnections 以 {serverID,userUUID} 为键，
// RemoveConnection 只按「当前」serverID 摘键 —— 先建映射再改 conn 的 serverID，
// 旧键与 group 成员都保留，但 connections 中已无该连接（解析时 conn==nil）。
func TestBroadcastGroup_PartialFailureCountsFailed(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")

	// 孤儿成员：建连 → srv-1 建 serverUser/group 映射 → 改挂 srv-2 → 摘连接
	orphan, id := addGroupMember(t, gw.cm, "g2", "srv-1", "user-orphan")
	gw.cm.GetConnection(id).SetServerID("srv-2")
	gw.cm.RemoveConnection(id)

	// 存活成员
	live, _ := addGroupMember(t, gw.cm, "g2", "srv-1", "user-live")

	sent, err := srv.Broadcast(context.Background(), &protoGw.BroadcastReq{
		GroupId: []string{"g2"},
		Cmd:     7,
		Data:    []byte("x"),
	})
	if err != nil {
		t.Fatalf("partial broadcast should not hard-fail: %v", err)
	}
	if sent == nil {
		t.Fatal("nil ack")
	}
	if orphan.frameCount() != 0 {
		t.Fatalf("removed conn received %d frames, want 0", orphan.frameCount())
	}
	if live.frameCount() != 1 {
		t.Fatalf("live conn frames=%d, want 1", live.frameCount())
	}
}
