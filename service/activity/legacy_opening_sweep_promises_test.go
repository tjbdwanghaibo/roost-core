package activity

// RR-20261001-09：窗口里一条没有 Intent 的 legacy Opening 不能永久占名额，一条坏
// Intent 不能让该 group 的 AdvanceExpired 每 tick 整体失败。
//
// 旧行为（RR-20260914-02 残余，4b0837d7 引入）：sweep 对 Opening 条目的处理是
// `entry.Intent == nil → continue`——升级前已经孤儿化的条目既不回收也不帮助，
// `Window.pending()` 却把它计入 MaxPendingActivities；旧版本里它在 OpeningGrace 后
// 会被回收。同一循环里 Intent 畸形（Key 不符 / 非 Pending / expected 集合非法）时
// `return nil, err`，同组其他到期活动的 complete / dispatch 跟着停，下一 tick 在
// 同一条上再卡一次。
//
// 承诺：legacy 条目过 OpeningGrace 且活动不存在 → 回收名额（只删 CAS 时仍无 Intent
// 的那一段）；坏 Intent → 跳过、每 tick 计 `sweep.opening_intent_malformed`、日志只在
// 状态变化时打一次，名额保留（见 bugfix 记录“为什么不回收”）；同组其他条目照常推进。
//
// 后端由 ROOST_REVIEW4_BACKEND 选：默认 Memory；"redis" 时用 ROOST_REVIEW_REDIS
// 指向的隔离 Redis（前缀 rr09:activity:<nanos>，不 FLUSH）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

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
		stores, err := NewRedisStores(client, fmt.Sprintf("rr09:activity:%d", time.Now().UnixNano()), time.Hour)
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

// 一条无 Intent 的 legacy Opening + 一条坏 Intent + 一条正常到期活动：sweep 后正常
// 活动完成、legacy 名额回收、坏条目被跳过且不让整组失败；坏条目的日志只打一次，
// 修好之后再打一次“恢复”并被 sweep 按计划创建。
func TestSweepReclaimsLegacyOpeningAndSkipsMalformedIntent(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	s, clock := newRR09Service(t, func(c *Config) { c.Metrics = recorder })
	logs := captureSweepLogs(t, "opening intent is malformed", "opening intent is usable again")
	ctx := context.Background()
	server := &Server{service: s}

	normal := activityKey("rr09-normal")
	legacy := activityKey("rr09-legacy")
	bad := activityKey("rr09-bad-intent")
	openActivity(t, s, normal, 1, 2)
	notify(t, s, normal, 1) // collecting, grace deadline now+30s
	now := clock.Now().Unix()
	seedOpening(t, s,
		OpeningEntry{Key: legacy, AdmittedAtUnix: now},
		OpeningEntry{Key: bad, AdmittedAtUnix: now, Intent: malformedIntent(bad)},
	)

	// Inside both graces nothing moves: the legacy entry is still within
	// OpeningGrace, so a sweep must not read "no activity yet" as dead.
	server.sweepGroup(ctx, s, normal.GroupID)
	if got := windowOf(t, s, normal.GroupID); got.pending() != 3 {
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
	if window.openingIndex(legacy) >= 0 {
		t.Fatalf("legacy opening without a plan still holds a slot after OpeningGrace: %+v", window)
	}
	if window.openingIndex(bad) < 0 || window.Opening[window.openingIndex(bad)].Intent == nil {
		t.Fatalf("the malformed entry was reclaimed or rewritten instead of kept for an operator: %+v", window)
	}
	if got := recorder.Count("dropped:sweep.opening_intent_malformed"); got != 2 {
		t.Fatalf("sweep.opening_intent_malformed counted %d after two sweeps past grace, want 2", got)
	}
	if got := recorder.Count("dropped:sweep.opening_legacy_reclaimed"); got != 1 {
		t.Fatalf("sweep.opening_legacy_reclaimed counted %d, want 1", got)
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
