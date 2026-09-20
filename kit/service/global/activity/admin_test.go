package activity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// dispatchStep is comfortably longer than any backoff the fixture configures,
// so each AttemptDispatch below is actually due rather than held.
const dispatchStep = time.Hour

// exhaustDispatch drives one game server's result delivery to
// DispatchExhausted, the way a game that is down longer than the retry budget
// would.
func exhaustDispatch(t *testing.T, s *Service, c *activityClock, key Key, gameSID int32) Dispatch {
	t.Helper()
	ctx := context.Background()
	openActivity(t, s, key, gameSID)
	notify(t, s, key, gameSID)
	for i := 0; i < 8; i++ {
		c.advance(dispatchStep)
		_, _ = s.AttemptDispatch(ctx, key, gameSID)
	}
	dispatch, found, err := s.LookupDispatch(ctx, key, gameSID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no dispatch for activity %s game %d", key, gameSID)
	}
	if dispatch.State != DispatchExhausted {
		t.Fatalf("dispatch is %s after burning the budget, want exhausted", dispatch.State)
	}
	return dispatch
}

// The dead end: an exhausted dispatch is refused by BOTH automatic paths, so a
// game server never receives the result of an activity its players took part
// in and nothing could change that.
func TestAnExhaustedDispatchCanBeReopened(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	before := exhaustDispatch(t, service, c, key, 7)

	// Confirm the dead end is real rather than assumed: both paths refuse.
	if _, err := service.AttemptDispatch(ctx, key, 7); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("AttemptDispatch on an exhausted dispatch returned %v", err)
	}
	if _, err := service.AckDispatch(ctx, key, 7, before.Token); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("AckDispatch on an exhausted dispatch returned %v", err)
	}

	reopened, err := service.ReopenDispatch(ctx, key, 7, "game 7 was down for maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != DispatchPending {
		t.Fatalf("a reopened dispatch is %s, want pending", reopened.State)
	}
	if reopened.Attempts != 0 {
		t.Fatalf("a reopened dispatch has %d attempts, want a fresh budget", reopened.Attempts)
	}
	if reopened.ExhaustedAtUnix != 0 {
		t.Fatal("ExhaustedAtUnix survived the reopen, so the record still says it is exhausted")
	}
	if reopened.Reopens != 1 || reopened.AdminActionAtUnix == 0 {
		t.Fatalf("the intervention was not recorded: %+v", reopened)
	}
	if !strings.Contains(reopened.AdminNote, "down for maintenance") {
		t.Fatalf("the operator note was not recorded: %q", reopened.AdminNote)
	}

	// And it is deliverable and ackable again.
	if _, err := service.AttemptDispatch(ctx, key, 7); err != nil {
		t.Fatalf("a reopened dispatch was not handed out: %v", err)
	}
	acked, err := service.AckDispatch(ctx, key, 7, reopened.Token)
	if err != nil {
		t.Fatalf("a reopened dispatch could not be acked: %v", err)
	}
	if acked.State != DispatchAcked {
		t.Fatalf("after ack the dispatch is %s", acked.State)
	}
}

// The ACK token must survive a reopen.
//
// A game server can have received a result, applied it, and then failed to
// acknowledge — a lost response, a restart between the two — after which the
// dispatch exhausts. Reopening delivers the same result again, and the ONLY
// key the game can deduplicate on is the token. Minting a fresh one would hand
// the same result under a new identity and break exactly the callers that did
// the right thing.
//
// It is the rule mail's claim token follows: server-generated, constant for the
// life of the thing it identifies, so a retry cannot buy a new idempotency key.
func TestReopeningPreservesTheAckToken(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	before := exhaustDispatch(t, service, c, key, 7)
	if before.Token == "" {
		t.Fatal("the fixture produced a dispatch with no token")
	}

	after, err := service.ReopenDispatch(ctx, key, 7, "game back")
	if err != nil {
		t.Fatal(err)
	}
	if after.Token != before.Token {
		t.Fatalf("the ack token changed on reopen (%q -> %q); a game that deduplicated the "+
			"re-delivered result on the token would apply it twice", before.Token, after.Token)
	}
	// Reopening repeatedly must not drift it — and Reopens must ACCUMULATE
	// across interventions rather than reset, because "we reopened this four
	// times and the game still never acked" is the fact that stops someone
	// reopening it a fifth time. Resetting Attempts alone would erase it.
	for i := 0; i < 3; i++ {
		for j := 0; j < 8; j++ {
			c.advance(dispatchStep)
			_, _ = service.AttemptDispatch(ctx, key, 7)
		}
		again, err := service.ReopenDispatch(ctx, key, 7, "still down")
		if err != nil {
			t.Fatal(err)
		}
		if again.Token != before.Token {
			t.Fatalf("the ack token changed on reopen %d", i+2)
		}
		if want := int32(i + 2); again.Reopens != want {
			t.Fatalf("after %d reopens Reopens is %d, want %d", want, again.Reopens, want)
		}
	}
}

// Reopening an ACKED dispatch is refused: the game already confirmed it
// applied the result, which is the one case where re-dispatching is known to be
// a second application rather than a retry.
func TestReopeningRefusesAnAckedOrPendingDispatch(t *testing.T) {
	ctx := context.Background()

	t.Run("acked", func(t *testing.T) {
		service, c := newActivityService(t)
		key := activityKey("act-1")
		exhausted := exhaustDispatch(t, service, c, key, 7)
		if _, err := service.ReopenDispatch(ctx, key, 7, "reopening once"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AttemptDispatch(ctx, key, 7); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AckDispatch(ctx, key, 7, exhausted.Token); err != nil {
			t.Fatal(err)
		}
		err := service.mustReopenFail(ctx, key, 7)
		if !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("reopening an ACKED dispatch returned %v; the game already applied the "+
				"result", err)
		}
	})

	t.Run("pending is refused distinctly", func(t *testing.T) {
		service, _ := newActivityService(t)
		key := activityKey("act-1")
		openActivity(t, service, key, 7)
		notify(t, service, key, 7)
		err := service.mustReopenFail(ctx, key, 7)
		if !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("reopening a PENDING dispatch returned %v", err)
		}
		if !strings.Contains(err.Error(), "already retryable") {
			t.Fatalf("the refusal does not say it is already retryable: %v", err)
		}
	})
}

// mustReopenFail attempts a reopen and returns the error, for tests that only
// care that it was refused.
func (s *Service) mustReopenFail(ctx context.Context, key Key, gameSID int32) error {
	_, err := s.ReopenDispatch(ctx, key, gameSID, "attempting")
	return err
}

// An intervention with no recorded reason cannot be reviewed.
func TestReopenDispatchRequiresANote(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	exhaustDispatch(t, service, c, key, 7)
	for _, blank := range []string{"", "  ", "\t"} {
		if _, err := service.ReopenDispatch(ctx, key, 7, blank); !errors.Is(err, ErrAdminNoteRequired) {
			t.Fatalf("a blank note %q returned %v", blank, err)
		}
	}
	if _, err := service.ReopenDispatch(ctx, key, 7, strings.Repeat("x", MaxAdminNoteBytes+1)); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatal("an oversized note was accepted")
	}
	// A refused intervention must leave the dispatch untouched.
	dispatch, _, err := service.LookupDispatch(ctx, key, 7)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.State != DispatchExhausted || dispatch.AdminNote != "" || dispatch.Reopens != 0 {
		t.Fatalf("a refused intervention changed the dispatch: %+v", dispatch)
	}
}

// A dispatch that was never created cannot be reopened, and says so as a
// missing dispatch rather than as an unresolvable one.
func TestReopeningAMissingDispatchIsRefused(t *testing.T) {
	service, _ := newActivityService(t)
	_, err := service.ReopenDispatch(context.Background(), activityKey("nope"), 7, "note")
	if !errors.Is(err, ErrDispatchMissing) {
		t.Fatalf("reopening a dispatch that does not exist returned %v", err)
	}
}

// Two operators, or one clicking twice, must not both reopen.
func TestConcurrentDispatchReopensGrantOneFreshBudget(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	exhaustDispatch(t, service, c, key, 7)

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.ReopenDispatch(ctx, key, 7, "concurrent operators"); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent reopens succeeded, want exactly 1", succeeded, racers)
	}
	dispatch, _, err := service.LookupDispatch(ctx, key, 7)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Reopens != 1 {
		t.Fatalf("Reopens is %d after %d concurrent attempts, want 1", dispatch.Reopens, racers)
	}
}

// Admin is deliberately not on the bus: the capability other processes hold
// must not satisfy it.
func TestTheOperatorSurfaceIsNotOnTheCoordinatorInterface(t *testing.T) {
	var asCoordinator any = Capability(&Service{})
	if _, ok := asCoordinator.(Admin); ok {
		t.Fatal("the capability published to other processes satisfies Admin; a caller over the " +
			"bus could re-dispatch a result, and the bus carries no identity this service can " +
			"verify")
	}
}
