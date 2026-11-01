package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestReviewWaitTerminalRejectsPreviousLifecycle(t *testing.T) {
	raw := &stubRaw{}
	e, err := NewEngine(stubLog{}, raw, Options{Layout: Layout{Prefix: "review.wait", Stream: "REVIEW", Partitions: 1, Groups: 1}})
	if err != nil {
		t.Fatal(err)
	}
	e.ready.Store(true)
	current := Record{Lifecycle: "NEWLIFE", ID: "s1", Status: StatusWaiting, Version: 2}
	raw.answer = func(string, []byte) ([]byte, error) {
		// Waiter has registered; emulate a lagging terminal follower delivering the previous lifetime.
		e.watch.observe(Record{Lifecycle: "OLDLIFE", ID: "s1", Status: StatusCompleted, Version: 10})
		e.watch.observe(Record{Lifecycle: "NEWLIFE", ID: "s1", Status: StatusCompleted, Version: 4})
		return json.Marshal(queryReply{V: WireVersion, Part: 0, Found: true, Record: &current})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := e.WaitTerminal(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Lifecycle != current.Lifecycle {
		t.Fatalf("waiting for NEWLIFE v2 returned previous lifetime: lifecycle=%s version=%d status=%v", got.Lifecycle, got.Version, got.Status)
	}
}

// 查询与登记之间的终态不会丢失；补查不得静默绑定下一轮。
func TestWaitTerminalBindingBoundaries(t *testing.T) {
	for _, scenario := range []string{"terminal_gap", "changed", "missing_then_new", "query_unavailable", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			raw := &stubRaw{}
			e, err := NewEngine(stubLog{}, raw, Options{Layout: Layout{Prefix: "review.wait", Stream: "REVIEW", Partitions: 1, Groups: 1}})
			if err != nil {
				t.Fatal(err)
			}
			e.ready.Store(true)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			raw.answer = func(string, []byte) ([]byte, error) {
				calls++
				record := Record{Lifecycle: "NEWLIFE", ID: "s1", Status: StatusWaiting, Version: 2}
				if scenario == "cancel" {
					cancel()
					return nil, ErrPartitionUnavailable
				}
				if calls == 1 && scenario == "missing_then_new" {
					e.watch.observe(Record{Lifecycle: "OLDLIFE", ID: "s1", Status: StatusCompleted, Version: 99})
					return json.Marshal(queryReply{V: WireVersion, Part: 0, Found: false})
				}
				if calls == 2 && scenario == "query_unavailable" {
					return nil, ErrPartitionUnavailable
				}
				if calls >= 2 {
					record.Status, record.Version = StatusCompleted, 4
					if scenario == "changed" {
						record.Lifecycle = "ANOTHER"
					}
				}
				return json.Marshal(queryReply{V: WireVersion, Part: 0, Found: true, Record: &record})
			}
			got, err := e.WaitTerminal(ctx, "s1")
			switch scenario {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case "changed":
				if !errors.Is(err, ErrLifecycleChanged) {
					t.Fatalf("changed: %+v %v", got, err)
				}
			default:
				if err != nil || got.Lifecycle != "NEWLIFE" || got.Version != 4 {
					t.Fatalf("terminal: %+v %v", got, err)
				}
			}
			e.watch.mu.Lock()
			defer e.watch.mu.Unlock()
			if len(e.watch.waiters) != 0 {
				t.Fatal("waiter leaked")
			}
		})
	}
}

func TestTerminalWatchRejectsOldLifecycleBeforeAndAfterCurrent(t *testing.T) {
	for _, oldFirst := range []bool{true, false} {
		w := newTerminalWatch()
		waiter, cancel := w.register("s1", "new")
		old := Record{ID: "s1", Lifecycle: "old", Version: 100, Status: StatusCompleted}
		current := Record{ID: "s1", Lifecycle: "new", Version: 4, Status: StatusCompleted}
		if oldFirst {
			w.observe(old)
		}
		w.observe(current)
		w.observe(old)
		got := waiter.newerThan(2)
		cancel()
		if got == nil || got.Lifecycle != "new" || got.Version != 4 {
			t.Fatalf("oldFirst=%v got=%+v", oldFirst, got)
		}
	}
}
