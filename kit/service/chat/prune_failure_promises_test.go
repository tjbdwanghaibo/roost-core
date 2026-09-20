package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// U-0122 · C5（静默吞错）· classscan C5 扫描 / 观察 O-5。
//
// 留存循环对 Prune 的失败只打日志、下个 tick 重试；成功时报 message.evicted.*，
// 失败却不报任何指标——一个一直失败的 prune 在面板上就是一个没什么可清的频道。
// 冲突（ErrConflict）另有 conflict:prune 计数，不重复计。

type failingChannelState struct {
	versionstore.Store[string, channelState]
	err error
}

func (s failingChannelState) Update(context.Context, string, versionstore.Mutate[channelState]) (versionstore.Versioned[channelState], bool, error) {
	return versionstore.Versioned[channelState]{}, false, s.err
}

func TestPruneFailureIsCountedNotJustReturned(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("channel state: connection reset")
	store, err := NewStore(failingChannelState{Store: versionstore.NewMemoryStore[string, channelState](), err: wire}, Config{
		Policy: allowAllPolicy{}, Bodies: testRegistry(t), Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err := store.Resolve(Channel{Kind: ChannelWorld, Target: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(context.Background(), world, PruneBatch); !errors.Is(err, wire) {
		t.Fatalf("Prune = %v, want the store error", err)
	}
	if got := recorder.Count("dropped:prune.failed"); got != 1 {
		t.Fatalf("prune.failed counted %d, want 1 (events: %s)", got, recorder.Events())
	}
	if got := recorder.Count("conflict:prune"); got != 0 {
		t.Fatalf("a wire error was counted as a conflict %d times", got)
	}

	// 冲突走冲突计数，不算失败。
	conflicting, err := NewStore(failingChannelState{Store: versionstore.NewMemoryStore[string, channelState](), err: versionstore.ErrConflict}, Config{
		Policy: allowAllPolicy{}, Bodies: testRegistry(t), Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflicting.Prune(context.Background(), world, PruneBatch); !errors.Is(err, versionstore.ErrConflict) {
		t.Fatalf("Prune under conflict = %v", err)
	}
	if recorder.Count("dropped:prune.failed") != 1 || recorder.Count("conflict:prune") != 1 {
		t.Fatalf("conflict accounting: %s", recorder.Events())
	}
}
