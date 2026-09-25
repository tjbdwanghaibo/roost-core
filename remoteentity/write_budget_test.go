package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestRemoteWriteBudgetIsIndependentAndNonBlocking(t *testing.T) {
	registerAdmissionKind()
	cfg := DefaultConfig()
	cfg.MaxConcurrentWrites = 8
	cfg.AsyncFinalizeCapacity = 64
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	ids := make([]int64, 64)
	for i := range ids {
		e := newTestRemoteEntity(int64(8200+i), 1, 236)
		loader.add(e)
		ids[i] = e.GUId()
	}
	m.SetBackend(loader)
	m.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.StopFinalizer(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		batch entity.RemoteWriteBatch
		err   error
	}
	results := make(chan result, len(ids))
	var group sync.WaitGroup
	for _, id := range ids {
		group.Go(func() { batch, err := m.PrepareRemoteWriteBatch(ctx, []int64{id}); results <- result{batch, err} })
	}
	group.Wait()
	close(results)
	var accepted []entity.RemoteWriteBatch
	for result := range results {
		if result.err == nil {
			accepted = append(accepted, result.batch)
		} else if !errors.Is(result.err, entity.ErrRemoteOverloaded) {
			t.Errorf("unexpected refusal: %v", result.err)
		}
	}
	stats := m.Stats()
	if len(accepted) != 8 || stats.WritesInFlight != 8 || stats.WriteRejected != 56 || stats.WriteLimit != 8 || stats.Wrappers != 8 {
		t.Errorf("accepted=%d stats=%+v", len(accepted), stats)
	}
	for _, batch := range accepted {
		if err := batch.Abort(ctx, errors.New("test")); err != nil {
			t.Error(err)
		}
		if err := batch.Close(ctx); err != nil {
			t.Error(err)
		}
		_ = batch.Close(ctx)
	}
	if m.Stats().WritesInFlight != 0 {
		t.Fatal("Close leaked write capacity")
	}
	batch, err := m.PrepareRemoteWriteBatch(ctx, ids[:1])
	if err != nil {
		t.Fatal(err)
	}
	_ = batch.Abort(ctx, errors.New("test"))
	_ = batch.Close(ctx)
	// 失败发生于资源预算之后，仍必须归还额度。
	missing := testRemoteFullIDWithKind(8999, 1, 236)
	if _, err := m.PrepareRemoteWriteBatch(ctx, []int64{missing}); err == nil {
		t.Fatal("missing entity unexpectedly prepared")
	}
	if m.Stats().WritesInFlight != 0 {
		t.Fatal("failed Prepare leaked write capacity")
	}
}

func TestRemoteWriteBudgetConfigCompatibility(t *testing.T) {
	for _, tc := range []struct{ limit, finalize, want int }{{0, 17, 17}, {8, 17, 8}, {32, 17, 17}, {0, 0, 4096}} {
		cfg := DefaultConfig()
		cfg.MaxConcurrentWrites = tc.limit
		cfg.AsyncFinalizeCapacity = tc.finalize
		m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
		if got := m.Stats().WriteLimit; got != tc.want {
			t.Fatalf("config=%+v effective=%d", tc, got)
		}
	}
}
