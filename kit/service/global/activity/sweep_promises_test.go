package activity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

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
