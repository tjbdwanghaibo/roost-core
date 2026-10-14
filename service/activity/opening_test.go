// 活动开窗：后到的创建不借用已承诺的名额、丢回复后恢复原计划、畸形 opening（含无计划条目）的隔离与运维移除、超限窗口拒绝、开窗与 sweep 交错不丢窗口索引。

package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func openingService(t *testing.T, mutate func(*Config)) (*Service, *activityClock) {
	t.Helper()
	return newActivityService(t, func(c *Config) {
		if os.Getenv("ROOST_BUGFIX5_BACKEND") == "redis" {
			addr := os.Getenv("ROOST_REVIEW_REDIS")
			if addr == "" {
				t.Fatal("isolated Redis address required")
			}
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			stores, err := NewRedisStores(client, redistest.Prefix(t, addr, "bugfix5:activity"), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			c.Activities = stores.Activities
			c.Participants = stores.Participants
			c.Ledger = stores.Ledger
			c.Audits = stores.Audits
			c.Dispatches = stores.Dispatches
			c.Windows = stores.Windows
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

type pausedActivityCreate struct {
	versionstore.Store[Key, Activity]
	once             atomic.Bool
	key              Key
	entered, release chan struct{}
}

func (s *pausedActivityCreate) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	if key == s.key && s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Create(ctx, key, value)
}

func TestLateCreateCannotBorrowAnAlreadyPromisedSlot(t *testing.T) {
	key := activityKey("zz-late")
	var gate *pausedActivityCreate
	s, clock := openingService(t, func(c *Config) {
		gate = &pausedActivityCreate{Store: c.Activities, key: key, entered: make(chan struct{}), release: make(chan struct{})}
		c.Activities = gate
	})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { _, err := s.OpenActivity(ctx, key, []int32{1, 2}); done <- err }()
	<-gate.entered
	released := false
	defer func() {
		if !released {
			close(gate.release)
			<-done
		}
	}()
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPendingActivities-1; i++ {
		openActivity(t, s, activityKey(fmt.Sprintf("new-%d", i)), 1)
	}
	if _, err := s.OpenActivity(ctx, activityKey("one-too-many"), []int32{1}); !errors.Is(err, ErrBacklog) {
		t.Fatal(err)
	}
	close(gate.release)
	released = true
	if err := <-done; err != nil && !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || window.Value.pending() != MaxPendingActivities {
		t.Fatal(window, err)
	}
	if _, err := s.NotifyPhase(ctx, key, 1); err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.GraceWindow + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LookupActivity(ctx, key)
	if err != nil || !found || got.Status != StatusComplete {
		t.Fatal(got, found, err)
	}
}

type lostReplyWindowStore struct {
	versionstore.Store[string, Window]
	writes atomic.Int32
	failAt int32
}

func (s *lostReplyWindowStore) Update(ctx context.Context, key string, mutate versionstore.Mutate[Window]) (versionstore.Versioned[Window], bool, error) {
	got, applied, err := s.Store.Update(ctx, key, mutate)
	if err == nil && applied && s.writes.Add(1) == s.failAt {
		return versionstore.Versioned[Window]{}, false, errors.New("admission response lost")
	}
	return got, applied, err
}
func TestLostAdmissionReplyRecoversItsOriginalPlan(t *testing.T) {
	s, clock := openingService(t, func(c *Config) { c.Windows = &lostReplyWindowStore{Store: c.Windows, failAt: 1} })
	ctx := context.Background()
	key := activityKey("lost")
	expected := []int32{1, 2}
	if _, err := s.OpenActivity(ctx, key, expected); err == nil {
		t.Fatal("lost reply not reported")
	}
	expected[0] = 999
	window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || len(window.Value.Opening) != 1 || window.Value.Opening[0].Intent == nil {
		t.Fatal(window, err)
	}
	rebuilt, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	got, found, err := rebuilt.LookupActivity(ctx, key)
	if err != nil || !found || len(got.ExpectedGameSIDs) != 2 || got.ExpectedGameSIDs[0] != 1 {
		t.Fatal(got, found, err)
	}
	if _, err := rebuilt.OpenActivity(ctx, key, []int32{3}); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	got, _, err = rebuilt.LookupActivity(ctx, key)
	if err != nil || got.ExpectedGameSIDs[0] != 1 {
		t.Fatal(got, err)
	}
}

type lostReplyActivityCreate struct {
	versionstore.Store[Key, Activity]
	once atomic.Bool
}

func (s *lostReplyActivityCreate) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	stored, created, err := s.Store.Create(ctx, key, value)
	if err == nil && created && s.once.CompareAndSwap(false, true) {
		return versionstore.Versioned[Activity]{}, false, errors.New("creation response lost")
	}
	return stored, created, err
}

func TestCreatedOrConfirmedReplyLossRemainsRecoverable(t *testing.T) {
	for _, failure := range []string{"create", "confirm"} {
		t.Run(failure, func(t *testing.T) {
			s, clock := openingService(t, func(c *Config) {
				if failure == "create" {
					c.Activities = &lostReplyActivityCreate{Store: c.Activities}
				} else {
					c.Windows = &lostReplyWindowStore{Store: c.Windows, failAt: 2}
				}
			})
			ctx := context.Background()
			key := activityKey(failure)
			if _, err := s.OpenActivity(ctx, key, []int32{1, 2}); err == nil {
				t.Fatal("lost response not reported")
			}
			window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || !window.Value.contains(key) || window.Value.pending() != 1 {
				t.Fatal(window, err)
			}
			rebuilt, err := New(s.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := rebuilt.NotifyPhase(ctx, key, 1); err != nil {
				t.Fatal(err)
			}
			clock.advance(s.cfg.GraceWindow + time.Second)
			advanced, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1)
			if err != nil || len(advanced) != 1 || advanced[0].Status != StatusComplete {
				t.Fatal(advanced, err)
			}
			window, _, err = rebuilt.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || window.Value.pending() != 0 || !window.Value.delivering(key) {
				t.Fatal(window, err)
			}
		})
	}
}

type countingActivityReads struct {
	versionstore.Store[Key, Activity]
	reads atomic.Int64
}

func (s *countingActivityReads) Get(ctx context.Context, key Key) (versionstore.Versioned[Activity], bool, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, key)
}

// 准入从不让窗口超过 MaxPendingActivities；超限的窗口（旧版本没有这个上限时写下的形态）在任何读之前被
// 拒绝（ErrConflict），不轮转扫描、不改窗口，交线下修复。
// 历史：此前按 ScanAfter 游标轮转扫描旧的超限窗口；维护者 2026-10-13 决定不兼容旧数据。
func TestAWindowAboveTheLimitIsRefusedBeforeAnyRead(t *testing.T) {
	var store *countingActivityReads
	s, _ := openingService(t, func(c *Config) { store = &countingActivityReads{Store: c.Activities}; c.Activities = store })
	ctx := context.Background()
	window := Window{GroupID: "group-a"}
	for i := 0; i < MaxPendingActivities+1; i++ {
		window.Keys = append(window.Keys, activityKey(fmt.Sprintf("oversized-%03d", i)))
	}
	stored, _, err := s.cfg.Windows.Create(ctx, window.GroupID, window)
	if err != nil {
		t.Fatal(err)
	}
	store.reads.Store(0)
	if _, err := s.AdvanceExpired(ctx, window.GroupID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("AdvanceExpired over a window above the limit = %v, want ErrConflict", err)
	}
	if store.reads.Load() != 0 {
		t.Fatalf("the refused window was still scanned: %d reads", store.reads.Load())
	}
	if after, _, err := s.cfg.Windows.Get(ctx, window.GroupID); err != nil || after.Version != stored.Version {
		t.Fatalf("the refused window was rewritten: %+v %v", after, err)
	}
}

// 没有 Intent 的 Opening 条目（本包从不写出；旧版本在计划出现之前的准入形态）是畸形条目：过 grace 也不回收、
// 名额保留；运维列表列出它（"intent is missing"），同 key Open 以 ErrConflict 拒绝、不补计划；运维移除之后
// 同 key Open 走正常准入。
// 历史：RR-20261001-09 曾在 grace 后回收它、同 key Open 为它补计划；维护者 2026-10-13 决定不兼容旧数据。
func TestAnOpeningWithoutAPlanIsMalformedAndKeptForAnOperator(t *testing.T) {
	s, clock := openingService(t, nil)
	ctx := context.Background()
	key := activityKey("planless-opening")
	if _, _, err := s.cfg.Windows.Create(ctx, key.GroupID, Window{GroupID: key.GroupID, Opening: []OpeningEntry{{Key: key, AdmittedAtUnix: clock.Now().Unix()}}}); err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatalf("a planless entry failed the whole sweep: %v", err)
	}
	if window, _, err := s.cfg.Windows.Get(ctx, key.GroupID); err != nil || window.Value.pending() != 1 {
		t.Fatalf("a planless entry past its grace was reclaimed: %+v %v", window, err)
	}
	problems, err := s.MalformedWindowEntries(ctx, key.GroupID)
	if err != nil || len(problems) != 1 || problems[0].List != WindowOpening || problems[0].Key != key || problems[0].Reason != "intent is missing" {
		t.Fatalf("MalformedWindowEntries = %+v %v, want the planless opening", problems, err)
	}
	if _, err := s.OpenActivity(ctx, key, []int32{1, 2}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-key Open over a planless entry = %v, want ErrConflict (no plan is supplied)", err)
	}
	if removed, err := s.RemoveMalformedWindowEntry(ctx, key.GroupID, WindowOpening, key, "planless opening from an older build"); err != nil || removed != 1 {
		t.Fatalf("RemoveMalformedWindowEntry = %d %v", removed, err)
	}
	if _, err := s.OpenActivity(ctx, key, []int32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if window, _, err := s.cfg.Windows.Get(ctx, key.GroupID); err != nil || len(window.Value.Opening) != 0 || len(window.Value.Keys) != 1 {
		t.Fatal(window, err)
	}
}

// 一条坏 Intent 不能让该 group 的 AdvanceExpired 每 tick 整体失败：坏 Intent → 跳过、每 tick 计
// sweep.opening_intent_malformed、日志只在状态变化时打一次，名额保留；同组其他条目照常推进。
// 后端由 ROOST_REVIEW4_BACKEND 选：默认 Memory；"redis" 时用 ROOST_REVIEW_REDIS 指向的隔离 Redis（前缀 rr09:activity:<nanos>，不 FLUSH）。
// 历史：RR-20261001-09。

func newRR09Service(t *testing.T, mutate ...func(*Config)) (*Service, *activityClock) {
	t.Helper()
	return newActivityService(t, append([]func(*Config){func(c *Config) {
		if os.Getenv("ROOST_REVIEW4_BACKEND") != "redis" {
			return
		}
		addr := os.Getenv("ROOST_REVIEW_REDIS")
		if addr == "" {
			t.Fatal("isolated Redis address required (ROOST_REVIEW_REDIS)")
		}
		client, err := driver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		stores, err := NewRedisStores(client, redistest.Prefix(t, addr, "rr09:activity"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		c.Activities, c.Participants, c.Ledger = stores.Activities, stores.Participants, stores.Ledger
		c.Audits, c.Dispatches, c.Windows = stores.Audits, stores.Dispatches, stores.Windows
	}}, mutate...)...)
}

// countingLogHandler counts slog records whose message contains a substring,
// so a "logged once per state change" promise can be asserted without
// depending on the output format.
type countingLogHandler struct {
	slog.Handler
	mu     sync.Mutex
	counts map[string]int
	needle []string
}

func (h *countingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	for _, n := range h.needle {
		if strings.Contains(r.Message, n) {
			h.counts[n]++
		}
	}
	h.mu.Unlock()
	return nil
}

func (h *countingLogHandler) count(needle string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[needle]
}

func captureSweepLogs(t *testing.T, needles ...string) *countingLogHandler {
	t.Helper()
	h := &countingLogHandler{
		Handler: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}),
		counts:  map[string]int{},
		needle:  needles,
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

func seedOpening(t *testing.T, s *Service, entries ...OpeningEntry) {
	t.Helper()
	groupID := entries[0].Key.GroupID
	_, _, err := s.cfg.Windows.Update(context.Background(), groupID, func(current Window, found bool) (Window, bool, error) {
		next := Window{GroupID: groupID}
		if found {
			next = current.clone()
		}
		next.Opening = append(next.Opening, entries...)
		return next, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func windowOf(t *testing.T, s *Service, groupID string) Window {
	t.Helper()
	window, found, err := s.cfg.Windows.Get(context.Background(), groupID)
	if err != nil || !found {
		t.Fatalf("window %q: found=%v err=%v", groupID, found, err)
	}
	return window.Value
}

func malformedIntent(key Key) *Activity {
	other := key
	other.ActivityID += "-other"
	return &Activity{Key: other, Status: StatusPending, ExpectedGameSIDs: []int32{1}}
}

// 一条坏 Intent + 一条正常到期活动：sweep 后正常活动完成，坏条目被跳过且不让整组失败；坏条目的日志
// 只打一次，修好之后再打一次“恢复”并被 sweep 按计划创建。
func TestSweepSkipsAMalformedIntentWithoutStallingTheGroup(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	s, clock := newRR09Service(t, func(c *Config) { c.Metrics = recorder })
	logs := captureSweepLogs(t, "opening intent is malformed", "opening intent is usable again")
	ctx := context.Background()
	server := &Server{service: s}

	normal := activityKey("rr09-normal")
	bad := activityKey("rr09-bad-intent")
	openActivity(t, s, normal, 1, 2)
	notify(t, s, normal, 1) // collecting, grace deadline now+30s
	now := clock.Now().Unix()
	seedOpening(t, s, OpeningEntry{Key: bad, AdmittedAtUnix: now, Intent: malformedIntent(bad)})

	// Inside both graces nothing moves: a sweep must not read "no activity
	// yet" as dead.
	server.sweepGroup(ctx, s, normal.GroupID)
	if got := windowOf(t, s, normal.GroupID); got.pending() != 2 {
		t.Fatalf("a sweep inside the grace changed the window: %+v", got)
	}

	clock.advance(s.cfg.OpeningGrace + time.Second)
	server.sweepGroup(ctx, s, normal.GroupID)
	server.sweepGroup(ctx, s, normal.GroupID)

	if got := recorder.Count("dropped:sweep.advance_failed"); got != 0 {
		t.Fatalf("one malformed intent failed the whole group's sweep %d times (events: %v)", got, recorder.Snapshot())
	}
	activity, found, err := s.LookupActivity(ctx, normal)
	if err != nil || !found || activity.Status != StatusComplete {
		t.Fatalf("the healthy due activity was not completed behind the bad entry: %+v found=%v err=%v", activity, found, err)
	}
	window := windowOf(t, s, normal.GroupID)
	if window.openingIndex(bad) < 0 || window.Opening[window.openingIndex(bad)].Intent == nil {
		t.Fatalf("the malformed entry was reclaimed or rewritten instead of kept for an operator: %+v", window)
	}
	if got := recorder.Count("dropped:sweep.opening_intent_malformed"); got != 2 {
		t.Fatalf("sweep.opening_intent_malformed counted %d after two sweeps past grace, want 2", got)
	}
	if got := logs.count("opening intent is malformed"); got != 1 {
		t.Fatalf("malformed intent logged %d times across two sweeps, want once per state change", got)
	}

	// An operator repairs the plan in place: the next sweep helps it forward
	// like any durable intent, and the recovery is logged once.
	_, _, err = s.cfg.Windows.Update(ctx, bad.GroupID, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		i := next.openingIndex(bad)
		next.Opening[i].Intent = &Activity{Key: bad, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: now, UpdatedAtUnix: now}
		return next, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server.sweepGroup(ctx, s, normal.GroupID)
	server.sweepGroup(ctx, s, normal.GroupID)
	if repaired, found, err := s.LookupActivity(ctx, bad); err != nil || !found || repaired.Status != StatusPending {
		t.Fatalf("repaired intent was not created by the sweep: %+v found=%v err=%v", repaired, found, err)
	}
	window = windowOf(t, s, normal.GroupID)
	if window.openingIndex(bad) >= 0 || !window.contains(bad) {
		t.Fatalf("repaired entry was not confirmed into Keys: %+v", window)
	}
	if got := logs.count("opening intent is usable again"); got != 1 {
		t.Fatalf("recovery logged %d times, want once", got)
	}
	if got := logs.count("opening intent is malformed"); got != 1 {
		t.Fatalf("malformed intent logged again without a state change: %d", got)
	}
}

// 三种畸形都只跳过，不让 AdvanceExpired 返回错误，名额保留。
func TestEveryMalformedIntentShapeIsSkippedNotFatal(t *testing.T) {
	key := activityKey("rr09-shape")
	shapes := map[string]*Activity{
		"key_mismatch":   malformedIntent(key),
		"not_pending":    {Key: key, Status: StatusCollecting, ExpectedGameSIDs: []int32{1}},
		"empty_expected": {Key: key, Status: StatusPending},
	}
	for name, intent := range shapes {
		t.Run(name, func(t *testing.T) {
			s, clock := newRR09Service(t)
			ctx := context.Background()
			seedOpening(t, s, OpeningEntry{Key: key, AdmittedAtUnix: clock.Now().Unix(), Intent: intent})
			clock.advance(s.cfg.OpeningGrace + time.Second)
			if _, err := s.AdvanceExpired(ctx, key.GroupID, 10); err != nil {
				t.Fatalf("a malformed intent failed the sweep: %v", err)
			}
			if window := windowOf(t, s, key.GroupID); window.openingIndex(key) < 0 {
				t.Fatalf("malformed entry was reclaimed: %+v", window)
			}
			if _, found, _ := s.LookupActivity(ctx, key); found {
				t.Fatal("a malformed plan was executed")
			}
		})
	}
}

// 一条 Intent 完好但 Activities.Create 持续失败的 Opening：错误仍上报（名额保留，
// 后端恢复后完成），但不再挡住同组其他到期活动的完成。
func TestOneFailingOpeningCreateDoesNotStallTheRestOfTheGroup(t *testing.T) {
	stuck := activityKey("rr09-stuck-create")
	var hook *openingStoreHook
	s, clock := newRR09Service(t, func(c *Config) {
		hook = &openingStoreHook{Store: c.Activities}
		c.Activities = hook
	})
	ctx := context.Background()
	normal := activityKey("rr09-normal-2")
	openActivity(t, s, normal, 1, 2)
	notify(t, s, normal, 1)
	now := clock.Now().Unix()
	seedOpening(t, s, OpeningEntry{Key: stuck, AdmittedAtUnix: now, Intent: &Activity{
		Key: stuck, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: now, UpdatedAtUnix: now,
	}})
	hook.createErr = errors.New("activities: connection reset")
	clock.advance(s.cfg.OpeningGrace + time.Second)

	completed, err := s.AdvanceExpired(ctx, normal.GroupID, 10)
	if err == nil {
		t.Fatal("expected the still-unavailable create to be reported")
	}
	if len(completed) != 1 || completed[0].Key != normal {
		t.Fatalf("the healthy due activity did not complete behind a failing create: %v err=%v", keysOf(completed), err)
	}
	if window := windowOf(t, s, normal.GroupID); window.openingIndex(stuck) < 0 {
		t.Fatalf("a create whose outcome is unknown lost its slot: %+v", window)
	}
	hook.createErr = nil
	if _, err := s.AdvanceExpired(ctx, normal.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	if activity, found, err := s.LookupActivity(ctx, stuck); err != nil || !found || activity.Status != StatusPending {
		t.Fatalf("plan was not recovered once the backend returned: %+v %v %v", activity, found, err)
	}
}

// OpenActivity 与 sweep 交错不能让一个成功打开的活动失去窗口索引：窗口条目有 opening / 确认两段生命周期，清理只能删它观察到的恢复责任；
// OpeningGrace 后帮助持久计划创建，确认只消费原来的名额。
// 历史：RR-20260914-02。

type openingStoreHook struct {
	versionstore.Store[Key, Activity]
	beforeCreate func()
	createErr    error
}

func (s *openingStoreHook) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	if s.beforeCreate != nil {
		s.beforeCreate()
	}
	if s.createErr != nil {
		return versionstore.Versioned[Activity]{}, false, s.createErr
	}
	return s.Store.Create(ctx, key, value)
}

func TestOpenSweepInterleaveKeepsTheActivityInTheWindow(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	s.cfg.Activities = &openingStoreHook{Store: s.cfg.Activities, beforeCreate: func() {
		// 一次真实的 sweep 恰好落在窗口条目已在、活动记录未建之间。
		if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
			t.Fatal(err)
		}
	}}
	k := activityKey("opening")
	openActivity(t, s, k, 1, 2)
	notify(t, s, k, 1)
	c.advance(time.Minute)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	a, found, err := s.LookupActivity(ctx, k)
	if err != nil || !found {
		t.Fatalf("lookup %v %v", found, err)
	}
	keys, err := s.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusComplete {
		t.Fatalf("open succeeded but expired activity stranded: status=%s window=%v", a.Status, keys)
	}
}

// Create 与确认之间进程退出:活动存在、窗口里只有 opening 条目。下一次 sweep 必须把它认领进窗口,
// 之后照常按宽限期完成。
func TestSweepHealsAnOpeningEntryWhoseActivityExists(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	k := activityKey("half-open")
	if _, err := s.admitToWindow(ctx, Activity{Key: k, ExpectedGameSIDs: []int32{1, 2}, Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	now := s.cfg.Now().Unix()
	if _, created, err := s.cfg.Activities.Create(ctx, k, Activity{Key: k, ExpectedGameSIDs: []int32{1, 2}, Status: StatusPending, OpenedAtUnix: now, UpdatedAtUnix: now}); err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	notify(t, s, k, 1)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.LookupActivity(ctx, k)
	if err != nil || a.Status != StatusComplete {
		t.Fatalf("half-open activity not completed by the sweep: status=%s err=%v", a.Status, err)
	}
}

// Create 返回错误不能证明未提交。Grace 后继续帮助相同计划,失败时仍保留名额,
// 后端恢复后成功创建;不能靠超时重新承诺这个名额。
func TestSweepRetainsAndRecoversAnUnknownOpeningAfterGrace(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	hook := &openingStoreHook{Store: s.cfg.Activities, createErr: errors.New("activities: connection reset")}
	s.cfg.Activities = hook
	k := activityKey("abandoned")
	if _, err := s.OpenActivity(ctx, k, []int32{1}); err == nil {
		t.Fatal("expected the injected create failure")
	}
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.PendingActivities(ctx, "group-a", 10); len(keys) != 1 {
		t.Fatalf("an opening entry inside its grace was reclaimed: window=%v", keys)
	}
	c.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err == nil {
		t.Fatal("expected the still-unavailable create to be reported")
	}
	if keys, _ := s.PendingActivities(ctx, "group-a", 10); len(keys) != 1 {
		t.Fatalf("unknown create lost its capacity promise: window=%v", keys)
	}
	hook.createErr = nil
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if activity, found, err := s.LookupActivity(ctx, k); err != nil || !found || activity.Status != StatusPending {
		t.Fatalf("creation plan was not recovered: %+v %v %v", activity, found, err)
	}
}

// 持久 opening 是恢复意图，重投先校验它，不把坏计划写到 Activities；sweep 不执行非法键或跨组计划。坏记录只拒绝 / 跳过并保留，
// 操作员修正后走正式入口恢复，不自动删生产记录。
// 历史：RR-20261005-NC-42。
func TestOpenActivityRejectsMalformedPersistedIntentBeforeCreate(t *testing.T) {
	for _, shape := range []string{"foreign_key", "collecting", "empty_expected"} {
		t.Run(shape, func(t *testing.T) {
			s, clock := newRR09Service(t)
			ctx := context.Background()
			key := activityKey("persisted-plan")
			intent := Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
			switch shape {
			case "foreign_key":
				intent.Key.ActivityID = "foreign"
			case "collecting":
				intent.Status = StatusCollecting
			case "empty_expected":
				intent.ExpectedGameSIDs = nil
			}
			seedOpening(t, s, OpeningEntry{Key: key, Intent: &intent, AdmittedAtUnix: clock.Now().Unix()})
			before, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.OpenActivity(ctx, key, []int32{1}); !errors.Is(err, ErrConflict) {
				t.Errorf("malformed %s plan was accepted: %v", shape, err)
			}
			if _, found, err := s.cfg.Activities.Get(ctx, key); err != nil || found {
				t.Errorf("malformed %s plan wrote an activity: found=%v err=%v", shape, found, err)
			}
			after, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed opening window: %+v %v", after, err)
			}
			if t.Failed() {
				return
			}
			_, _, err = s.cfg.Windows.Update(ctx, key.GroupID, func(current Window, found bool) (Window, bool, error) {
				next := current.clone()
				next.Opening[0].Intent = &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
				return next, true, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.OpenActivity(ctx, key, []int32{1})
			if err != nil || got.Key != key || got.Status != StatusPending {
				t.Fatalf("repaired plan did not recover: %+v %v", got, err)
			}
			if _, err := s.OpenActivity(ctx, key, []int32{1}); !errors.Is(err, ErrExists) {
				t.Fatalf("healthy duplicate: %v", err)
			}
		})
	}
}

func TestOpeningSweepRejectsInvalidOrForeignGroupBeforeSideEffects(t *testing.T) {
	for _, shape := range []string{"empty_activity", "slash_phase", "foreign_group"} {
		t.Run(shape, func(t *testing.T) {
			recorder := servicemetrics.NewRecorder()
			s, clock := newRR09Service(t, func(c *Config) { c.Metrics = recorder })
			ctx := context.Background()
			normal := activityKey("healthy")
			openActivity(t, s, normal, 1, 2)
			notify(t, s, normal, 1)
			bad := activityKey("bad")
			switch shape {
			case "empty_activity":
				bad.ActivityID = ""
			case "slash_phase":
				bad.Phase = "close/other"
			case "foreign_group":
				bad.GroupID = "foreign-group"
			}
			intent := Activity{Key: bad, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
			_, _, err := s.cfg.Windows.Update(ctx, normal.GroupID, func(current Window, found bool) (Window, bool, error) {
				next := current.clone()
				next.Opening = append(next.Opening, OpeningEntry{Key: bad, Intent: &intent, AdmittedAtUnix: clock.Now().Unix()})
				return next, true, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(s.cfg.OpeningGrace + time.Second)
			completed, err := s.AdvanceExpired(ctx, normal.GroupID, 10)
			if err != nil || len(completed) != 1 || completed[0].Key != normal {
				t.Errorf("bad %s plan stalled healthy group: %v %v", shape, completed, err)
			}
			if _, found, err := s.cfg.Activities.Get(ctx, bad); err != nil || found {
				t.Errorf("bad %s plan wrote an activity: found=%v err=%v", shape, found, err)
			}
			if windowOf(t, s, normal.GroupID).openingIndex(bad) < 0 {
				t.Error("malformed plan lost its reserved slot")
			}
			if recorder.Count("dropped:sweep.opening_intent_malformed") != 1 {
				t.Error("malformed plan was not reported")
			}
		})
	}
}

// 同 key Open 恢复一条持久的开窗计划时沿用计划里的期望集合（不按本次请求改写），并把它确认进 Keys。
func TestOpeningRecoveryKeepsTheDurableIntent(t *testing.T) {
	s, clock := newRR09Service(t)
	key := activityKey("valid-recovery")
	now := clock.Now().Unix()
	seedOpening(t, s, OpeningEntry{Key: key, AdmittedAtUnix: now,
		Intent: &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1, 2}, OpenedAtUnix: now, UpdatedAtUnix: now}})
	got, err := s.OpenActivity(context.Background(), key, []int32{3})
	if err != nil || !reflect.DeepEqual(got.ExpectedGameSIDs, []int32{1, 2}) {
		t.Fatalf("durable recovery changed intent: %+v %v", got, err)
	}
	window := windowOf(t, s, key.GroupID)
	if window.openingIndex(key) >= 0 || !window.contains(key) || window.pending() != 1 {
		t.Fatalf("recovery did not confirm its one slot: %+v", window)
	}
}

func TestMalformedForeignOpeningDiagnosticsClearWithItsWindow(t *testing.T) {
	s, clock := newRR09Service(t)
	logs := captureSweepLogs(t, "opening intent is malformed", "opening intent is usable again")
	ctx := context.Background()
	group := "group-a"
	bad := Key{GroupID: "foreign", ActivityID: "bad", Phase: PhaseClose}
	intent := &Activity{Key: bad, Status: StatusPending, ExpectedGameSIDs: []int32{1}}
	if _, _, err := s.cfg.Windows.Create(ctx, group, Window{GroupID: group, Opening: []OpeningEntry{{Key: bad, Intent: intent, AdmittedAtUnix: clock.Now().Unix()}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceExpired(ctx, group, 10); err != nil {
		t.Fatal(err)
	}
	if logs.count("opening intent is malformed") != 1 {
		t.Fatal("foreign key diagnostic was not reported")
	}
	if _, _, err := s.cfg.Windows.Update(ctx, group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.Opening = nil
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceExpired(ctx, group, 10); err != nil {
		t.Fatal(err)
	}
	if logs.count("opening intent is usable again") != 1 {
		t.Fatal("removed corrupt entry left its diagnostic active")
	}
}

type progressReconcileLedger struct {
	versionstore.Store[RequestKey, ProgressReservation]
	beforeMark func()
}

func (l *progressReconcileLedger) Update(ctx context.Context, key RequestKey, mutate versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	if l.beforeMark != nil {
		hook := l.beforeMark
		l.beforeMark = nil
		hook()
	}
	return l.Store.Update(ctx, key, mutate)
}

// N06 admin 增量控制：读完 pending 后另一个请求成功写入，新证明不能被旧对账清掉。
func TestProgressReconcileRetainsAConcurrentNewProof(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("reconcile-new-proof")
	openActivity(t, s, key, 1)
	healthy := s.cfg.Ledger
	s.cfg.Ledger = failedProgressMark{healthy}
	if _, err := s.ApplyProgress(ctx, key, "p", "old", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("lost-mark fixture did not fail")
	}
	fired := false
	l := &progressReconcileLedger{Store: healthy}
	s.cfg.Ledger = l
	l.beforeMark = func() {
		fired = true
		// 创建第二个 Service 共享存储，避免修改执行中的对象或绕过正式 ApplyProgress。
		cfg := s.cfg
		cfg.Ledger = failedProgressMark{healthy}
		other, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.ApplyProgress(ctx, key, "p", "new", ProgressDelta{Score: 2}); err == nil {
			t.Fatal("new lost-mark fixture did not fail")
		}
	}
	got, err := s.ReconcileProgress(ctx, key, "p", "finish old mark")
	if !fired {
		t.Fatal("reconcile interleaving did not run")
	}
	if err != nil || got.Score != 3 || got.Applies != 2 || !reflect.DeepEqual(got.PendingRequestIDs, []string{"new"}) {
		t.Fatalf("old reconcile lost concurrent proof: %+v %v", got, err)
	}
	got, err = s.ReconcileProgress(ctx, key, "p", "finish new mark")
	if err != nil || len(got.PendingRequestIDs) != 0 || got.Score != 3 || got.Applies != 2 {
		t.Fatalf("second reconcile did not converge: %+v %v", got, err)
	}
}
