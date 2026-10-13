// 活动 sweep：按截止时间推进、跨轮重新发现派发、欠款清单、不消耗交付尝试、窗口条目单一读取入口与坏键隔离。

package activity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type failingWindows struct {
	versionstore.Store[string, Window]
	err error
}

func (w failingWindows) Get(context.Context, string) (versionstore.Versioned[Window], bool, error) {
	return versionstore.Versioned[Window]{}, false, w.err
}

func TestSweepFailuresAreCountedNotJustLogged(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("windows store: connection reset")
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Windows = failingWindows{Store: cfg.Windows, err: wire}
		cfg.Metrics = recorder
	})
	server := &Server{service: service}
	ctx := context.Background()
	server.sweepGroup(ctx, service, "group-a")
	server.sweepGroup(ctx, service, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("sweep.advance_failed counted %d times after two failing sweeps, want 2 (events: %s)", got, recorder.Events())
	}

	// 健康的 sweep 不计失败。
	healthy, _ := newActivityService(t, func(cfg *Config) { cfg.Metrics = recorder })
	(&Server{service: healthy}).sweepGroup(ctx, healthy, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("a healthy sweep changed the failure count to %d", got)
	}
}

func TestTheSweepLoopAdvancesTheConfiguredGroups(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t, func(cfg *Config) { cfg.SweepGroups = []string{"group-a"} })
	key := activityKey("act-loop")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1) // deadline = now + 30s grace
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		activity, found, err := service.LookupActivity(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if found && activity.Status == StatusComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep loop never advanced the lapsed activity in a configured group: status=%v", activity.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTheSweepLoopWithoutGroupsSweepsNothingAndStopsCleanly(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t)
	key := activityKey("act-idle")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if activity, _, _ := service.LookupActivity(context.Background(), key); activity.Status != StatusCollecting {
		t.Fatalf("a process with no configured groups advanced an activity: %v", activity.Status)
	}
}

// A bounded sweep takes the earliest deadline first, so an activity that has
// waited longest is never starved by newer ones that happen to sort first by
// key. The keys here are named so that key order and deadline order disagree.
func TestABoundedSweepCompletesTheEarliestDeadlineFirst(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	late := activityKey("act-z-first-notified")
	early := activityKey("act-a-notified-later")
	openActivity(t, service, late, 1, 2)
	notify(t, service, late, 1) // deadline t0+30s
	c.advance(10 * time.Second)
	openActivity(t, service, early, 1, 2)
	notify(t, service, early, 1) // deadline t0+40s
	c.advance(31 * time.Second)  // both lapsed

	completed, err := service.AdvanceExpired(ctx, "group-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Key != late {
		t.Fatalf("a sweep limited to one completed %+v; the activity with the earliest deadline is %s", keysOf(completed), late)
	}
}

func keysOf(activities []Activity) []Key {
	out := make([]Key, 0, len(activities))
	for _, activity := range activities {
		out = append(out, activity.Key)
	}
	return out
}

// dispatchesFailingOnce is the dispatch store whose first Create is lost: the
// completing notify then returns an error with the aggregation already
// complete, and the activity stays listed in the window with its deliveries
// missing. The sweep is documented as the thing that heals exactly this.
type dispatchesFailingOnce struct {
	versionstore.Store[DispatchKey, Dispatch]
	failed bool
}

func (d *dispatchesFailingOnce) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	if !d.failed {
		d.failed = true
		return versionstore.Versioned[Dispatch]{}, false, fmt.Errorf("dispatch store: connection reset")
	}
	return d.Store.Create(ctx, key, value)
}

func TestTheSweepHealsACompletedActivityWhoseDispatchesWereNeverCreated(t *testing.T) {
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Dispatches = &dispatchesFailingOnce{Store: versionstore.NewMemoryStore[DispatchKey, Dispatch]()}
	})
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	if _, err := service.NotifyPhase(ctx, key, 2); err == nil {
		t.Fatal("the completing notify reported success although a dispatch was never created")
	}
	activity, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found || activity.Status != StatusComplete {
		t.Fatalf("the aggregation must be complete regardless of the failed dispatch: found=%v status=%v err=%v", found, activity.Status, err)
	}
	pending, err := service.PendingActivities(ctx, "group-a", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("the unsettled completion must stay in the window for the sweep: %v %v", pending, err)
	}

	if _, err := service.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatalf("the healing sweep failed: %v", err)
	}
	for _, gameSID := range []int32{1, 2} {
		if _, found, err := service.LookupDispatch(ctx, key, gameSID); err != nil || !found {
			t.Errorf("game %d still has no dispatch after the sweep: found=%v err=%v", gameSID, found, err)
		}
	}
	pending, err = service.PendingActivities(ctx, "group-a", 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("the healed activity is still listed in the window: %v %v", pending, err)
	}
}

// 后台 sweep 能跨轮重新发现待派发的活动：完成的活动进入窗口里独立于聚合的 Delivering 索引，保留到全部 dispatch 终态（acked / exhausted），
// 每轮 sweep 都能重新找到它且仍然可取；它不占 MaxPendingActivities 的开活动名额。sweep 不调 AttemptDispatch。
// 历史：RR-20260914-03。

func TestSweepRetriesDispatchesAcrossRounds(t *testing.T) {
	for _, mode := range []string{"grace", "notify", "heal"} {
		t.Run(mode, func(t *testing.T) {
			s, c := newActivityService(t)
			ctx := context.Background()
			k := activityKey(mode)
			if mode == "heal" {
				s.cfg.Dispatches = &dispatchesFailingOnce{Store: s.cfg.Dispatches}
			}
			openActivity(t, s, k, 1, 2)
			notify(t, s, k, 1)
			switch mode {
			case "grace":
				c.advance(time.Minute)
			case "notify":
				notify(t, s, k, 2)
			case "heal":
				if _, err := s.NotifyPhase(ctx, k, 2); err == nil {
					t.Fatal("expected the injected dispatch create failure")
				}
			}
			server := &Server{service: s}
			server.sweepGroup(ctx, s, "group-a")
			c.advance(time.Minute)
			server.sweepGroup(ctx, s, "group-a")

			// Still rediscoverable after a round it was not created in: the
			// Delivering index, not this round's completions, is what the
			// sweep works from.
			keys, err := s.DeliveringActivities(ctx, "group-a", 10)
			if err != nil {
				t.Fatalf("delivering: %v", err)
			}
			if len(keys) != 1 || keys[0] != k {
				t.Fatalf("the completed activity is not in the delivering index after two sweeps: %v", keys)
			}
			due, err := s.DueDispatches(ctx, k, 10)
			if err != nil {
				t.Fatalf("due: %v", err)
			}
			if len(due) == 0 {
				t.Fatal("the activity is indexed but its dispatch is not due to anybody")
			}
			// And the budget is intact: the sweeps delivered nothing, so they
			// spent nothing.
			d, found, err := s.LookupDispatch(ctx, k, 1)
			if err != nil || !found {
				t.Fatalf("lookup %v %v", found, err)
			}
			if d.Attempts != 0 {
				t.Fatalf("the sweep spent %d attempts without delivering anything", d.Attempts)
			}
			// The game takes it, and that is what costs an attempt.
			taken, err := s.AttemptDispatch(ctx, k, 1)
			if err != nil {
				t.Fatalf("the game could not take its result: %v", err)
			}
			if taken.Attempts != 1 {
				t.Fatalf("the game's take is attempt %d, want 1", taken.Attempts)
			}
		})
	}
}

// 派发索引到终态才退出:全部 ACK 后不再被扫描;打满尝试预算(exhausted)同样退出,不会永远占位。
func TestDeliveringIndexRetiresAtTerminalStates(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	k := activityKey("ack")
	openActivity(t, s, k, 1, 2)
	notify(t, s, k, 1)
	notify(t, s, k, 2)
	server := &Server{service: s}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 1 || keys[0] != k {
		t.Fatalf("completed activity not indexed for delivery: %v", keys)
	}
	for _, sid := range []int32{1, 2} {
		d, _, err := s.LookupDispatch(ctx, k, sid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AckDispatch(ctx, k, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 0 {
		t.Fatalf("fully acked activity still indexed: %v", keys)
	}

	// exhausted — now driven by the taker, because that is who spends the
	// budget. A game that keeps taking the payload and never acks is the case
	// exhaustion is for; a game that is merely absent is not.
	k2 := activityKey("exhaust")
	openActivity(t, s, k2, 1)
	notify(t, s, k2, 1)
	for i := 0; i < s.cfg.DispatchMaxAttempts+1; i++ {
		_, _ = s.AttemptDispatch(ctx, k2, 1)
		c.advance(time.Hour)
	}
	d, _, err := s.LookupDispatch(ctx, k2, 1)
	if err != nil || d.State != DispatchExhausted {
		t.Fatalf("dispatch not exhausted: state=%s attempts=%d err=%v", d.State, d.Attempts, err)
	}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 0 {
		t.Fatalf("exhausted activity still indexed: %v", keys)
	}
}

// dispatch 要真的被交付，“欠我什么”要能问出来：尝试次数只在游戏真的拿到 payload 时消耗（sweep 不消耗），游戏能按 gameSID 问出欠它的
// 清单，不必知道 activity id。
// 历史：RR-20260919-10。

// memoryOwedDispatches is the memory store plus the per-game index, so the
// service's own tests exercise the same path the Redis store implements.
type memoryOwedDispatches struct {
	versionstore.Store[DispatchKey, Dispatch]
	mu   sync.Mutex
	seen map[DispatchKey]struct{}
}

func newMemoryOwedDispatches() *memoryOwedDispatches {
	return &memoryOwedDispatches{
		Store: versionstore.NewMemoryStore[DispatchKey, Dispatch](),
		seen:  map[DispatchKey]struct{}{},
	}
}

func (m *memoryOwedDispatches) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	result, created, err := m.Store.Create(ctx, key, value)
	if created {
		m.mu.Lock()
		m.seen[key] = struct{}{}
		m.mu.Unlock()
	}
	return result, created, err
}

func (m *memoryOwedDispatches) OwedDispatches(ctx context.Context, groupID string, gameSID int32, nowUnix int64, limit int) ([]DispatchKey, error) {
	m.mu.Lock()
	keys := make([]DispatchKey, 0, len(m.seen))
	for key := range m.seen {
		keys = append(keys, key)
	}
	m.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]DispatchKey, 0, limit)
	for _, key := range keys {
		if key.GameSID != gameSID || key.Activity.GroupID != groupID {
			continue
		}
		current, found, err := m.Store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found || current.Value.State != DispatchPending || !current.Value.Due(nowUnix) {
			continue
		}
		out = append(out, key)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func newOwedService(t *testing.T) (*Service, *activityClock, *memoryOwedDispatches) {
	t.Helper()
	dispatches := newMemoryOwedDispatches()
	service, clock := newActivityService(t, func(cfg *Config) { cfg.Dispatches = dispatches })
	return service, clock, dispatches
}

// A game that was away for several windows still finds what it is owed.
func TestAGameFindsWhatItIsOwedWithoutGuessingActivityIDs(t *testing.T) {
	service, clock, _ := newOwedService(t)
	ctx := context.Background()
	const gameSID = int32(1000)

	// Three windows complete while this game is away; it notified each one
	// before going offline, which is what makes them complete.
	for _, id := range []string{"race-1", "race-2", "race-3"} {
		key := activityKey(id)
		openActivity(t, service, key, gameSID)
		if _, err := service.NotifyPhase(ctx, key, gameSID); err != nil {
			t.Fatalf("%s: notify: %v", id, err)
		}
	}
	clock.advance(time.Hour)

	owed, err := service.OwedDispatches(ctx, "group-a", gameSID, 10)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if len(owed) != 3 {
		t.Fatalf("the game is owed %d results, want the three windows it missed", len(owed))
	}
	ids := make([]string, 0, len(owed))
	for _, key := range owed {
		ids = append(ids, key.ActivityID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "race-1,race-2,race-3" {
		t.Fatalf("owed = %v, want every window with an unacked result", ids)
	}

	// Taking one and acking it removes it from what is owed; the others stay.
	dispatch, err := service.AttemptDispatch(ctx, owed[0], gameSID)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := service.AckDispatch(ctx, owed[0], gameSID, dispatch.Token); err != nil {
		t.Fatalf("ack: %v", err)
	}
	remaining, err := service.OwedDispatches(ctx, "group-a", gameSID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("after one ack the game is owed %d, want 2", len(remaining))
	}
}

// The sweep must not spend the delivery budget. A game that is down for a
// while has to find its result intact when it comes back, not exhausted by a
// loop that never delivered anything.
func TestTheSweepDoesNotConsumeDeliveryAttempts(t *testing.T) {
	service, clock, _ := newOwedService(t)
	ctx := context.Background()
	const gameSID = int32(1000)
	key := activityKey("race-1")
	openActivity(t, service, key, gameSID)
	if _, err := service.NotifyPhase(ctx, key, gameSID); err != nil {
		t.Fatal(err)
	}

	server := &Server{service: service}
	for i := 0; i < 5; i++ {
		clock.advance(time.Minute)
		server.sweepGroup(ctx, service, key.GroupID)
	}

	dispatch, found, err := service.LookupDispatch(ctx, key, gameSID)
	if err != nil || !found {
		t.Fatalf("dispatch lookup: found=%v err=%v", found, err)
	}
	if dispatch.State != DispatchPending {
		t.Fatalf("the dispatch is %q after five sweeps with no delivery, want still pending", dispatch.State)
	}
	if dispatch.Attempts != 0 {
		t.Fatalf("the sweep spent %d delivery attempts without delivering anything", dispatch.Attempts)
	}

	// And the game, when it comes back, still has its whole budget.
	taken, err := service.AttemptDispatch(ctx, key, gameSID)
	if err != nil {
		t.Fatalf("the game could not take its result: %v", err)
	}
	if taken.Attempts != 1 {
		t.Fatalf("the game's first take is attempt %d, want 1", taken.Attempts)
	}
}

// AdvanceExpired 只处理键合法且属于本组的确认条目：坏条目（别组的键、不合法的键）跳过、保留给运维（不自动删）、每 tick 计
// dropped:sweep.window_key_malformed；它不挡住本组其他活动的结算，也不写别的组的活动或窗口。
// 历史：RR-20261005-NC-51。

// plantConfirmedKeys appends keys to group's confirmed Keys the way a stored
// record written by something other than this package would hold them.
func plantConfirmedKeys(t *testing.T, windows versionstore.Store[string, Window], group string, keys ...Key) {
	t.Helper()
	if _, _, err := windows.Update(context.Background(), group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.GroupID = group
		next.Keys = append(next.Keys, keys...)
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSweepSkipsConfirmedKeysOfAnotherGroup(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	openActivity(t, s, foreign, 2001, 2002)
	notify(t, s, foreign, 2001) // collecting in its own group
	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	notify(t, s, local, 1)
	plantConfirmedKeys(t, windows, "group-a", foreign)
	clock.advance(time.Minute) // both grace windows lapse

	done, err := s.AdvanceExpired(ctx, "group-a", 10)
	if err != nil {
		t.Fatalf("a foreign confirmed key failed group-a's sweep: %v", err)
	}
	if len(done) != 1 || done[0].Key != local {
		t.Fatalf("group-a's sweep completed %+v, want only its own %s", done, local)
	}
	if got, _, err := s.LookupActivity(ctx, foreign); err != nil || got.Status != StatusCollecting {
		t.Fatalf("group-a's sweep wrote group-b's activity: %+v err=%v", got, err)
	}
	if _, found, err := s.LookupDispatch(ctx, foreign, 2001); err != nil || found {
		t.Fatalf("group-a's sweep created group-b's dispatch: found=%v err=%v", found, err)
	}
	window, _, err := windows.Get(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if window.Value.delivering(foreign) {
		t.Fatalf("group-b's key joined group-a's Delivering: %+v", window.Value)
	}
	if !containsKey(window.Value.Keys, foreign) {
		t.Fatalf("the foreign entry was deleted instead of kept for an operator: %+v", window.Value)
	}
	if got := recorder.Count("dropped:sweep.window_key_malformed"); got != 1 {
		t.Fatalf("sweep.window_key_malformed counted %d, want 1 (%s)", got, recorder.Events())
	}

	// group-b's own sweep still finishes it, exactly once.
	if done, err := s.AdvanceExpired(ctx, "group-b", 10); err != nil || len(done) != 1 || done[0].Key != foreign {
		t.Fatalf("group-b's own sweep: %+v err=%v", done, err)
	}
}

func TestSweepSkipsInvalidConfirmedKeys(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	notify(t, s, local, 1)
	bad := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	plantConfirmedKeys(t, windows, "group-a", bad)
	clock.advance(time.Minute)

	for tick := 1; tick <= 2; tick++ {
		if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
			t.Fatalf("tick %d: an invalid confirmed key failed the sweep: %v", tick, err)
		}
		window, _, err := windows.Get(ctx, "group-a")
		if err != nil {
			t.Fatal(err)
		}
		if !containsKey(window.Value.Keys, bad) || window.Value.delivering(bad) {
			t.Fatalf("tick %d: the invalid entry was acted on (read as an activity, pruned or moved) instead of skipped and kept: %+v", tick, window.Value)
		}
		if got := recorder.Count("dropped:sweep.window_key_malformed"); got != tick {
			t.Fatalf("tick %d: sweep.window_key_malformed counted %d (%s)", tick, got, recorder.Events())
		}
	}
	if got, _, err := s.LookupActivity(ctx, local); err != nil || got.Status != StatusComplete {
		t.Fatalf("the group's own activity did not complete: %+v err=%v", got, err)
	}
}

func containsKey(keys []Key, key Key) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// 读取已存窗口条目只走一个入口：Keys / Opening / Delivering 的读者（含对 game 公开的 PendingActivities 与 DeliveringActivities）都经同一个
// 校验；坏条目（windowKeyProblem 非空）跳过、保留给运维、按列表计数；RetireDelivered 只在本次确实把键移出 Delivering 时回报 true。

// plantDelivering appends keys to group's Delivering list the way a stored
// record written by something other than this package would hold them.
func plantDelivering(t *testing.T, windows versionstore.Store[string, Window], group string, keys ...Key) {
	t.Helper()
	if _, _, err := windows.Update(context.Background(), group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.GroupID = group
		next.Delivering = append(next.Delivering, keys...)
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ackAll acknowledges every dispatch of a complete activity.
func ackAll(t *testing.T, s *Service, key Key, sids ...int32) {
	t.Helper()
	for _, sid := range sids {
		d, found, err := s.LookupDispatch(context.Background(), key, sid)
		if err != nil || !found {
			t.Fatalf("dispatch %s/%d: found=%v err=%v", key, sid, found, err)
		}
		if _, err := s.AckDispatch(context.Background(), key, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeliveringSkipsMalformedEntriesAndKeepsThem(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, _ := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	// group-b's own activity: complete, every dispatch acked, still listed in
	// group-b's Delivering (group-b's sweep has not run yet).
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	openActivity(t, s, foreign, 2001)
	notify(t, s, foreign, 2001)
	ackAll(t, s, foreign, 2001)
	// group-a's own activity, complete and still delivering.
	local := activityKey("local")
	openActivity(t, s, local, 1)
	notify(t, s, local, 1)
	invalid := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	plantDelivering(t, windows, "group-a", foreign, invalid)

	keys, err := s.DeliveringActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != local {
		t.Fatalf("group-a's delivering list = %v, want only its own %s", keys, local)
	}
	if got := recorder.Count("dropped:sweep.delivering_key_malformed"); got != 2 {
		t.Fatalf("sweep.delivering_key_malformed counted %d, want 2 (%s)", got, recorder.Events())
	}

	before, _, err := windows.Get(ctx, "group-b")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{service: s}
	server.sweepGroup(ctx, s, "group-a")
	after, _, err := windows.Get(ctx, "group-b")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !after.Value.delivering(foreign) {
		t.Fatalf("group-a's sweep wrote group-b's window: before %+v after %+v", before, after)
	}
	window, _, err := windows.Get(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if !window.Value.delivering(foreign) || !window.Value.delivering(invalid) {
		t.Fatalf("a malformed delivering entry was removed instead of kept for an operator: %+v", window.Value)
	}

	// group-b's own sweep still retires it.
	server.sweepGroup(ctx, s, "group-b")
	if keys, _ := s.DeliveringActivities(ctx, "group-b", 10); len(keys) != 0 {
		t.Fatalf("group-b's own sweep did not retire its acked activity: %v", keys)
	}
}

func TestPendingActivitiesSkipsMalformedEntries(t *testing.T) {
	var windows versionstore.Store[string, Window]
	s, _ := newActivityService(t, func(c *Config) { windows = c.Windows })
	ctx := context.Background()

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	plantConfirmedKeys(t, windows, "group-a", foreign, Key{GroupID: "group-a", Phase: PhaseClose})
	seedOpening(t, s, OpeningEntry{Key: Key{GroupID: "group-a", ActivityID: "bad/id", Phase: PhaseClose}})

	keys, err := s.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != local {
		t.Fatalf("PendingActivities(group-a) = %v, want only %s: a game must not be handed another group's or an invalid key", keys, local)
	}
}

func TestRetireDeliveredReportsOnlyWhatItRemoved(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()

	k := activityKey("acked")
	openActivity(t, s, k, 1)
	notify(t, s, k, 1)
	ackAll(t, s, k, 1)

	retired, err := s.RetireDelivered(ctx, k)
	if err != nil || !retired {
		t.Fatalf("first retire = %v, %v; want true", retired, err)
	}
	retired, err = s.RetireDelivered(ctx, k)
	if err != nil || retired {
		t.Fatalf("retiring a key that is no longer listed = %v, %v; want false", retired, err)
	}
	never := activityKey("never-opened")
	if retired, err := s.RetireDelivered(ctx, never); err != nil || retired {
		t.Fatalf("retiring a key that was never listed = %v, %v; want false", retired, err)
	}
}

// The operator side of B9: the malformed entries the sweep skips can be
// listed and removed by the owning process, one at a time, with a note — and
// only malformed ones.
func TestOperatorRemovesOnlyMalformedWindowEntries(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()
	logs := captureSweepLogs(t, "delivering window key is malformed", "delivering window key is usable again")

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	done := activityKey("done")
	openActivity(t, s, done, 1)
	notify(t, s, done, 1) // complete, delivering
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	invalid := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	badPlan := activityKey("bad-plan")
	plantConfirmedKeys(t, windows, "group-a", foreign, foreign)
	plantDelivering(t, windows, "group-a", invalid)
	seedOpening(t, s, OpeningEntry{Key: badPlan, AdmittedAtUnix: clock.Now().Unix(), Intent: malformedIntent(badPlan)})

	listed, err := s.MalformedWindowEntries(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	want := map[malformedID]bool{{WindowKeys, foreign}: true, {WindowDelivering, invalid}: true, {WindowOpening, badPlan}: true}
	got := map[malformedID]bool{}
	for _, entry := range listed {
		if entry.Reason == "" {
			t.Errorf("entry %+v has no reason", entry)
		}
		got[malformedID{entry.List, entry.Key}] = true
	}
	if len(got) != len(want) {
		t.Fatalf("MalformedWindowEntries = %+v, want exactly %v", listed, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("MalformedWindowEntries = %+v, missing %+v", listed, id)
		}
	}
	if _, err := s.DeliveringActivities(ctx, "group-a", 10); err != nil { // the sweep's tick sees it
		t.Fatal(err)
	}

	// Refusals: no note, unknown list, healthy entries, absent entries.
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", WindowKeys, foreign, " "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("no note: %v, want ErrAdminNoteRequired", err)
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", "pending", foreign, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown list: %v, want ErrInvalid", err)
	}
	for _, tc := range []struct {
		list WindowList
		key  Key
	}{{WindowKeys, local}, {WindowDelivering, done}} {
		if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", tc.list, tc.key, "cleanup"); !errors.Is(err, ErrStatus) {
			t.Fatalf("removing the healthy %s entry %s: %v, want ErrStatus", tc.list, tc.key, err)
		}
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", WindowOpening, foreign, "cleanup"); !errors.Is(err, ErrMissing) {
		t.Fatalf("an entry not in that list: %v, want ErrMissing", err)
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-z", WindowKeys, foreign, "cleanup"); !errors.Is(err, ErrMissing) {
		t.Fatalf("a group with no window: %v, want ErrMissing", err)
	}
	if w := windowOf(t, s, "group-a"); !containsKey(w.Keys, local) || !w.delivering(done) || w.AdminNote != "" {
		t.Fatalf("a refused repair changed the window: %+v", w)
	}

	// The repairs.
	for _, tc := range []struct {
		list WindowList
		key  Key
		want int
	}{{WindowKeys, foreign, 2}, {WindowDelivering, invalid, 1}, {WindowOpening, badPlan, 1}} {
		removed, err := s.RemoveMalformedWindowEntry(ctx, "group-a", tc.list, tc.key, "INC-42: written by a broken migration")
		if err != nil || removed != tc.want {
			t.Fatalf("remove %s %s = %d, %v; want %d", tc.list, tc.key, removed, err, tc.want)
		}
	}
	w := windowOf(t, s, "group-a")
	if containsKey(w.Keys, foreign) || w.delivering(invalid) || w.openingIndex(badPlan) >= 0 {
		t.Fatalf("a repaired entry is still stored: %+v", w)
	}
	if !containsKey(w.Keys, local) || !w.delivering(done) {
		t.Fatalf("a repair removed a healthy entry: %+v", w)
	}
	if w.AdminNote != "INC-42: written by a broken migration" || w.AdminActionAtUnix != clock.Now().Unix() {
		t.Fatalf("the repair note was not recorded on the window: %+v", w)
	}
	if listed, err := s.MalformedWindowEntries(ctx, "group-a"); err != nil || len(listed) != 0 {
		t.Fatalf("after the repairs MalformedWindowEntries = %+v, %v", listed, err)
	}
	if got := recorder.Count("accepted:admin.remove_malformed_window_entry"); got != 3 {
		t.Fatalf("repairs counted %d, want 3 (%s)", got, recorder.Events())
	}

	// The next tick sees the list clean: no further count, one "gone" line.
	if _, err := s.DeliveringActivities(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Count("dropped:sweep.delivering_key_malformed"); got != 1 {
		t.Fatalf("sweep.delivering_key_malformed counted %d after the repair, want only the one before it", got)
	}
	if logs.count("delivering window key is malformed") != 1 || logs.count("delivering window key is usable again") != 1 {
		t.Fatalf("delivering logs: appeared %d, cleared %d; want one each",
			logs.count("delivering window key is malformed"), logs.count("delivering window key is usable again"))
	}
}
