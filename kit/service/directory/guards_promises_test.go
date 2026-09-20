package directory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// U-0150 · C2 · gap map kit `service/directory` 7/15：Redis 状态缺前缀 / 带 ttl 不能构造；Commit 拒绝空键、
// 空令牌、从未预留的键；Cancel 拒绝空键；Release 拒绝空所有者。
func TestDirectoryRefusesBlankIdentifiersAndUnknownClaims(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisState(nil, "  ", 0); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisState with a blank prefix = %v", err)
	}
	if _, err := NewRedisState(nil, "dir", time.Minute); err == nil || !strings.Contains(err.Error(), "ttl is not supported") {
		t.Fatalf("NewRedisState with a ttl = %v", err)
	}
	dir, _ := newDirectory(t)
	if _, err := dir.Commit(ctx, Claim{Token: "t"}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Commit without a key = %v", err)
	}
	if _, err := dir.Commit(ctx, Claim{Key: "alice"}); !errors.Is(err, ErrClaimNotFound) {
		t.Fatalf("Commit without a token = %v", err)
	}
	if _, err := dir.Commit(ctx, Claim{Key: "alice", Token: "ghost", Owner: "o"}); !errors.Is(err, ErrClaimNotFound) {
		t.Fatalf("Commit of a never-reserved key = %v", err)
	}
	if err := dir.Cancel(ctx, Claim{Token: "t"}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Cancel without a key = %v", err)
	}
	if err := dir.Release(ctx, "alice", ""); !errors.Is(err, ErrOwnerEmpty) {
		t.Fatalf("Release without an owner = %v", err)
	}
}
