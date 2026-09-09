package failurelog

import (
	"context"
	"errors"
	"testing"
)

// U-0147 · C2 · nightly gap map core `failurelog` 5/5：每个操作对空 key 报 ErrKeyEmpty，
// 不把空 key 当成一个真的列表去碰 Redis。
func TestRedisListRefusesAnEmptyKeyOnEveryOperation(t *testing.T) {
	ctx := context.Background()
	list := NewRedisList(newFakeRedis(), Config{Namespace: "test"})
	if err := list.AppendRaw(ctx, "", []byte("x")); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("AppendRaw = %v", err)
	}
	if _, err := list.ListRaw(ctx, "", 0, -1); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("ListRaw = %v", err)
	}
	if _, err := list.Purge(ctx, ""); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Purge = %v", err)
	}
	if _, err := list.CountRaw(ctx, ""); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("CountRaw = %v", err)
	}
	if _, err := list.DeleteRaw(ctx, "", [][]byte{[]byte("x")}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("DeleteRaw = %v", err)
	}
}
