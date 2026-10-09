package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func TestGraceWindowRejectsSubsecond(t *testing.T) {
	s, _ := newActivityService(t)
	cfg := s.cfg
	cfg.GraceWindow = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond grace window accepted despite Unix-second deadline")
	}
}

type brokenActivityRead struct {
	versionstore.Store[Key, Activity]
	key Key
}

func (s brokenActivityRead) Get(ctx context.Context, key Key) (versionstore.Versioned[Activity], bool, error) {
	if key == s.key {
		return versionstore.Versioned[Activity]{}, false, versionstore.ErrMalformedRecord
	}
	return s.Store.Get(ctx, key)
}

func TestUnreadableActivityDoesNotStallHealthyExpiry(t *testing.T) {
	s, c := newActivityService(t)
	bad, good := activityKey("a-bad"), activityKey("z-good")
	for _, key := range []Key{bad, good} {
		openActivity(t, s, key, 1, 2)
		notify(t, s, key, 1)
	}
	c.advance(time.Minute)
	s.cfg.Activities = brokenActivityRead{Store: s.cfg.Activities, key: bad}
	completed, err := s.AdvanceExpired(context.Background(), good.GroupID, 10)
	if !errors.Is(err, versionstore.ErrMalformedRecord) {
		t.Fatalf("corruption not reported: %v", err)
	}
	if len(completed) != 1 || completed[0].Key != good {
		t.Fatalf("healthy expiry stalled: completed=%v err=%v", completed, err)
	}
	window, _, err := s.cfg.Windows.Get(context.Background(), good.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Value.Keys) != 1 || window.Value.Keys[0] != bad {
		t.Fatalf("bad record must stay for repair: %+v", window.Value)
	}
}

func TestSweepStillRetiresDeliveriesWhenAnotherActivityIsUnreadable(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	bad, good := activityKey("a-bad"), activityKey("z-good")
	openActivity(t, s, bad, 1)
	openActivity(t, s, good, 1)
	notify(t, s, good, 1)
	dispatch, err := s.AttemptDispatch(ctx, good, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AckDispatch(ctx, good, 1, dispatch.Token); err != nil {
		t.Fatal(err)
	}
	s.cfg.Activities = brokenActivityRead{Store: s.cfg.Activities, key: bad}
	(&Server{service: s}).sweepGroup(ctx, s, good.GroupID)
	if windowOf(t, s, good.GroupID).delivering(good) {
		t.Fatal("healthy terminal delivery not retired behind corrupt activity")
	}
}

type createOpeningRace struct {
	versionstore.Store[Key, Activity]
}

func (s createOpeningRace) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	// 模拟 opener 在 sweep 的 Get 与 Create 之间完成；Create 返回零值而非赢家值。
	value.Status = StatusComplete
	if _, _, err := s.Store.Create(ctx, key, value); err != nil {
		return versionstore.Versioned[Activity]{}, false, err
	}
	return s.Store.Create(ctx, key, value)
}

func TestOpeningCreateCollisionReadsWinnerBeforeClassification(t *testing.T) {
	s, c := newActivityService(t)
	key := activityKey("racing-opening")
	seedOpening(t, s, OpeningEntry{Key: key, AdmittedAtUnix: c.Now().Unix(), Intent: &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}}})
	c.advance(s.cfg.OpeningGrace + time.Second)
	s.cfg.Activities = createOpeningRace{s.cfg.Activities}
	if _, err := s.AdvanceExpired(context.Background(), key.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	if !windowOf(t, s, key.GroupID).delivering(key) {
		t.Fatal("completed winner not moved to delivering in the same sweep")
	}
}

func TestOperatorCanRetirePermanentlyOfflineGameAndReopenSameDelivery(t *testing.T) {
	s, _ := newActivityService(t)
	ctx, key := context.Background(), activityKey("offline-game")
	openActivity(t, s, key, 1)
	notify(t, s, key, 1)
	before, _, err := s.LookupDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExhaustDispatch(ctx, key, 1, ""); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("missing note accepted: %v", err)
	}
	stopped, err := s.ExhaustDispatch(ctx, key, 1, "game permanently decommissioned")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != DispatchExhausted || stopped.Token != before.Token || stopped.Attempts != 0 || stopped.AdminNote == "" {
		t.Fatalf("operator fabricated attempts or lost identity: %+v", stopped)
	}
	again, err := s.ExhaustDispatch(ctx, key, 1, "retry after lost reply")
	if err != nil || again.AdminNote != stopped.AdminNote {
		t.Fatalf("replay changed terminal record: %+v %v", again, err)
	}
	if _, err := s.AckDispatch(ctx, key, 1, before.Token); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("late ACK accepted: %v", err)
	}
	if retired, err := s.RetireDelivered(ctx, key); err != nil || !retired {
		t.Fatalf("not retired: %v %v", retired, err)
	}
	reopened, err := s.ReopenDispatch(ctx, key, 1, "server restored")
	if err != nil || reopened.State != DispatchPending || reopened.Token != before.Token {
		t.Fatalf("reopen lost identity: %+v %v", reopened, err)
	}
}
