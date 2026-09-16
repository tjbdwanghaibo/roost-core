package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// U-0114 · C2（空洞测试）· nightly gap map kit `service/session` 10/20。
//
// 会话服务的入口守卫：owner 必须为正、运维强制释放必须点名 run、Redis 存储
// 必须有前缀与请求 TTL；幂等账本指向一个已不存在的 run 是 ErrConflict 而不是
// 静默新开一局；Attach / Finish / Leave 对不存在的 run、以及 run 在 Get 与 Update
// 之间消失（resolve、markReleased）都以 ErrRunMissing 拒绝；Get 不存在的 run 是
// (false, nil) 而不是错误。

// vanishingRuns forwards to a real store but, once armed, reports every Update
// after the first `passUpdates` as "run not found" — the run disappeared
// between the caller's Get and its compare-and-set.
type vanishingRuns struct {
	RunStore
	armed       bool
	passUpdates int
	updates     int
}

func (s *vanishingRuns) Update(ctx context.Context, key string, mutate versionstore.Mutate[Run]) (versionstore.Versioned[Run], bool, error) {
	if s.armed {
		s.updates++
		if s.updates > s.passUpdates {
			var zero versionstore.Versioned[Run]
			_, _, err := mutate(Run{}, false)
			return zero, false, err
		}
	}
	return s.RunStore.Update(ctx, key, mutate)
}

func TestEnterAndForceReleaseRejectInvalidIdentifiers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, owner := range []int64{0, -1} {
		if _, err := h.service.Enter(ctx, owner, enterReq("req-1")); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "owner id must be positive") {
			t.Fatalf("Enter(owner=%d) = %v", owner, err)
		}
	}
	if _, err := h.service.ForceRelease(ctx, "   ", "scene", "scene-1", "note"); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "run id is empty") {
		t.Fatalf("ForceRelease with a blank run id = %v", err)
	}
}

func TestRedisStoresRequirePrefixAndRequestTTL(t *testing.T) {
	if _, err := NewRedisStores(nil, RedisConfig{RequestTTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores without prefix = %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := NewRedisStores(nil, RedisConfig{Prefix: "session", RequestTTL: ttl}); err == nil || !strings.Contains(err.Error(), "request ttl must be positive") {
			t.Fatalf("NewRedisStores with ttl %v = %v", ttl, err)
		}
	}
}

func TestLedgerNamingAMissingRunIsAConflictNotANewRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// 账本说请求 req-ghost 属于 owner 7、对应 run "run-ghost"，但那个 run 不存在。
	requests := h.service.cfg.Requests
	if _, created, err := requests.Create(ctx, "req-ghost", LedgerEntry{RequestID: "req-ghost", OwnerID: 7, RunID: "run-ghost"}); err != nil || !created {
		t.Fatalf("seed ledger: created=%v err=%v", created, err)
	}
	_, err := h.service.Enter(ctx, 7, enterReq("req-ghost"))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "names missing run run-ghost") {
		t.Fatalf("Enter on a ledger entry naming a missing run = %v, want ErrConflict", err)
	}
	if _, found, _ := h.service.Current(ctx, 7); found {
		t.Fatal("a conflicting enter still opened a run")
	}
}

func TestOperationsOnMissingRunsAreRunMissing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.Attach(ctx, 1, "run-none", scene("s-1")); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("Attach to a missing run = %v", err)
	}
	if _, err := h.service.Leave(ctx, 1, "run-none", "bye"); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("Leave a missing run = %v", err)
	}
	if _, err := h.service.Finish(ctx, 1, "run-none", StateSucceeded, "done"); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("Finish a missing run = %v", err)
	}
	run, found, err := h.service.Get(ctx, 1, "run-none")
	if err != nil || found || run.ID != "" {
		t.Fatalf("Get a missing run = (%+v, %v, %v), want (zero, false, nil)", run, found, err)
	}
}

func TestRunVanishingBetweenGetAndUpdateIsRunMissing(t *testing.T) {
	ctx := context.Background()
	store := &vanishingRuns{}
	h := newHarness(t, func(cfg *Config) {
		store.RunStore = cfg.Runs
		cfg.Runs = store
	})
	run := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("s-1")); err != nil {
		t.Fatal(err)
	}

	// resolve：Get 看到 run，随后的 compare-and-set 发现它没了。
	store.armed, store.passUpdates, store.updates = true, 0, 0
	if _, err := h.service.Leave(ctx, 1, run.ID, "bye"); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("Leave with the run vanishing before resolve = %v", err)
	}
	if h.releaser.total() != 0 {
		t.Fatal("a run that could not be resolved still released its resources")
	}

	// markReleased：状态迁移写成功、资源也归还了，但标记释放时 run 没了。
	store.passUpdates, store.updates = 1, 0
	if _, err := h.service.Leave(ctx, 1, run.ID, "bye"); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("Leave with the run vanishing before markReleased = %v", err)
	}
	if h.releaser.count("scene", "s-1") != 1 {
		t.Fatalf("scene released %d times, want exactly once before the mark failed", h.releaser.count("scene", "s-1"))
	}
}
