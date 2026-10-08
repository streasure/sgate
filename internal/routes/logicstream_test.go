package routes

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

// incomingFromOutgoing 模拟服务端：把出站元数据转放入入站上下文。
func incomingFromOutgoing(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("expected outgoing metadata")
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestLogicStreamMetadataRoundtrip(t *testing.T) {
	ctx := AppendLogicStreamMetadata(context.Background(), LogicStreamMeta{
		LogicID: "logic1-tcp", Zone: "default", Addr: "10.0.0.5:50052",
		ShardIdx: 7, ShardCount: 96,
	})
	got, ok := LogicStreamMetadataFromContext(incomingFromOutgoing(t, ctx))
	if !ok {
		t.Fatal("expected metadata to parse")
	}
	if got.LogicID != "logic1-tcp" || got.Zone != "default" || got.Addr != "10.0.0.5:50052" || got.ShardIdx != 7 || got.ShardCount != 96 {
		t.Fatalf("unexpected metadata: %+v", got)
	}
}

func TestLogicStreamMetadataRejects(t *testing.T) {
	cases := []struct {
		name string
		meta LogicStreamMeta
	}{
		{"missing id", LogicStreamMeta{Zone: "default", ShardIdx: 0, ShardCount: 4}},
		{"negative idx", LogicStreamMeta{LogicID: "l", ShardIdx: -1, ShardCount: 4}},
		{"zero count", LogicStreamMeta{LogicID: "l", ShardIdx: 0, ShardCount: 0}},
		{"idx out of range", LogicStreamMeta{LogicID: "l", ShardIdx: 4, ShardCount: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := AppendLogicStreamMetadata(context.Background(), tc.meta)
			if _, ok := LogicStreamMetadataFromContext(incomingFromOutgoing(t, ctx)); ok {
				t.Fatal("expected reject")
			}
		})
	}

	// 无任何元数据 → 旧模式回落。
	if _, ok := LogicStreamMetadataFromContext(context.Background()); ok {
		t.Fatal("expected reject for empty context")
	}
}
