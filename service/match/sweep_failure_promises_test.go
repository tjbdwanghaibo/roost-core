package match

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// U-0121 · C5（静默吞错）· classscan C5 扫描。
//
// 过期 sweep 的失败被后台循环记日志后 continue；成功时报 ticket.expired，失败
// 却不报任何指标——一个一直失败的 sweep 看起来就像一个没有过期票的队列。

type failingQueueState struct {
	versionstore.Store[string, queueState]
	err error
}

func (s failingQueueState) Update(context.Context, string, versionstore.Mutate[queueState]) (versionstore.Versioned[queueState], bool, error) {
	return versionstore.Versioned[queueState]{}, false, s.err
}

func TestSweepFailureIsCountedNotJustReturned(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("queue state: connection reset")
	store, err := NewStore(failingQueueState{Store: versionstore.NewMemoryStore[string, queueState](), err: wire}, Config{
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }, Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sweep(context.Background(), ranked(), 10); !errors.Is(err, wire) {
		t.Fatalf("Sweep = %v, want the store error", err)
	}
	if got := recorder.Count("dropped:sweep.failed"); got != 1 {
		t.Fatalf("sweep.failed counted %d, want 1 (events: %s)", got, recorder.Events())
	}
	if got := recorder.Count("dropped:ticket.expired"); got != 0 {
		t.Fatalf("a failed sweep reported %d expired tickets", got)
	}
}
