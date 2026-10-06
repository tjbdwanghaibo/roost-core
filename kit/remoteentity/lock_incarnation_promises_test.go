package remoteentity

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
)

// O-M6-6：RemoteEntityMod 在 App 单实例锁启用时把锁的身份（app.ModSingletonIncarnation）交给 Remote，取锁 token
// 带上 "~<sid>~" 开头的进程代际，同 sid 重启后第一次取锁即可接管上一代进程留下的锁；未启用单实例锁（没有这个能力）
// 或 Remote 的 sid 不是单实例锁的 sid 时不传，token 是旧格式，照旧按 lock_ttl 等待。

// tokenRecordingRedis 记下取锁脚本收到的 token，并回答“被别人持有”（不触及 Mongo 许可）。
type tokenRecordingRedis struct {
	fredis.IRedis
	mu     sync.Mutex
	tokens []string
}

func (r *tokenRecordingRedis) Eval(_ context.Context, _ string, _ []string, args ...any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) > 0 {
		token, _ := args[0].(string)
		r.tokens = append(r.tokens, token)
	}
	return []any{int64(0), int64(0), int64(0), int64(0)}, nil
}

type unusedMongo struct{ fmongo.IMongo }

func lockTokenAfterProvide(t *testing.T, modSid int32, incarnation *app.SingletonIncarnation) string {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", 1101)
	redis := &tokenRecordingRedis{}
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModRedis, fredis.IRedis(redis)); err != nil {
		t.Fatal(err)
	}
	if incarnation != nil {
		if err := r.Register(mods.ModSingletonIncarnation, *incarnation); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := coreremote.NewBackend(entity.NewManagerAccess(entity.NewEntityManager()), coreremote.NewMongoCommitter(unusedMongo{}, "remote_entity", 1101, 0))
	if err != nil {
		t.Fatal(err)
	}
	mod := NewRemoteEntityMod(modSid, WithBackend(backend))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(r); err != nil {
		t.Fatal(err)
	}
	factory := app.MustLookup[fredis.IVersionedLockFactory](r, mods.ModRedisVLock)
	lock := factory.NewVersionedLock(42, fredis.VersionedLockOptions{Key: "e", TTL: 3e9})
	_ = lock.TryLock(context.Background())
	redis.mu.Lock()
	defer redis.mu.Unlock()
	if len(redis.tokens) != 1 {
		t.Fatalf("lock scripts = %d, want one TryLock", len(redis.tokens))
	}
	return redis.tokens[0]
}

func TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks(t *testing.T) {
	held := &app.SingletonIncarnation{Key: "roost:demo:singleton:game:1101", Sid: 1101, Token: "0123456789abcdef"}
	if token := lockTokenAfterProvide(t, 0, held); !strings.HasPrefix(token, "~1101~") || !strings.Contains(token, "~0123456789abcdef~") {
		t.Fatalf("lock token %q with the singleton held, want the incarnation prefix ~1101~<holder>~0123456789abcdef~", token)
	}
	if token := lockTokenAfterProvide(t, 0, nil); strings.HasPrefix(token, "~") {
		t.Fatalf("lock token %q with the singleton disabled, want the old format (no takeover)", token)
	}
	if token := lockTokenAfterProvide(t, 1102, held); strings.HasPrefix(token, "~") {
		t.Fatalf("lock token %q for a Remote sid other than the singleton's, want the old format (no takeover)", token)
	}
}
