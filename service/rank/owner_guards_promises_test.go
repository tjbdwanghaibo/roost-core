package rank

import (
	"context"
	"errors"
	"testing"
)

// U-0150 · C2 · gap map kit `service/rank` 5/20：Remove / Rank 拒绝 owner 0；无成员记录的条目查询报 ErrNotFound。
// `redis_store.go:211 / 224`（Lua 返回形状不对）需要能篡改脚本返回值的替身，留待。
func TestOwnerZeroAndMissingMembersAreRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	if err := store.Remove(ctx, arena(), 0); !errors.Is(err, ErrOwnerInvalid) {
		t.Fatalf("Remove(owner 0) = %v", err)
	}
	if _, _, err := store.Rank(ctx, arena(), 0); !errors.Is(err, ErrOwnerInvalid) {
		t.Fatalf("Rank(owner 0) = %v", err)
	}
	if _, err := store.entryFor(ctx, "test:rank:z", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entryFor without a member = %v", err)
	}
}
