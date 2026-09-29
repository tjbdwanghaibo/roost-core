package versionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	driver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

func TestConditionalDeleteProtectsRecreatedIdentity(t *testing.T) {
	run := func(t *testing.T, store interface {
		Store[string, string]
		ConditionalDeleter[string, string]
	}) {
		ctx := context.Background()
		a, _, err := store.Create(ctx, "owner", "run-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "owner", a); err != nil {
			t.Fatal(err)
		}
		b, _, err := store.Create(ctx, "owner", "run-b")
		if err != nil {
			t.Fatal(err)
		}
		if b.Version != a.Version {
			t.Fatalf("fixture did not recreate the ABA: %d/%d", a.Version, b.Version)
		}
		err = store.DeleteIf(ctx, "owner", a, func(current string) bool { return current == a.Value })
		if !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("old identity delete: %v", err)
		}
		got, found, err := store.Get(ctx, "owner")
		if err != nil || !found || got.Value != "run-b" {
			t.Fatalf("new holder lost: %+v %v %v", got, found, err)
		}
		if err := store.DeleteIf(ctx, "owner", b, func(current string) bool { return current == b.Value }); err != nil {
			t.Fatal(err)
		}
		if _, found, err := store.Get(ctx, "owner"); found || err != nil {
			t.Fatal(found, err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryStore[string, string]()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("ROOST_REVIEW_REDIS")
		if addr == "" {
			t.Skip("set ROOST_REVIEW_REDIS for isolated Redis")
		}
		client, err := driver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		prefix := fmt.Sprintf("delete-regression:%d", time.Now().UnixNano())
		store, err := NewRedisStore(client, RedisConfig[string, string]{Prefix: prefix, KeyOf: func(k string) string { return k }, Codec: JSONCodec[string]{}, Index: &RedisIndex[string]{Key: prefix + ":pending", Entry: func(string) (float64, bool) { return 0, true }}})
		if err != nil {
			t.Fatal(err)
		}
		run(t, store)
		if size, err := client.ZCard(context.Background(), prefix+":pending"); err != nil || size != 0 {
			t.Fatalf("deleted value left an index member: size=%d err=%v", size, err)
		}
	})
}
