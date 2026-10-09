package remoteentity

// O-M6-6（维护者决定第十一轮，docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §7）：owner 被 SIGKILL 后同 sid
// 重启，持有 App 单实例锁的新进程第一次取某个实体的共享锁时，当场接管上一代进程留下的锁，不再等 lock_ttl。
// 修前（锁 token 里没有 sid 与进程代际）新进程只能等旧租约过期：锁的重试预算用完报 versioned lock not acquired，
// Request 先到截止就是结果未知。只接管“同一单实例锁持有者、旧代际”的锁：别的 sid / 服务类型、本代自己的、
// 旧格式的、未启用单实例锁（没有 Incarnation）的一律按 TTL。接管等价于旧租约此刻过期：fence 递增、Mongo
// 许可换代，上一代的许可、续期与释放都失效。
//
// 这里的 Redis 是按 versioned_lock_lua.go 逐行写的内存替身；同一组判定在真实 Redis 上的验证见
// lock_takeover_integration_test.go（scripts/mirror-local.sh test-core）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// takeoverRedis 按 versionedTryLockLua / Refresh / Touch / Unlock / Abandon 的语义在内存里模拟锁 hash 与 fence 计数器。
type takeoverRedis struct {
	fredis.IRedis
	mu       sync.Mutex
	hashes   map[string]map[string]string
	fences   map[string]int64
	tryLocks int
}

func newTakeoverRedis() *takeoverRedis {
	return &takeoverRedis{hashes: map[string]map[string]string{}, fences: map[string]int64{}}
}

func argString(args []any, i int) string {
	if i >= len(args) {
		return ""
	}
	switch v := args[i].(type) {
	case string:
		return v
	case uint64:
		return strconv.FormatUint(v, 10)
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return ""
}

func (r *takeoverRedis) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hashes[keys[0]]
	if h == nil {
		h = map[string]string{}
		r.hashes[keys[0]] = h
	}
	owner, held := h["owner"]
	token := argString(args, 0)
	switch script {
	case versionedTryLockLua:
		r.tryLocks++
		earlier, takeover := false, false
		if prefix := argString(args, 2); held && prefix != "" && strings.HasPrefix(owner, prefix) {
			seq, err := strconv.ParseUint(strings.TrimPrefix(owner, prefix), 10, 64)
			current, _ := strconv.ParseUint(argString(args, 3), 10, 64)
			earlier = err == nil && seq < current
		}
		if scope, self := argString(args, 4), argString(args, 5); held && !earlier && scope != "" && strings.HasPrefix(owner, scope) && !strings.HasPrefix(owner, self) {
			takeover = true
		}
		if held && !earlier && !takeover {
			return []any{int64(0), int64(0), int64(0), int64(0)}, nil
		}
		r.fences[keys[1]]++
		h["owner"] = token
		version := h["version"]
		if version == "" {
			version = "0"
		}
		flag := int64(0)
		if takeover {
			flag = 1
		}
		return []any{int64(1), version, strconv.FormatInt(r.fences[keys[1]], 10), flag}, nil
	case versionedRefreshLua:
		if held && owner == token {
			return int64(1), nil
		}
		return int64(0), nil
	case versionedTouchLua:
		if !held || owner != token {
			return int64(-1), nil
		}
		return int64(1000), nil
	case versionedUnlockLua:
		if held && owner == token {
			h["version"], h["last_unlock"] = argString(args, 2), argString(args, 1)
			delete(h, "owner")
			return int64(1), nil
		}
		if h["last_unlock"] == argString(args, 1) {
			return int64(2), nil
		}
		return int64(0), nil
	case versionedAbandonLua:
		if held && owner == token {
			delete(h, "owner")
			return int64(1), nil
		}
		return int64(0), nil
	}
	return nil, errors.New("takeoverRedis: unexpected script")
}

// takeoverBackend 让各“进程”的 Assembly 共用同一份 Mongo 权威（同一个部署里的同一个控制库）。
type takeoverBackend struct {
	*atomicTestBackend
	authority WriteAuthority
}

func (b takeoverBackend) WriteAuthority() WriteAuthority { return b.authority }

type takeoverFixture struct {
	t      *testing.T
	redis  *takeoverRedis
	store  *MongoCommitter
	cfg    *Config
	entity int64
}

const takeoverSid int32 = 1101

func newTakeoverFixture(t *testing.T) *takeoverFixture {
	t.Helper()
	ctx := context.Background()
	f := &takeoverFixture{t: t, redis: newTakeoverRedis(), store: NewMongoCommitter(newRemoteMongoFake(), "control", takeoverSid, 0), cfg: DefaultConfig()}
	f.cfg.LockTTL = 3 * time.Second
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	f.entity, _ = entity.BuildEntityID(997, 198)
	lease, err := f.store.ClaimOwnership(ctx, f.entity, takeoverSid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnterSharedExpected(ctx, f.entity, lease); err != nil {
		t.Fatal(err)
	}
	return f
}

// process 模拟一个进程：自己的 Assembly（新的锁工厂）与这个实体的一把锁（不开异步续期：SIGKILL 之后没人续期）。
func (f *takeoverFixture) process(sid int32, incarnation *ProcessIncarnation) *versionedLock {
	f.t.Helper()
	asm, err := Assemble(AssemblyDeps{Redis: f.redis, Backend: takeoverBackend{&atomicTestBackend{newRemoteTestLoader()}, f.store}, Incarnation: incarnation}, f.cfg, sid, MongoBackendConfig{})
	if err != nil {
		f.t.Fatal(err)
	}
	return asm.LockFactory.NewVersionedLock(f.entity, fredis.VersionedLockOptions{Key: f.cfg.LockKey, TTL: f.cfg.LockTTL, RetryCount: 3, RetryInterval: time.Millisecond}).(*versionedLock)
}

func (f *takeoverFixture) tryLocks() int {
	f.redis.mu.Lock()
	defer f.redis.mu.Unlock()
	return f.redis.tryLocks
}

func gameSingleton(sid int32, token string) *ProcessIncarnation {
	return &ProcessIncarnation{Holder: "roost:demo:singleton:game:" + strconv.Itoa(int(sid)), Token: token}
}

func TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)
	old := f.process(takeoverSid, gameSingleton(takeoverSid, "0123456789abcdef"))
	if err := old.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	oldGrant, _ := old.writeGrant()
	oldFence := old.Fence()
	// 旧进程被 SIGKILL：不释放、不续期。同 sid 的新进程拿到单实例锁（新的 token）后第一次取锁。
	restarted := f.process(takeoverSid, gameSingleton(takeoverSid, "fedcba9876543210"))
	before := f.tryLocks()
	if err := restarted.Lock(ctx); err != nil {
		t.Fatalf("the restarted same-sid process's first lock = %v, want it to take over the dead incarnation's lease right away", err)
	}
	if attempts := f.tryLocks() - before; attempts != 1 {
		t.Fatalf("the takeover needed %d lock attempts, want the first one", attempts)
	}
	newGrant, ok := restarted.writeGrant()
	if !ok || restarted.Fence() <= oldFence || newGrant.Fence <= oldGrant.Fence || !newGrant.Ownership.Shared {
		t.Fatalf("takeover fence=%d grant=%+v (held %v), want a newer Redis fence than %d and a newer Mongo grant than %+v", restarted.Fence(), newGrant, ok, oldFence, oldGrant)
	}
	// 上一代的许可、续期与释放都失效，新一代不受影响（K3：接管等价于旧租约此刻过期）。
	if _, err := f.store.CommitRemote(ctx, authorityCommit(t, f.entity, 1, oldGrant)); err == nil {
		t.Fatal("the previous incarnation's grant still committed after the takeover")
	}
	if err := old.Touch(ctx, time.Second); !errors.Is(err, ErrVersionedLockExpired) {
		t.Fatalf("the previous incarnation's touch = %v, want expired", err)
	}
	if err := old.Unlock(ctx, 9, time.Second); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("the previous incarnation's unlock = %v, want not owned", err)
	}
	if !restarted.IsAcquired() {
		t.Fatal("the previous incarnation's touch / unlock released the new one")
	}
	if _, err := f.store.CommitRemote(ctx, authorityCommit(t, f.entity, 2, newGrant)); err != nil {
		t.Fatalf("commit with the new incarnation's grant: %v", err)
	}
	if err := restarted.Unlock(ctx, newGrant.Version+1, time.Second); err != nil {
		t.Fatalf("new incarnation unlock: %v", err)
	}
}

func TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder(t *testing.T) {
	ctx := context.Background()
	const oldToken, newToken = "0123456789abcdef", "fedcba9876543210"
	cases := []struct {
		name      string
		holder    func(f *takeoverFixture) *versionedLock
		contender func(f *takeoverFixture) *versionedLock
	}{
		{"another sid holds the lock", func(f *takeoverFixture) *versionedLock { return f.process(1102, gameSingleton(1102, oldToken)) },
			func(f *takeoverFixture) *versionedLock {
				return f.process(takeoverSid, gameSingleton(takeoverSid, newToken))
			}},
		{"another server type with the same sid", func(f *takeoverFixture) *versionedLock {
			return f.process(takeoverSid, &ProcessIncarnation{Holder: "roost:demo:singleton:match:1101", Token: oldToken})
		}, func(f *takeoverFixture) *versionedLock {
			return f.process(takeoverSid, gameSingleton(takeoverSid, newToken))
		}},
		{"singleton disabled on the restarted process", func(f *takeoverFixture) *versionedLock {
			return f.process(takeoverSid, gameSingleton(takeoverSid, oldToken))
		},
			func(f *takeoverFixture) *versionedLock { return f.process(takeoverSid, nil) }},
		{"old-format token from a process without an incarnation", func(f *takeoverFixture) *versionedLock { return f.process(takeoverSid, nil) },
			func(f *takeoverFixture) *versionedLock {
				return f.process(takeoverSid, gameSingleton(takeoverSid, newToken))
			}},
		{"same incarnation, another lock object", func(f *takeoverFixture) *versionedLock {
			return f.process(takeoverSid, gameSingleton(takeoverSid, newToken))
		},
			func(f *takeoverFixture) *versionedLock {
				return f.process(takeoverSid, gameSingleton(takeoverSid, newToken))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTakeoverFixture(t)
			holder := tc.holder(f)
			if err := holder.Lock(ctx); err != nil {
				t.Fatal(err)
			}
			contender := tc.contender(f)
			if err := contender.TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
				t.Fatalf("contender TryLock = %v, want not acquired: the lease is not the previous incarnation of the same singleton holder", err)
			}
			if err := holder.Touch(ctx, time.Second); err != nil {
				t.Fatalf("the holder lost its lease to a contender that must not take over: %v", err)
			}
		})
	}
}

func TestAssembleRejectsAMalformedIncarnation(t *testing.T) {
	f := newTakeoverFixture(t)
	for _, incarnation := range []*ProcessIncarnation{
		{Holder: "", Token: "abc"},
		{Holder: "k", Token: ""},
		{Holder: "k", Token: "a~b"},
		{Holder: "k", Token: "a.b"},
		{Holder: "k", Token: strings.Repeat("a", 65)},
	} {
		_, err := Assemble(AssemblyDeps{Redis: f.redis, Backend: takeoverBackend{&atomicTestBackend{newRemoteTestLoader()}, f.store}, Incarnation: incarnation}, f.cfg, takeoverSid, MongoBackendConfig{})
		if !errors.Is(err, ErrVersionedLockConfig) {
			t.Fatalf("Assemble with incarnation %+v = %v, want ErrVersionedLockConfig", incarnation, err)
		}
	}
}
