// 会话入参与边角守卫：标识与配置、缺失的 run、字段校验、run 创建丢回复可回收、慢 claim 不准入过期 run、准入 sweep 不删已准入的 run。

package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 会话服务的入口守卫：owner 必须为正、运维强制释放必须点名 run、Redis 存储必须有前缀与请求 TTL；幂等账本指向一个已不存在的 run 是
// ErrConflict 而不是静默新开一局；Attach / Finish / Leave 对不存在的 run、以及 run 在 Get 与 Update 之间消失都以 ErrRunMissing 拒绝；
// Get 不存在的 run 是 (false, nil) 而不是错误。

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

func validRun() Run {
	return Run{ID: "run-1", OwnerID: 7, Kind: "dungeon-3", RequestID: "req-1", StartedAtUnix: 100, DeadlineUnix: 200,
		Resources: []Resource{{Kind: "instance", ID: "i-1"}}, Context: map[string]string{"difficulty": "hard"}}
}

func manyContext(n int) map[string]string {
	out := make(map[string]string, n)
	for i := 0; i < n; i++ {
		out[strings.Repeat("k", i+1)] = "v"
	}
	return out
}

// Run.Validate is the shape gate for every run the session service stores;
// the deadline rule is what keeps held resources from being held forever.
// Each rule is pinned by sentinel and message.
func TestRunValidateRefusesEachBrokenField(t *testing.T) {
	if err := validRun().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*Run)
		sentinel error
		text     string
	}{
		{"blank id", func(r *Run) { r.ID = " " }, ErrRunInvalid, "id is empty"},
		{"owner not positive", func(r *Run) { r.OwnerID = 0 }, ErrRunInvalid, "owner id must be positive"},
		{"blank kind", func(r *Run) { r.Kind = "" }, ErrRunInvalid, "kind is empty"},
		{"blank request id", func(r *Run) { r.RequestID = "  " }, ErrRequestInvalid, "an idempotency key is required"},
		{"no deadline", func(r *Run) { r.DeadlineUnix = 0 }, ErrRunInvalid, "a run must have a deadline"},
		{"deadline not after start", func(r *Run) { r.DeadlineUnix = r.StartedAtUnix }, ErrRunInvalid, "is not after the start"},
		{"too much context", func(r *Run) { r.Context = manyContext(MaxContextEntries + 1) }, ErrRunInvalid, "context has 65 entries, limit 64"},
		{"too many resources", func(r *Run) {
			r.Resources = nil
			for i := 0; i <= MaxResourceEntries; i++ {
				r.Resources = append(r.Resources, Resource{Kind: "instance", ID: strings.Repeat("i", i+1)})
			}
		}, ErrRunInvalid, "run holds 9 resources, limit 8"},
		{"resource without kind", func(r *Run) { r.Resources = []Resource{{ID: "i-1"}} }, ErrRunInvalid, "resource kind is empty"},
		{"resource without id", func(r *Run) { r.Resources = []Resource{{Kind: "instance"}} }, ErrRunInvalid, "resource id is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRun()
			tc.mutate(&r)
			err := r.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}

// EnterRequest.validate is the admission gate: without an idempotency key a
// retried enter allocates a second run and a second set of resources.
func TestEnterRequestValidateRefusesEachBrokenField(t *testing.T) {
	good := EnterRequest{Kind: "dungeon-3", RequestID: "req-1"}
	if err := good.validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		request  EnterRequest
		sentinel error
		text     string
	}{
		{"blank kind", EnterRequest{Kind: " ", RequestID: "req-1"}, ErrRunInvalid, "kind is empty"},
		{"blank request id", EnterRequest{Kind: "dungeon-3"}, ErrRequestInvalid, "an idempotency key is required; without one a retry allocates a second run"},
		{"too much context", EnterRequest{Kind: "dungeon-3", RequestID: "req-1", Context: manyContext(MaxContextEntries + 1)}, ErrRunInvalid, "context has 65 entries, limit 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.request.validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}

type releasedClaimBetweenCreateAndRead struct {
	ClaimStore
	first bool
}

type lostRunCreation struct {
	RunStore
	id string
}

func (s *lostRunCreation) Create(ctx context.Context, key string, value Run) (versionstore.Versioned[Run], bool, error) {
	s.id = key
	if _, _, err := s.RunStore.Create(ctx, key, value); err != nil {
		return versionstore.Versioned[Run]{}, false, err
	}
	return versionstore.Versioned[Run]{}, false, versionstore.ErrOutcomeUnknown
}
func (s *lostRunCreation) PendingAdmissions(context.Context, int64, int) ([]string, error) {
	return []string{s.id}, nil
}
func (s *lostRunCreation) DeleteIf(ctx context.Context, id string, expect versionstore.Versioned[Run], match func(Run) bool) error {
	return s.RunStore.(versionstore.ConditionalDeleter[string, Run]).DeleteIf(ctx, id, expect, match)
}

func TestLostRunCreateReplyCanBeReclaimedWithoutOwnerClaim(t *testing.T) {
	var store *lostRunCreation
	h := newHarness(t, func(c *Config) { store = &lostRunCreation{RunStore: c.Runs}; c.Runs = store })
	ctx := context.Background()
	if _, err := h.service.Enter(ctx, 1, enterReq("lost-run")); !errors.Is(err, versionstore.ErrOutcomeUnknown) {
		t.Fatalf("want unknown: %v", err)
	}
	if _, found, err := h.claims.Get(ctx, 1); err != nil || found {
		t.Fatalf("unexpected claim: %v %v", found, err)
	}
	h.clock.advance(time.Hour)
	if _, err := h.service.SweepPending(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(ctx, store.id); err != nil || found {
		t.Fatalf("unreachable unadmitted run leaked: found=%v err=%v", found, err)
	}
}

type slowClaimCreation struct {
	ClaimStore
	delay func()
}

func (s slowClaimCreation) Create(ctx context.Context, key int64, value Claim) (versionstore.Versioned[Claim], bool, error) {
	s.delay()
	return s.ClaimStore.Create(ctx, key, value)
}
func TestSlowClaimCannotAdmitExpiredRun(t *testing.T) {
	h := newHarness(t)
	h.service.cfg.Claims = slowClaimCreation{ClaimStore: h.claims, delay: func() { h.clock.advance(time.Hour) }}
	if run, err := h.service.Enter(context.Background(), 1, enterReq("slow-claim")); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("expired run handed to business: %+v %v", run, err)
	}
}

func (store *releasedClaimBetweenCreateAndRead) Create(ctx context.Context, key int64, value Claim) (versionstore.Versioned[Claim], bool, error) {
	if !store.first {
		store.first = true
		return versionstore.Versioned[Claim]{}, false, nil
	}
	return store.ClaimStore.Create(ctx, key, value)
}
func TestEnterRetakesClaimReleasedBeforeRead(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.Claims = &releasedClaimBetweenCreateAndRead{ClaimStore: cfg.Claims} })
	run, err := h.service.Enter(context.Background(), 1, enterReq("released-race"))
	if err != nil || run.ID == "" {
		t.Fatalf("claim already released but Enter failed: %v", err)
	}
}
func TestRunTTLRejectsSubsecond(t *testing.T) {
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.TTL = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond run TTL accepted despite Unix-second deadline")
	}
}

func TestRunCodecRequiresAdmissionFormat(t *testing.T) {
	codec := runCodec{}
	if _, err := codec.Decode([]byte(`{"id":"old","state":"open"}`)); err == nil {
		t.Fatal("legacy run accepted")
	}
	want := Run{ID: "pending", AdmissionPending: true}
	raw, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.Decode(raw)
	if err != nil || got.ID != want.ID || !got.AdmissionPending {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
func TestAdmissionSweepCannotDeleteAdmittedRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run, err := h.service.Enter(ctx, 1, enterReq("admitted"))
	if err != nil {
		t.Fatal(err)
	}
	h.service.cfg.Runs = &lostRunCreation{RunStore: h.service.cfg.Runs, id: run.ID}
	h.clock.advance(time.Hour)
	if _, err := h.service.SweepPending(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.runs.Get(ctx, run.ID); err != nil || !found {
		t.Fatalf("stale index deleted admitted run: %v %v", found, err)
	}
}

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.Now = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.cfg.Now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}
