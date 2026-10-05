package account

// B9（N06 S3 观察 4，2026-10-06）：两个换名请求并发释放同一个死计划时，
// create_role.plan_released 只能计一次。
//
// 旧行为：releaseCreationSlot 把 DeleteIf 的 ErrVersionMismatch（别人已经删掉）当成功返回 nil，
// 调用方据此各计一次 plan_released：同一个 slot 只被删了一次，指标却是 2。
//
// 承诺：只有真正删掉 slot 的那一次计 plan_released；输掉的一方不计、不报错，照常按新的 slot 判定。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// slotsDeleteBarrier holds every conditional delete until two callers have
// arrived, so both release the same observed slot version.
type slotsDeleteBarrier struct {
	versionstore.Store[string, Slot]
	pending atomic.Int32 // deletes still to be held at the barrier
	arrived sync.WaitGroup
}

func (s *slotsDeleteBarrier) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if s.pending.Add(-1) >= 0 {
		s.arrived.Done()
		s.arrived.Wait()
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func TestConcurrentReleasesOfOneDeadPlanCountOnce(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsDeleteBarrier
	s, cfg, _, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsDeleteBarrier{Store: c.Slots}
		slots.pending.Store(-1 << 30) // disarmed while the fixture runs
		c.Slots = slots
	})
	slots.arrived.Add(2)
	slots.pending.Store(2)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, 2)
	roles := make([]Role, 2)
	for i, name := range []string{"Knight", "Mage"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			roles[i], results[i] = s.CreateRole(ctx, a.ID, 1, name)
		}()
	}
	wg.Wait()

	won := 0
	for i, err := range results {
		switch {
		case err == nil:
			won++
			if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != roles[i].PlayerID {
				t.Fatalf("winner %+v is not the slot's role: %+v", roles[i], slot)
			}
		case errors.Is(err, ErrRoleLimit):
		default:
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of two concurrent different-name requests created a role, want exactly 1: %v", won, results)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("one dead plan released once was counted %d times; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:rollback.failed"); got != 0 {
		t.Fatalf("losing the release race counted as a failed rollback: %s", sink.Events())
	}
}
