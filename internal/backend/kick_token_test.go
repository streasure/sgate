package backend

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestKickTokenOK(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-internal-token", "secret"))

	if !kickTokenOK(ctx, "secret") {
		t.Fatal("matching token should pass")
	}
	if kickTokenOK(ctx, "wrong") {
		t.Fatal("wrong token should fail")
	}
	if kickTokenOK(ctx, "") {
		t.Fatal("empty expected token should fail (kick disabled)")
	}
	if kickTokenOK(context.Background(), "secret") {
		t.Fatal("missing metadata should fail")
	}
}
