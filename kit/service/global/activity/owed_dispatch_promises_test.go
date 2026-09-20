package activity

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RR-20260919-10：dispatch 要真的被交付，而"欠我什么"要能问出来。
//
// 两半都缺：
//
//   - 服务端 sweep 调 `AttemptDispatch` 只是**消耗一次尝试**并把 payload 丢掉——
//     链上没有任何传输。默认五次之后 dispatch 进 exhausted，而游戏服一次都没收到。
//   - 游戏端只能按本地时钟猜"当前 / 上一窗口"两个 activity id 去 `LookupDispatch`。
//     离线超过一个窗口，旧的 owed dispatch 就再也查不到，而它的义务仍然持久存在。
//
// 所以：**尝试次数只在游戏真的拿到 payload 时消耗**，并且游戏能按 gameSID 问出
// 欠它的清单，不必知道 activity id。

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
