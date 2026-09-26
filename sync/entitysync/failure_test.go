package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestPolicyFailureRetainsCauseAndCountsOnce(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	cause := errors.New("policy admission rejected")
	cancel := m.RegisterPolicy(func() error { return cause })
	if err := m.Flush(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("lost returned cause: %v", err)
	}
	if !errors.Is(m.LastError(), cause) || m.Counters().FlushFailures != 1 {
		t.Fatalf("lost policy failure: count=%d last=%v", m.Counters().FlushFailures, m.LastError())
	}
	cancel()
	mustFlush(t, m)
	if m.Counters().FlushFailures != 1 || !errors.Is(m.LastError(), cause) {
		t.Fatal("success erased history or counted again")
	}
}

// 在部分准入后取消，等同于运行循环 Stop 遇到尚在执行的 Push。
// 计数保留取消事实，但下一轮必须能补齐，不能吞掉已交付的前缀。
func TestCancelledFlushRetainsAdmissionCauseAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRecordingTransport()
	calls := 0
	transport := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
		calls++
		if calls == 2 {
			cancel()
			return ctx.Err()
		}
		return r.Push(ctx, sid, raw)
	})
	m := newTestManager(t, transport, ManagerConfig{})
	open(t, m, 1, 2)
	packs := 0
	state := testSubject(t, 1, &packs)
	if err := m.Register(state); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
	}
	err := m.Flush(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(m.LastError(), context.Canceled) || m.Counters().FlushFailures != 1 {
		t.Fatalf("cancel evidence lost: %v / %v", err, m.LastError())
	}
	oneFrame(t, r, 1)
	mustFlush(t, m)
	for _, sid := range []SessionID{1, 2} {
		if f := oneFrame(t, r, sid); !f.updates[1].Full {
			t.Fatalf("retry needs full for %d", sid)
		}
	}
	if m.Counters().FlushFailures != 1 || m.Stats().Pending != 0 {
		t.Fatal("failed cancellation recovery")
	}
}
