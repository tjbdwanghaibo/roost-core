package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// 去掉独立 Committer 后，在正式 Projector 上固定持锁、重试和并发回放契约。
type singlePathStore struct {
	mu      sync.Mutex
	calls   int
	failure error
}

func (s *singlePathStore) Project(context.Context, coredata.CommitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failure
}

func TestProjectorSinglePathHoldsRetriesAndSerializesReplay(t *testing.T) {
	for _, policy := range []corenest.DurabilityPolicy{corenest.DurabilityStrict, corenest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			ctx := context.Background()
			store := &singlePathStore{}
			p, wal := manualProjector(t, store)
			record := projectorRecord(61, false)
			record.Durability = policy.Record()
			if policy == corenest.DurabilityPipelined {
				ticket, err := p.Enqueue(ctx, record)
				if err != nil {
					t.Fatal(err)
				}
				<-ticket.Done()
				if err := ticket.Err(); err != nil {
					t.Fatal(err)
				}
			} else if err := p.Commit(ctx, record); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ReplayPass(ctx); !errors.Is(err, errProjectorTransactionHeld) {
				t.Fatalf("held replay=%v", err)
			}
			if store.calls != 0 {
				t.Fatal("projected before transaction released its entity locks")
			}
			p.TransactionReleased(record.ID)
			retry := errors.New("temporary projection failure")
			store.failure = retry
			if _, err := p.ReplayPass(ctx); !errors.Is(err, retry) {
				t.Fatalf("projection error=%v", err)
			}
			assertWALReplayCount(t, wal, 1)
			store.failure = nil
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					if _, err := p.ReplayPass(ctx); err != nil {
						t.Errorf("replay: %v", err)
					}
				})
			}
			wg.Wait()
			if store.calls != 2 {
				t.Fatalf("calls=%d, want one failed attempt and one successful projection", store.calls)
			}
			assertWALReplayCount(t, wal, 0)
		})
	}
}
