package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type releasedClaimBetweenCreateAndRead struct {
	ClaimStore
	first bool
}

type lostRunCreation struct {
	RunStore
	id string
}

func (s *lostRunCreation) Create(ctx context.Context, key string, value Run) (versionstore.Versioned[Run], bool, error) {
	s.id = key
	if _, _, err := s.RunStore.Create(ctx, key, value); err != nil {
		return versionstore.Versioned[Run]{}, false, err
	}
	return versionstore.Versioned[Run]{}, false, versionstore.ErrOutcomeUnknown
}
func (s *lostRunCreation) PendingAdmissions(context.Context, int64, int) ([]string, error) {
	return []string{s.id}, nil
}
func (s *lostRunCreation) DeleteIf(ctx context.Context, id string, expect versionstore.Versioned[Run], match func(Run) bool) error {
	return s.RunStore.(versionstore.ConditionalDeleter[string, Run]).DeleteIf(ctx, id, expect, match)
}

func TestLostRunCreateReplyCanBeReclaimedWithoutOwnerClaim(t *testing.T) {
	var store *lostRunCreation
	h := newHarness(t, func(c *Config) { store = &lostRunCreation{RunStore: c.Runs}; c.Runs = store })
	ctx := context.Background()
	if _, err := h.service.Enter(ctx, 1, enterReq("lost-run")); !errors.Is(err, versionstore.ErrOutcomeUnknown) {
		t.Fatalf("want unknown: %v", err)
	}
	if _, found, err := h.claims.Get(ctx, 1); err != nil || found {
		t.Fatalf("unexpected claim: %v %v", found, err)
	}
	h.clock.advance(time.Hour)
	if _, err := h.service.SweepPending(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(ctx, store.id); err != nil || found {
		t.Fatalf("unreachable unadmitted run leaked: found=%v err=%v", found, err)
	}
}

type slowClaimCreation struct {
	ClaimStore
	delay func()
}

func (s slowClaimCreation) Create(ctx context.Context, key int64, value Claim) (versionstore.Versioned[Claim], bool, error) {
	s.delay()
	return s.ClaimStore.Create(ctx, key, value)
}
func TestSlowClaimCannotAdmitExpiredRun(t *testing.T) {
	h := newHarness(t)
	h.service.cfg.Claims = slowClaimCreation{ClaimStore: h.claims, delay: func() { h.clock.advance(time.Hour) }}
	if run, err := h.service.Enter(context.Background(), 1, enterReq("slow-claim")); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("expired run handed to business: %+v %v", run, err)
	}
}

func (store *releasedClaimBetweenCreateAndRead) Create(ctx context.Context, key int64, value Claim) (versionstore.Versioned[Claim], bool, error) {
	if !store.first {
		store.first = true
		return versionstore.Versioned[Claim]{}, false, nil
	}
	return store.ClaimStore.Create(ctx, key, value)
}
func TestEnterRetakesClaimReleasedBeforeRead(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.Claims = &releasedClaimBetweenCreateAndRead{ClaimStore: cfg.Claims} })
	run, err := h.service.Enter(context.Background(), 1, enterReq("released-race"))
	if err != nil || run.ID == "" {
		t.Fatalf("claim already released but Enter failed: %v", err)
	}
}
func TestRunTTLRejectsSubsecond(t *testing.T) {
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.TTL = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond run TTL accepted despite Unix-second deadline")
	}
}

func TestRunCodecRequiresAdmissionFormat(t *testing.T) {
	codec := runCodec{}
	if _, err := codec.Decode([]byte(`{"id":"old","state":"open"}`)); err == nil {
		t.Fatal("legacy run accepted")
	}
	want := Run{ID: "pending", AdmissionPending: true}
	raw, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.Decode(raw)
	if err != nil || got.ID != want.ID || !got.AdmissionPending {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
func TestAdmissionSweepCannotDeleteAdmittedRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run, err := h.service.Enter(ctx, 1, enterReq("admitted"))
	if err != nil {
		t.Fatal(err)
	}
	h.service.cfg.Runs = &lostRunCreation{RunStore: h.service.cfg.Runs, id: run.ID}
	h.clock.advance(time.Hour)
	if _, err := h.service.SweepPending(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.runs.Get(ctx, run.ID); err != nil || !found {
		t.Fatalf("stale index deleted admitted run: %v %v", found, err)
	}
}
