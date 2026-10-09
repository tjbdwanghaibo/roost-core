// RR-20261004-NC-21：Lua 结果未知必须保留错误，不得重放覆盖另一完成的写。
package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

type unknownWriteRedis struct {
	fredis.IRedis
	afterApply  func() error
	beforeError error
	evals       int
	fallbacks   int
}

func (r *unknownWriteRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	r.evals++
	if r.beforeError != nil {
		return nil, r.beforeError
	}
	value, err := r.IRedis.Eval(ctx, script, keys, args...)
	if err != nil {
		return value, err
	}
	if r.afterApply != nil {
		hook := r.afterApply
		r.afterApply = nil
		if err := hook(); err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	}
	return value, nil
}

func (r *unknownWriteRedis) Pipeline() fredis.IPipeline {
	if r.evals > 0 {
		r.fallbacks++
	}
	return r.IRedis.Pipeline()
}

func (r *unknownWriteRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	r.fallbacks++
	return r.IRedis.Del(ctx, keys...)
}

func TestRefHMapWriteErrorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		apply bool
	}{
		{"before_deadline", context.DeadlineExceeded, false}, {"before_cancel", context.Canceled, false},
		{"wrapped_cause", fmt.Errorf("transport: %w", context.DeadlineExceeded), false},
		{"applied_reply_lost", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newRefHMapFakeRedis()
			cfg := RefHMapConfig[int64, refHMapSession]{Name: "session", StoreConfig: refHMapSessionConfig()}
			authority := NewRedisRefHMapStore(fake, cfg)
			if err := authority.Set(context.Background(), refHMapSession{ID: 1, Version: 1}); err != nil {
				t.Fatal(err)
			}
			wrapped := &unknownWriteRedis{IRedis: fake, beforeError: tc.cause}
			if tc.apply {
				wrapped.beforeError = nil
				wrapped.afterApply = func() error { return nil }
			}
			err := NewRedisRefHMapStore(wrapped, cfg).Set(context.Background(), refHMapSession{ID: 1, Version: 2})
			if !errors.Is(err, tc.cause) || wrapped.evals != 1 || wrapped.fallbacks != 0 {
				t.Fatalf("err=%v evals=%d fallbacks=%d", err, wrapped.evals, wrapped.fallbacks)
			}
			got, held, err := authority.Get(context.Background(), 1)
			want := uint64(1)
			if tc.apply {
				want = 2
			}
			if err != nil || !held || got.Version != want {
				t.Fatalf("stored=%+v held=%v err=%v", got, held, err)
			}
		})
	}
}

func TestRefHMapUnknownWrite(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, fault := range []bool{false, true} {
		name := "normal_control"
		if fault {
			name = "applied_reply_lost_newer_write"
		}
		t.Run(name, func(t *testing.T) {
			prefix := "roost:unknownwrite:" + name
			cfg := RefHMapConfig[int64, refHMapSession]{Prefix: prefix, Name: "session", StoreConfig: refHMapSessionConfig()}
			authority := NewRedisRefHMapStore(client, cfg)
			t.Cleanup(func() { _ = authority.Delete(context.Background(), 1) })
			if err := authority.Set(ctx, refHMapSession{ID: 1, Version: 1}); err != nil {
				t.Fatal(err)
			}
			wrapped := &unknownWriteRedis{IRedis: client}
			if fault {
				wrapped.afterApply = func() error { return authority.Set(ctx, refHMapSession{ID: 1, Version: 3}) }
			}
			writer := NewRedisRefHMapStore(wrapped, cfg)
			err := writer.Set(ctx, refHMapSession{ID: 1, Version: 2})
			got, held, readErr := authority.Get(ctx, 1)
			if readErr != nil || !held {
				t.Fatalf("read: %+v %v %v", got, held, readErr)
			}
			if fault {
				t.Logf("ack lost after v2; another writer stored v3; Set err=%v final=%d", err, got.Version)
				if !errors.Is(err, context.DeadlineExceeded) || got.Version != 3 {
					t.Fatal("unknown Lua outcome was replayed over a newer authoritative write")
				}
			} else if err != nil || got.Version != 2 {
				t.Fatalf("normal write: %v %+v", err, got)
			}
		})
	}
}
