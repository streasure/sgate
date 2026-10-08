package routes

import (
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

// TestStreamBatchRecvReuseSafety 验证复用同一个 StreamBatch 接收多帧时，
// 先前帧内取出的 *StreamData 指针不会被后一帧覆写（接收循环 sb 复用的前提）。
// proto.Unmarshal 默认 Reset 目标消息；若实现改为回收 repeated 元素对象，
// 本测试将失败并要求恢复每帧新分配。
func TestStreamBatchRecvReuseSafety(t *testing.T) {
	encode := func(cmd int32, data string) []byte {
		b, err := proto.Marshal(&protoGw.StreamBatch{Items: []*protoGw.StreamData{
			{Cmd: cmd, Data: []byte(data), SeqId: int64(cmd)},
		}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	sb := &protoGw.StreamBatch{}
	if err := proto.Unmarshal(encode(100, "first"), sb); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	if len(sb.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(sb.Items))
	}
	first := sb.Items[0]

	// 模拟接收循环：Reset 后解析下一帧（gRPC RecvMsg 内部同样走 proto.Unmarshal）
	sb.Reset()
	if err := proto.Unmarshal(encode(200, "second"), sb); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if len(sb.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(sb.Items))
	}
	second := sb.Items[0]

	if first == second {
		t.Fatalf("protobuf recycled element pointer: first=%p second=%p — sb 复用会破坏已入批消息", first, second)
	}
	if first.Cmd != 100 || string(first.Data) != "first" || first.SeqId != 100 {
		t.Fatalf("first message corrupted: cmd=%d data=%q seq=%d", first.Cmd, first.Data, first.SeqId)
	}
	if second.Cmd != 200 || string(second.Data) != "second" {
		t.Fatalf("second message corrupted: cmd=%d data=%q", second.Cmd, second.Data)
	}
}
