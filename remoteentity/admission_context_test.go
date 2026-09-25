package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type contextOwnershipStore struct {
	*mockMarkerStore
	mu        sync.Mutex
	deadlines []time.Time
	entered   chan context.Context
	release   chan struct{}
}

func (s *contextOwnershipStore) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	deadline, _ := ctx.Deadline()
	s.mu.Lock()
	s.deadlines = append(s.deadlines, deadline)
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- ctx
		select {
		case <-ctx.Done():
			return entity.RemoteEntityMarkerLease{}, false, ctx.Err()
		case <-s.release:
			return entity.RemoteEntityMarkerLease{}, false, errors.New("test released")
		}
	}
	return s.mockMarkerStore.GetOwnership(ctx, id)
}

var registerAdmissionKind = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 236, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
})

func admissionManager(t *testing.T, store *contextOwnershipStore, budget time.Duration) (*Manager, []int64) {
	t.Helper()
	registerAdmissionKind()
	cfg := DefaultConfig()
	cfg.OpTimeout = budget
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	ids := make([]int64, 3)
	for i := range ids {
		live := newTestRemoteEntity(int64(7310+i), 1, 236)
		loader.add(live)
		ids[i] = live.GUId()
	}
	m.SetBackend(loader)
	m.SetOwnershipStore(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.StopFinalizer(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return m, ids
}
func TestColdRemoteAdmissionHonorsCallerCancellation(t *testing.T) {
	for _, ownership := range []bool{false, true} {
		name := "write"
		if ownership {
			name = "ownership"
		}
		t.Run(name, func(t *testing.T) {
			store := &contextOwnershipStore{mockMarkerStore: newMockMarkerStore(), entered: make(chan context.Context, 4), release: make(chan struct{})}
			defer close(store.release)
			m, ids := admissionManager(t, store, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if ownership {
					_, err := m.ClaimRemoteOwnership(ctx, ids[0])
					done <- err
				} else {
					_, err := m.PrepareRemoteWriteBatch(ctx, ids[:1])
					done <- err
				}
			}()
			observed := awaitChan(t, store.entered, "initial ownership read")
			cancel()
			select {
			case <-observed.Done():
			case <-time.After(150 * time.Millisecond):
				t.Fatal("cold ownership read detached from caller cancellation")
			}
			if err := awaitChan(t, done, "canceled admission"); !errors.Is(err, context.Canceled) {
				t.Fatalf("admission=%v", err)
			}
		})
	}
}
func TestRemoteBatchSharesOneBoundedAdmissionDeadline(t *testing.T) {
	store := &contextOwnershipStore{mockMarkerStore: newMockMarkerStore()}
	m, ids := admissionManager(t, store, 100*time.Millisecond)
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := time.Now()
	batch, err := m.PrepareRemoteWriteBatch(parent, ids)
	if err != nil {
		t.Fatal(err)
	}
	_ = batch.Abort(context.Background(), errors.New("test complete"))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deadlines) != len(ids) {
		t.Fatalf("authority reads=%d want=%d (cold construction must not perform I/O)", len(store.deadlines), len(ids))
	}
	first := store.deadlines[0]
	if first.After(started.Add(150 * time.Millisecond)) {
		t.Fatalf("caller deadline bypassed operation budget: %v", first.Sub(started))
	}
	for _, deadline := range store.deadlines[1:] {
		if !deadline.Equal(first) {
			t.Fatal("each entity received a fresh operation budget")
		}
	}
}
