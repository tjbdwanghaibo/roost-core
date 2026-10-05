package entity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// RR-20261005-NC-35：权威加载的身份必须与请求完全一致；错误结果不能写入其他键，
// 也不能将请求键的旧缓存包装成满足 minVersion 的成功结果。拒绝后仍可加载合法结果。
func TestAuthoritativeLoaderRejectsForeignKeyBeforePublish(t *testing.T) {
	key := l2ConflictKey(t, 233, 9535)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		for _, field := range []string{"tenant", "entity", "scope", "policy"} {
			t.Run(fmt.Sprintf("mode%d/%s", mode, field), func(t *testing.T) {
				foreign := key
				switch field {
				case "tenant":
					foreign.Tenant++
				case "entity":
					foreign.EntityID, _ = BuildEntityID(9536, key.Kind)
				case "scope":
					foreign.Scope++
				case "policy":
					foreign.Policy++
				}
				if !foreign.Valid() {
					t.Fatal("fixture foreign key is invalid")
				}
				bad := true
				l2 := newL2Fake()
				c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute}, l2,
					func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
						if bad {
							return snapshotAt(foreign, 4, "foreign"), true, nil
						}
						return snapshotAt(key, 4, "recovered"), true, nil
					})
				if err := c.Publish(context.Background(), snapshotAt(key, 2, "before")); err != nil {
					t.Fatal(err)
				}
				got, found, err := c.Get(context.Background(), key, mode, 4)
				wrong, leaked, l2Err := l2.Get(context.Background(), foreign)
				if err == nil || found || leaked || l2Err != nil {
					t.Fatalf("foreign authority accepted: found=%v version=%d err=%v foreignL2=%v/%d l2err=%v", found, got.StateVersion, err, leaked, wrong.StateVersion, l2Err)
				}
				if _, leaked, _ := c.local.Get(context.Background(), foreign); leaked {
					t.Fatal("foreign result reached L1")
				}
				before, found, err := c.local.Get(context.Background(), key)
				if err != nil || !found || before.StateVersion != 2 || string(before.Payload.BytesCopy()) != "before" {
					t.Fatal("rejection changed requested cache")
				}
				bad = false
				got, found, err = c.Get(context.Background(), key, mode, 4)
				if err != nil || !found || got.Key != key || got.StateVersion != 4 || string(got.Payload.BytesCopy()) != "recovered" {
					t.Fatalf("valid recovery: found=%v snapshot=%+v err=%v", found, got, err)
				}
			})
		}
	}
}

// RR-20261005-NC-36：Publish 可以因较新 epoch 保留旧 L1，但 read 的最终结果仍须
// 满足 minVersion；不能只检查 loader 的原始结果。错误后不得覆盖较新 epoch，合法加载可恢复。
func TestAuthoritativeReadChecksStoredMinimumVersion(t *testing.T) {
	key := l2ConflictKey(t, 233, 9537)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			answer := snapshotAt(key, 4, "old-epoch")
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute}, nil,
				func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
					return answer, true, nil
				})
			current := snapshotAt(key, 2, "new-epoch")
			current.MarkerEpoch, current.RouteEpoch = 2, 2
			if err := c.Publish(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			got, found, err := c.Get(context.Background(), key, mode, 4)
			if !errors.Is(err, ErrRemoteSnapshotStale) || found {
				t.Fatalf("minimum version violated after admission: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
			kept, found, err := c.local.Get(context.Background(), key)
			if err != nil || !found || kept.MarkerEpoch != 2 || kept.StateVersion != 2 || string(kept.Payload.BytesCopy()) != "new-epoch" {
				t.Fatal("older epoch overwrote current cache")
			}
			answer = snapshotAt(key, 4, "recovered")
			answer.MarkerEpoch, answer.RouteEpoch = 2, 2
			got, found, err = c.Get(context.Background(), key, mode, 4)
			if err != nil || !found || got.StateVersion != 4 || string(got.Payload.BytesCopy()) != "recovered" {
				t.Fatalf("valid recovery: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
		})
	}
}

// RR-20260913-08 残余：L2 发布期间跨过信封截止时间，最终读取必须用当前时间再判过期。
// 等待的是信封自己的语义截止时间，不是用任意 sleep 猜测并发顺序。
type deadlineSnapshotL2 struct {
	*l2Fake
	writes int
}

func (s *deadlineSnapshotL2) Set(ctx context.Context, value RemoteSnapshotEnvelope) error {
	s.writes++
	if value.ExpiresAt > 0 {
		timer := time.NewTimer(time.Until(time.Unix(0, value.ExpiresAt)))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.l2Fake.Set(ctx, value)
}

func TestAuthoritativeReadRechecksExpiryAfterL2Publish(t *testing.T) {
	key := l2ConflictKey(t, 233, 9538)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			l2 := &deadlineSnapshotL2{l2Fake: newL2Fake()}
			fresh := false
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, LoadTimeout: 3 * time.Second}, l2,
				func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
					if fresh {
						return snapshotAt(key, 5, "fresh"), true, nil
					}
					v := snapshotAt(key, 4, "same")
					v.ExpiresAt = time.Now().Add(250 * time.Millisecond).UnixNano()
					return v, true, nil
				})
			got, found, err := c.Get(context.Background(), key, mode, 4)
			if l2.writes != 1 {
				t.Fatalf("fixture did not cross L2 write: writes=%d", l2.writes)
			}
			if err != nil || found {
				t.Fatalf("expired during publish was served: found=%v expired=%v err=%v", found, got.Expired(time.Now()), err)
			}
			// 合法恢复使用较新版本，避免混入“同版本零有效期刷新”的另一个契约。
			fresh = true
			got, found, err = c.Get(context.Background(), key, mode, 5)
			if err != nil || !found || got.StateVersion != 5 || got.Expired(time.Now()) {
				t.Fatalf("expiry recovery: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
		})
	}
}
