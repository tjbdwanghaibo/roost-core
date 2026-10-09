package activity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type failedProgressMark struct {
	versionstore.Store[RequestKey, ProgressReservation]
}

type pausedParticipantWrite struct {
	versionstore.Store[ParticipantKey, Participant]
	once             atomic.Bool
	entered, release chan struct{}
}

func (s *pausedParticipantWrite) Update(ctx context.Context, k ParticipantKey, m versionstore.Mutate[Participant]) (versionstore.Versioned[Participant], bool, error) {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Update(ctx, k, m)
}
func TestStaleReservedReaderCannotApplyAfterProofWasReclaimed(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = &reviewLostMark{c.Ledger, true} })
	ctx := context.Background()
	key := activityKey("stale-reserved")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("expected mark failure")
	}
	gate := &pausedParticipantWrite{Store: s.cfg.Participants, entered: make(chan struct{}), release: make(chan struct{})}
	s.cfg.Participants = gate
	done := make(chan error, 1)
	go func() { _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); done <- err }()
	<-gate.entered
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err != nil {
		close(gate.release)
		<-done
		t.Fatal(err)
	}
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprintf("new-%d", i), ProgressDelta{Score: 1}); err != nil {
			close(gate.release)
			<-done
			t.Fatal(err)
		}
	}
	before, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || before.Applied("lost") {
		close(gate.release)
		<-done
		t.Fatal("proof was not reclaimed", before, err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || after.Score != before.Score || after.Applies != before.Applies {
		t.Fatalf("stale reader reapplied: before=%+v after=%+v err=%v", before, after, err)
	}
}

func (s failedProgressMark) Update(context.Context, RequestKey, versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	return versionstore.Versioned[ProgressReservation]{}, false, errors.New("mark unavailable")
}

func TestUnconfirmedProgressBackpressureNeverEvictsProof(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = failedProgressMark{c.Ledger} })
	ctx := context.Background()
	key := activityKey("full-proof")
	openActivity(t, s, key, 1)
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprint(i), ProgressDelta{Score: 1}); err == nil {
			t.Fatal("missing mark failure")
		}
	}
	before, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || before.Score != MaxProgressWindow || len(before.PendingRequestIDs) != MaxProgressWindow {
		t.Fatal(before, err)
	}
	// Full inside the TTL is backpressure (ErrProgressBacklog since
	// RR-20261001-05; it was versionstore.ErrConflict before), never eviction.
	if _, err = s.ApplyProgress(ctx, key, "p", "extra", ProgressDelta{Score: 1}); !errors.Is(err, ErrProgressBacklog) {
		t.Fatalf("full proof ignored: %v", err)
	}
	after, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || after.Score != before.Score || !after.Applied("0") {
		t.Fatal(after, err)
	}
	if _, err = s.ApplyProgress(ctx, key, "p", "0", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("mark failure hidden")
	}
	after, _, _ = s.LookupParticipant(ctx, key, "p")
	if after.Score != before.Score {
		t.Fatalf("old pending proof applied twice: %+v", after)
	}
}
