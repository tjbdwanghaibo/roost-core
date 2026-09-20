package activity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/errcode"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// activityClock is the injected clock for the activity half. Named apart from
// the routing half's clock so both halves can be exercised in one package
// without one test's time travel moving the other's deadlines.
type activityClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *activityClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *activityClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newActivityService(t *testing.T, mutate ...func(*Config)) (*Service, *activityClock) {
	t.Helper()
	c := &activityClock{now: time.Unix(1_700_000_000, 0)}
	cfg := Config{
		Activities:          versionstore.NewMemoryStore[Key, Activity](),
		Participants:        versionstore.NewMemoryStore[ParticipantKey, Participant](),
		Ledger:              versionstore.NewMemoryStore[RequestKey, ProgressReservation](),
		Audits:              versionstore.NewMemoryStore[Key, NotifyAuditLog](),
		Dispatches:          versionstore.NewMemoryStore[DispatchKey, Dispatch](),
		Windows:             versionstore.NewMemoryStore[string, Window](),
		GraceWindow:         30 * time.Second,
		ReservationTTL:      10 * time.Minute,
		DispatchBackoff:     5 * time.Second,
		DispatchMaxAttempts: 3,
		Now:                 c.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, c
}

func activityKey(id string) Key {
	return Key{GroupID: "group-a", ActivityID: id, Phase: PhaseClose}
}

func openActivity(t *testing.T, s *Service, key Key, expected ...int32) Activity {
	t.Helper()
	activity, err := s.OpenActivity(context.Background(), key, expected)
	if err != nil {
		t.Fatal(err)
	}
	return activity
}

// notify is a fatal-on-error notify, for arranging state.
func notify(t *testing.T, s *Service, key Key, gameSID int32) Activity {
	t.Helper()
	activity, err := s.NotifyPhase(context.Background(), key, gameSID)
	if err != nil {
		t.Fatal(err)
	}
	return activity
}

// --- opening ---

func TestOpenActivityIsInsertOnlyAndBounded(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")

	opened := openActivity(t, service, key, 1, 2, 3)
	if opened.Status != StatusPending {
		t.Fatalf("a new activity is %q, want pending", opened.Status)
	}
	// The grace deadline does not exist until a game notifies: global must not
	// hold a timeline of its own.
	if opened.GraceDeadlineUnix != 0 || opened.FirstNotifyAtUnix != 0 {
		t.Fatalf("opening scheduled an advance: %+v", opened)
	}

	if _, err := service.OpenActivity(ctx, key, []int32{1, 2}); !errors.Is(err, ErrExists) {
		t.Fatalf("reopening returned %v, want ErrExists", err)
	}
	// The refused reopen did not narrow the expected set.
	current, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if len(current.ExpectedGameSIDs) != 3 {
		t.Fatalf("the refused reopen changed the expected set: %+v", current)
	}

	tooMany := make([]int32, MaxExpectedGames+1)
	for i := range tooMany {
		tooMany[i] = int32(i + 1)
	}
	for _, testCase := range []struct {
		label    string
		key      Key
		expected []int32
		want     error
	}{
		{"empty group", Key{ActivityID: "a", Phase: PhaseClose}, []int32{1}, ErrInvalid},
		{"empty activity id", Key{GroupID: "g", Phase: PhaseClose}, []int32{1}, ErrInvalid},
		{"empty phase", Key{GroupID: "g", ActivityID: "a"}, []int32{1}, ErrInvalid},
		{"separator in id", Key{GroupID: "g", ActivityID: "a/b", Phase: PhaseClose}, []int32{1}, ErrInvalid},
		{"oversized id", Key{GroupID: "g", ActivityID: strings.Repeat("x", MaxActivityIDLen+1), Phase: PhaseClose}, []int32{1}, ErrInvalid},
		{"empty expected set", activityKey("act-2"), nil, ErrInvalid},
		{"expected set too large", activityKey("act-3"), tooMany, ErrInvalid},
		{"zero game sid", activityKey("act-4"), []int32{1, 0}, ErrInvalid},
		{"duplicate game sid", activityKey("act-5"), []int32{1, 1}, ErrInvalid},
	} {
		if _, err := service.OpenActivity(ctx, testCase.key, testCase.expected); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: got %v, want %v", testCase.label, err, testCase.want)
		}
	}
}

// The pending window is what the back-stop sweep scans, so it is bounded by
// count and a full window refuses new opens loudly instead of admitting an
// activity nothing would ever sweep.
func TestPendingWindowIsBoundedAndCountsRefusals(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	for i := 0; i < MaxPendingActivities; i++ {
		openActivity(t, service, activityKey(fmt.Sprintf("act-%d", i)), 1, 2)
	}
	_, err := service.OpenActivity(ctx, activityKey("one-too-many"), []int32{1, 2})
	if !errors.Is(err, ErrBacklog) {
		t.Fatalf("the %dth open returned %v, want ErrBacklog", MaxPendingActivities+1, err)
	}
	// The refusal is counted in the window record, not only returned to the
	// caller that happened to lose.
	window, found, err := service.cfg.Windows.Get(ctx, "group-a")
	if err != nil || !found {
		t.Fatalf("window read: found=%v err=%v", found, err)
	}
	if window.Value.RefusedOpens == 0 {
		t.Fatal("a refused open was not counted, so a backlog is invisible")
	}
	if len(window.Value.Keys) != MaxPendingActivities {
		t.Fatalf("window holds %d keys, want %d", len(window.Value.Keys), MaxPendingActivities)
	}
	// And the refused activity was not created.
	if _, found, err := service.LookupActivity(ctx, activityKey("one-too-many")); err != nil || found {
		t.Fatalf("the refused open created an activity: found=%v err=%v", found, err)
	}
}

// --- first notify, immediate completion ---

// "第一个 game 通知后创建 collecting snapshot": the first notify is what turns
// a declaration into a collecting aggregation and what starts the grace
// window.
func TestFirstNotifyCreatesTheCollectingSnapshot(t *testing.T) {
	service, c := newActivityService(t)
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2, 3)

	first := notify(t, service, key, 1)
	if first.Status != StatusCollecting {
		t.Fatalf("status after the first notify is %q, want collecting", first.Status)
	}
	wantDeadline := c.Now().Add(30 * time.Second).Unix()
	if first.GraceDeadlineUnix != wantDeadline {
		t.Fatalf("grace deadline = %d, want %d (now + grace window)", first.GraceDeadlineUnix, wantDeadline)
	}
	if first.FirstNotifyAtUnix != c.Now().Unix() {
		t.Fatalf("first notify time = %d, want %d", first.FirstNotifyAtUnix, c.Now().Unix())
	}
	if len(first.NotifiedGameSIDs) != 1 || first.NotifiedGameSIDs[0] != 1 {
		t.Fatalf("collecting snapshot = %+v", first.NotifiedGameSIDs)
	}

	// A later notify does not move the deadline: the window is measured from
	// the first notify, so a trickle of reports cannot extend it forever.
	c.advance(10 * time.Second)
	second := notify(t, service, key, 2)
	if second.GraceDeadlineUnix != wantDeadline {
		t.Fatalf("the second notify moved the deadline to %d", second.GraceDeadlineUnix)
	}
	if second.Status != StatusCollecting {
		t.Fatalf("status = %q with one game outstanding", second.Status)
	}
}

// "收齐 expected games 立即完成", and the completion creates one dispatch per
// expected game.
func TestCollectingEveryExpectedGameCompletesImmediately(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2, 3)

	notify(t, service, key, 1)
	notify(t, service, key, 2)
	completed := notify(t, service, key, 3)

	if completed.Status != StatusComplete {
		t.Fatalf("status = %q after every expected game notified, want complete", completed.Status)
	}
	if completed.CompletionReason != CompletedCollected {
		t.Fatalf("completion reason = %q, want %q", completed.CompletionReason, CompletedCollected)
	}
	if completed.CompletedAtUnix != c.Now().Unix() {
		t.Fatalf("completed at %d, want %d — completion is immediate, not swept",
			completed.CompletedAtUnix, c.Now().Unix())
	}
	if len(completed.MissingGameSIDs()) != 0 {
		t.Fatalf("a collected aggregation reports missing games: %+v", completed.MissingGameSIDs())
	}
	// One retryable delivery per expected game, each with its own token.
	tokens := map[string]bool{}
	for _, gameSID := range []int32{1, 2, 3} {
		dispatch, found, err := service.LookupDispatch(ctx, key, gameSID)
		if err != nil || !found {
			t.Fatalf("dispatch for game %d: found=%v err=%v", gameSID, found, err)
		}
		if dispatch.State != DispatchPending || dispatch.Token == "" {
			t.Fatalf("dispatch for game %d = %+v", gameSID, dispatch)
		}
		if dispatch.Result.Reason != CompletedCollected || dispatch.Result.CompletedAtUnix == 0 {
			t.Fatalf("dispatch payload for game %d = %+v", gameSID, dispatch.Result)
		}
		if tokens[dispatch.Token] {
			t.Fatalf("two dispatches share an ack token, so one game can ack another's: %q", dispatch.Token)
		}
		tokens[dispatch.Token] = true
	}
	// The completed activity leaves the sweep's window.
	pending, err := service.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a completed activity is still in the sweep window: %+v", pending)
	}
}

// A redelivered notify from a game that already reported is a no-op, and is
// NOT audited: it is a healthy client retrying, and burying the real refusals
// under retry noise is its own kind of missing audit.
func TestDuplicateNotifyIsAnUnauditedNoOp(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	first := notify(t, service, key, 1)
	again := notify(t, service, key, 1)
	if len(again.NotifiedGameSIDs) != 1 {
		t.Fatalf("a redelivered notify was counted twice: %+v", again.NotifiedGameSIDs)
	}
	if again.UpdatedAtUnix != first.UpdatedAtUnix {
		t.Fatal("a redelivered notify wrote to the activity")
	}
	if again.Status != StatusCollecting {
		t.Fatalf("status = %q, want collecting: a duplicate must not complete a set of two", again.Status)
	}
	audits, err := service.NotifyAudits(ctx, key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 0 {
		t.Fatalf("a duplicate notify was audited as a refusal: %+v", audits)
	}
}

// --- notify refusal audit ---

// The confirmed defect: notify audit was documented but written on only some
// refusal paths. Every refusal here must leave a record, so this table walks
// all four reasons and asserts both the coded error and the audit.
func TestEveryRefusedNotifyWritesAnAudit(t *testing.T) {
	for _, testCase := range []struct {
		label       string
		arrange     func(t *testing.T, s *Service, c *activityClock, key Key)
		gameSID     int32
		wantErr     error
		wantRefusal NotifyRefusal
		wantCode    int32
	}{
		{
			label:       "unknown activity",
			arrange:     func(*testing.T, *Service, *activityClock, Key) {},
			gameSID:     1,
			wantErr:     ErrMissing,
			wantRefusal: RefusalUnknownActivity,
			wantCode:    CodeMissing,
		},
		{
			label: "game outside the expected set",
			arrange: func(t *testing.T, s *Service, _ *activityClock, key Key) {
				openActivity(t, s, key, 1, 2)
			},
			gameSID:     99,
			wantErr:     ErrNotifyUnexpected,
			wantRefusal: RefusalUnexpectedGame,
			wantCode:    CodeNotifyUnexpected,
		},
		{
			label: "late, after the grace window closed",
			arrange: func(t *testing.T, s *Service, c *activityClock, key Key) {
				openActivity(t, s, key, 1, 2)
				notify(t, s, key, 1)
				c.advance(31 * time.Second)
			},
			gameSID:     2,
			wantErr:     ErrNotifyLate,
			wantRefusal: RefusalLate,
			wantCode:    CodeNotifyLate,
		},
		{
			label: "stale status, the aggregation already completed",
			arrange: func(t *testing.T, s *Service, c *activityClock, key Key) {
				openActivity(t, s, key, 1, 2)
				notify(t, s, key, 1)
				c.advance(31 * time.Second)
				completed, err := s.AdvanceExpired(context.Background(), key.GroupID, 10)
				if err != nil || len(completed) != 1 {
					t.Fatalf("arrange sweep: %d completed, err=%v", len(completed), err)
				}
			},
			gameSID:     2,
			wantErr:     ErrStatus,
			wantRefusal: RefusalStaleStatus,
			wantCode:    CodeStatus,
		},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			service, c := newActivityService(t)
			ctx := context.Background()
			key := activityKey("act-1")
			testCase.arrange(t, service, c, key)

			before, _, err := service.LookupActivity(ctx, key)
			if err != nil {
				t.Fatal(err)
			}

			_, err = service.NotifyPhase(ctx, key, testCase.gameSID)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("notify returned %v, want %v", err, testCase.wantErr)
			}
			if got := Code(err); got != testCase.wantCode {
				t.Fatalf("code = %d, want %d", got, testCase.wantCode)
			}

			audits, err := service.NotifyAudits(ctx, key, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(audits) != 1 {
				t.Fatalf("%d audit records after one refusal, want 1", len(audits))
			}
			audit := audits[0]
			if audit.Refusal != testCase.wantRefusal {
				t.Fatalf("audit refusal = %q, want %q", audit.Refusal, testCase.wantRefusal)
			}
			if audit.GameSID != testCase.gameSID || audit.Key != key {
				t.Fatalf("audit does not identify the notification: %+v", audit)
			}
			if audit.Seq == 0 || audit.AtUnix == 0 {
				t.Fatalf("audit is not stamped: %+v", audit)
			}

			// A refusal changes nothing about the aggregation.
			after, found, err := service.LookupActivity(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if found && (len(after.NotifiedGameSIDs) != len(before.NotifiedGameSIDs) ||
				after.Status != before.Status || after.UpdatedAtUnix != before.UpdatedAtUnix) {
				t.Fatalf("a refused notify changed the activity: %+v -> %+v", before, after)
			}
		})
	}
}

// The refusal and the audit are one code path, so a second refusal appends a
// second record with a fresh sequence rather than overwriting the first.
func TestAuditsAccumulateAndAreBounded(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	for i := 0; i < MaxNotifyAudits+5; i++ {
		if _, err := service.NotifyPhase(ctx, key, 99); !errors.Is(err, ErrNotifyUnexpected) {
			t.Fatalf("refusal %d returned %v", i, err)
		}
	}
	audits, err := service.NotifyAudits(ctx, key, MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != MaxNotifyAudits {
		t.Fatalf("audit log holds %d entries, want the bound %d", len(audits), MaxNotifyAudits)
	}
	// The oldest entries survive a flood — they say when it started — and the
	// drops are counted rather than silent.
	overflow, err := service.AuditOverflow(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if overflow != 5 {
		t.Fatalf("overflow counter = %d, want 5 dropped audits counted", overflow)
	}
	seen := map[uint64]bool{}
	for _, audit := range audits {
		if seen[audit.Seq] {
			t.Fatalf("two audits share sequence %d, so one overwrote the other", audit.Seq)
		}
		seen[audit.Seq] = true
	}
	if audits[0].Seq != uint64(MaxNotifyAudits) {
		t.Fatalf("newest audit has sequence %d, want %d (newest first)", audits[0].Seq, MaxNotifyAudits)
	}
}

// A service with no audit store is refused at construction rather than
// running as a service that cannot audit.
func TestNewActivityServiceRejectsAnIncompleteConfig(t *testing.T) {
	full := func() Config {
		return Config{
			Activities:   versionstore.NewMemoryStore[Key, Activity](),
			Participants: versionstore.NewMemoryStore[ParticipantKey, Participant](),
			Ledger:       versionstore.NewMemoryStore[RequestKey, ProgressReservation](),
			Audits:       versionstore.NewMemoryStore[Key, NotifyAuditLog](),
			Dispatches:   versionstore.NewMemoryStore[DispatchKey, Dispatch](),
			Windows:      versionstore.NewMemoryStore[string, Window](),
		}
	}
	for _, testCase := range []struct {
		label  string
		mutate func(*Config)
	}{
		{"no activity store", func(c *Config) { c.Activities = nil }},
		{"no participant store", func(c *Config) { c.Participants = nil }},
		{"no ledger", func(c *Config) { c.Ledger = nil }},
		{"no audit store", func(c *Config) { c.Audits = nil }},
		{"no dispatch store", func(c *Config) { c.Dispatches = nil }},
		{"no window store", func(c *Config) { c.Windows = nil }},
		{"negative grace window", func(c *Config) { c.GraceWindow = -time.Second }},
		{"negative reservation ttl", func(c *Config) { c.ReservationTTL = -time.Second }},
		{"negative dispatch backoff", func(c *Config) { c.DispatchBackoff = -time.Second }},
		{"negative dispatch attempts", func(c *Config) { c.DispatchMaxAttempts = -1 }},
	} {
		cfg := full()
		testCase.mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatalf("%s was accepted", testCase.label)
		}
	}
	// Zero durations select the documented defaults rather than meaning "no
	// window" or "no retry budget".
	service, err := New(full())
	if err != nil {
		t.Fatal(err)
	}
	if service.cfg.GraceWindow != DefaultGraceWindow ||
		service.cfg.ReservationTTL != DefaultReservationTTL ||
		service.cfg.DispatchBackoff != DefaultDispatchBackoff ||
		service.cfg.DispatchMaxAttempts != DefaultDispatchAttempts {
		t.Fatalf("zero values did not select the defaults: %+v", service.cfg)
	}
}

// --- the back-stop sweep ---

// The sweep only finishes a window a game opened. An activity nobody notified
// stays pending forever, however long the clock runs: global does not drive
// the activity timeline from its own schedule.
func TestAdvanceExpiredNeverCompletesAPendingActivity(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	c.advance(72 * time.Hour)
	completed, err := service.AdvanceExpired(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("the sweep completed %d activity nobody notified: %+v", len(completed), completed)
	}
	current, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if current.Status != StatusPending {
		t.Fatalf("status = %q after three days without a notify, want pending", current.Status)
	}
	// And it is still in the window, so a notify that arrives on day four is
	// still back-stopped.
	pending, err := service.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("the pending activity was pruned from the window: %+v", pending)
	}
}

// "未收齐则由 aggregation runner 扫描 pending 窗口兜底完成", and the sweep is
// bounded per call.
func TestAdvanceExpiredIsBoundedAndCompletesLapsedWindows(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	const activities = 5
	for i := 0; i < activities; i++ {
		key := activityKey(fmt.Sprintf("act-%d", i))
		openActivity(t, service, key, 1, 2)
		notify(t, service, key, 1)
	}
	c.advance(31 * time.Second)

	// Bounded: three, then two, then none. A bound that could be bypassed with
	// a zero limit would not be a bound.
	first, err := service.AdvanceExpired(ctx, "group-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("the sweep completed %d activities with limit 3", len(first))
	}
	second, err := service.AdvanceExpired(ctx, "group-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != activities-3 {
		t.Fatalf("the second sweep completed %d, want %d", len(second), activities-3)
	}
	third, err := service.AdvanceExpired(ctx, "group-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("a third sweep completed %d already-complete activities", len(third))
	}

	for _, completed := range append(first, second...) {
		if completed.Status != StatusComplete {
			t.Fatalf("swept activity is %q: %+v", completed.Status, completed)
		}
		if completed.CompletionReason != CompletedGraceExpired {
			t.Fatalf("completion reason = %q, want %q", completed.CompletionReason, CompletedGraceExpired)
		}
		missing := completed.MissingGameSIDs()
		if len(missing) != 1 || missing[0] != 2 {
			t.Fatalf("a grace-expired result must name who never reported, got %+v", missing)
		}
		// The result was dispatched to every expected game, including the one
		// that never reported — it has to learn it was settled without it.
		for _, gameSID := range []int32{1, 2} {
			dispatch, found, err := service.LookupDispatch(ctx, completed.Key, gameSID)
			if err != nil || !found {
				t.Fatalf("dispatch for game %d: found=%v err=%v", gameSID, found, err)
			}
			if len(dispatch.Result.MissingGameSIDs) != 1 {
				t.Fatalf("dispatched result does not carry the missing games: %+v", dispatch.Result)
			}
		}
	}
	// Everything is out of the sweep window.
	pending, err := service.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("completed activities are still in the window: %+v", pending)
	}
}

// A collecting aggregation whose window has not closed is left alone.
func TestAdvanceExpiredLeavesALiveWindowAlone(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)

	c.advance(29 * time.Second)
	completed, err := service.AdvanceExpired(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("the sweep completed an activity one second before its deadline: %+v", completed)
	}
	// One second later the deadline has passed and the back-stop fires.
	c.advance(2 * time.Second)
	completed, err = service.AdvanceExpired(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 {
		t.Fatalf("the sweep completed %d activities past the deadline, want 1", len(completed))
	}
}

// Every listing and sweep is bounded, and a non-positive limit is an error
// rather than "unlimited".
func TestActivityListingsRejectAnUnboundedLimit(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	for _, limit := range []int{0, -1, MaxPageSize + 1} {
		if _, err := service.AdvanceExpired(ctx, "group-a", limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("AdvanceExpired(limit=%d) = %v, want ErrRangeInvalid", limit, err)
		}
		if _, err := service.PendingActivities(ctx, "group-a", limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("PendingActivities(limit=%d) = %v, want ErrRangeInvalid", limit, err)
		}
		if _, err := service.NotifyAudits(ctx, key, limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("NotifyAudits(limit=%d) = %v, want ErrRangeInvalid", limit, err)
		}
		if _, err := service.DueDispatches(ctx, key, limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("DueDispatches(limit=%d) = %v, want ErrRangeInvalid", limit, err)
		}
	}
}

// --- participant progress ---

// The confirmed defect: the notify path had no idempotency, so a redelivered
// notification double-counted a participant's progress. A replayed request id
// must be a no-op, and the no-op must be observable — the score does not move
// and the apply counter does not move.
func TestAReplayedRequestIDIsANoOp(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	delta := ProgressDelta{Score: 40, Progress: 2}
	applied, err := service.ApplyProgress(ctx, key, "player-1", "req-1", delta)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Score != 40 || applied.Progress != 2 || applied.Applies != 1 {
		t.Fatalf("first apply = %+v", applied)
	}

	for attempt := 0; attempt < 3; attempt++ {
		replayed, err := service.ApplyProgress(ctx, key, "player-1", "req-1", delta)
		if err != nil {
			t.Fatalf("replay %d returned %v, want the current state", attempt, err)
		}
		if replayed.Score != 40 || replayed.Progress != 2 {
			t.Fatalf("replay %d double-counted: %+v", attempt, replayed)
		}
		if replayed.Applies != 1 {
			t.Fatalf("replay %d was applied again: applies = %d", attempt, replayed.Applies)
		}
	}
	// The stored value did not change either — not just the returned one.
	current, found, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if current.Score != 40 || current.Progress != 2 || current.Applies != 1 {
		t.Fatalf("stored participant after three replays = %+v", current)
	}
	// The ledger records the claim as applied, which is what answers a replay
	// without touching the participant record at all.
	reservation, found, err := service.Reservation(ctx, key, "player-1", "req-1")
	if err != nil || !found {
		t.Fatalf("reservation: found=%v err=%v", found, err)
	}
	if reservation.State != ReservationApplied || reservation.AppliedAtUnix == 0 {
		t.Fatalf("reservation = %+v, want an applied claim", reservation)
	}
	if reservation.ExpiresAtUnix <= reservation.CreatedAtUnix {
		t.Fatalf("reservation has no expiry, so the ledger is unbounded: %+v", reservation)
	}

	// A different request id is a different request and does apply.
	second, err := service.ApplyProgress(ctx, key, "player-1", "req-2", ProgressDelta{Score: 2})
	if err != nil {
		t.Fatal(err)
	}
	if second.Score != 42 || second.Applies != 2 {
		t.Fatalf("a distinct request did not apply: %+v", second)
	}
}

// The reservation and the participant record are two keys and cannot be
// written atomically, so "reserved" alone can never tell whether the apply
// landed. This arranges exactly that gap — the mark that says "applied" never
// lands — and asserts a replay still cannot double count, because the request
// id was recorded in the same compare-and-set that moved the score.
//
// Deterministic on purpose: the concurrent test below hits this window only if
// the scheduler cooperates, and an invariant that is only tested when the
// timing is unlucky is not tested.
func TestAnUnmarkedReservationCannotDoubleApply(t *testing.T) {
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Ledger = unmarkableLedger{Store: cfg.Ledger}
	})
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	first, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 5})
	if err != nil {
		t.Fatal(err)
	}
	if first.Score != 5 || first.Applies != 1 {
		t.Fatalf("first apply = %+v", first)
	}
	// The arrangement: the claim is still merely reserved, so a replay cannot
	// be answered by the ledger and must be answered by the apply itself.
	reservation, found, err := service.Reservation(ctx, key, "player-1", "req-1")
	if err != nil || !found {
		t.Fatalf("reservation: found=%v err=%v", found, err)
	}
	if reservation.State != ReservationReserved {
		t.Fatalf("arrangement failed: reservation is %q", reservation.State)
	}

	for attempt := 0; attempt < 3; attempt++ {
		replayed, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 5})
		if err != nil {
			t.Fatalf("replay %d: %v", attempt, err)
		}
		if replayed.Score != 5 || replayed.Applies != 1 {
			t.Fatalf("replay %d double-counted through an unmarked reservation: %+v", attempt, replayed)
		}
	}
	current, _, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Score != 5 || current.Applies != 1 {
		t.Fatalf("stored participant = %+v, want score 5 applied once", current)
	}
}

// A progress apply must claim its ledger reservation BEFORE it touches the
// participant record — "participant progress 必须先通过 request ledger CAS
// reserve 再更新参与者积分/进度". This asserts the order, so removing the
// reserve step (or moving it after the apply) fails here rather than only
// under a race.
func TestProgressReservesBeforeItApplies(t *testing.T) {
	log := &storeCallLog{}
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Ledger = tracedStore[RequestKey, ProgressReservation]{
			inner: cfg.Ledger, name: "ledger", log: log,
		}
		cfg.Participants = tracedStore[ParticipantKey, Participant]{
			inner: cfg.Participants, name: "participant", log: log,
		}
	})
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	log.reset()
	if _, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatal(err)
	}
	calls := log.snapshot()
	reserve, apply := -1, -1
	for index, call := range calls {
		if call == "ledger.Create" && reserve < 0 {
			reserve = index
		}
		if call == "participant.Update" && apply < 0 {
			apply = index
		}
	}
	if reserve < 0 {
		t.Fatalf("no insert-only ledger reservation was taken: %v", calls)
	}
	if apply < 0 {
		t.Fatalf("the participant record was never written: %v", calls)
	}
	if reserve > apply {
		t.Fatalf("the apply ran before the reservation was claimed: %v", calls)
	}
	// And the participant is only ever written through Update: a versionstore
	// has no unconditional write, which is what makes the CAS invariant a
	// property of the type rather than of whichever store was configured.
	for _, call := range calls {
		if strings.HasPrefix(call, "participant.") && call != "participant.Update" && call != "participant.Get" {
			t.Fatalf("participant progress used %q", call)
		}
	}
}

func TestApplyProgressValidatesItsRequest(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	for _, testCase := range []struct {
		label         string
		key           Key
		participantID string
		requestID     string
		delta         ProgressDelta
		want          error
	}{
		{"empty participant", key, "  ", "req", ProgressDelta{Score: 1}, ErrParticipantInvalid},
		{"oversized participant", key, strings.Repeat("p", MaxParticipantIDLen+1), "req", ProgressDelta{Score: 1}, ErrParticipantInvalid},
		{"empty request id", key, "player-1", "", ProgressDelta{Score: 1}, ErrRequestInvalid},
		{"oversized request id", key, "player-1", strings.Repeat("r", MaxRequestIDLen+1), ProgressDelta{Score: 1}, ErrRequestInvalid},
		{"request id with separator", key, "player-1", "a/b", ProgressDelta{Score: 1}, ErrRequestInvalid},
		{"negative delta", key, "player-1", "req", ProgressDelta{Score: -1}, ErrRequestInvalid},
		{"empty delta", key, "player-1", "req", ProgressDelta{}, ErrRequestInvalid},
		{"unknown activity", activityKey("nope"), "player-1", "req", ProgressDelta{Score: 1}, ErrMissing},
		{"invalid activity key", Key{}, "player-1", "req", ProgressDelta{Score: 1}, ErrInvalid},
	} {
		_, err := service.ApplyProgress(ctx, testCase.key, testCase.participantID, testCase.requestID, testCase.delta)
		if !errors.Is(err, testCase.want) {
			t.Fatalf("%s: got %v, want %v", testCase.label, err, testCase.want)
		}
		if code := Code(err); code == errcode.CodeInternal {
			t.Fatalf("%s: a client mistake reported the internal code", testCase.label)
		}
	}

	// One request id, two different deltas: neither answer is right, and
	// applying both is the double count the ledger exists to prevent.
	if _, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 5}); err != nil {
		t.Fatal(err)
	}
	_, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 6})
	if !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("a reused request id with a different delta returned %v", err)
	}
	current, _, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Score != 5 {
		t.Fatalf("the refused reuse changed the score: %+v", current)
	}
}

// Progress after the aggregation completed is refused: its result has already
// been dispatched, so an apply now would be a score only this service knows
// about.
func TestProgressAfterCompletionIsRefused(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1)
	notify(t, service, key, 1)

	_, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 1})
	if !errors.Is(err, ErrStatus) {
		t.Fatalf("an apply against a complete aggregation returned %v, want ErrStatus", err)
	}
	if _, found, err := service.LookupParticipant(ctx, key, "player-1"); err != nil || found {
		t.Fatalf("the refused apply created a participant: found=%v err=%v", found, err)
	}
}

// --- dispatch ---

func TestDispatchIsRetriedAckedAndFinallyExhausted(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	notify(t, service, key, 2)

	due, err := service.DueDispatches(ctx, key, MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("%d dispatches are due, want one per expected game", len(due))
	}
	if bounded, err := service.DueDispatches(ctx, key, 1); err != nil || len(bounded) != 1 {
		t.Fatalf("bounded listing returned %d (err=%v)", len(bounded), err)
	}

	// Game 1 processes its delivery and acks with the token it was handed.
	first, err := service.AttemptDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Attempts != 1 || first.Token == "" {
		t.Fatalf("attempt = %+v", first)
	}
	// A second attempt before the backoff elapsed is refused, so two runners
	// cannot turn one dispatch into a hot loop.
	if _, err := service.AttemptDispatch(ctx, key, 1); !errors.Is(err, ErrDispatchNotDue) {
		t.Fatalf("an immediate second attempt returned %v, want ErrDispatchNotDue", err)
	}
	acked, err := service.AckDispatch(ctx, key, 1, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if acked.State != DispatchAcked || acked.AckedAtUnix == 0 {
		t.Fatalf("ack = %+v", acked)
	}
	// A redelivered ack is idempotent and does not move the ack time.
	again, err := service.AckDispatch(ctx, key, 1, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if again.AckedAtUnix != acked.AckedAtUnix {
		t.Fatalf("a redelivered ack moved the ack time: %d -> %d", acked.AckedAtUnix, again.AckedAtUnix)
	}
	// An acked dispatch is no longer due, so the runner stops on its own.
	if due, err := service.DueDispatches(ctx, key, MaxPageSize); err != nil || len(due) != 1 {
		t.Fatalf("%d dispatches still due after one ack (err=%v)", len(due), err)
	}

	// Game 2 never acks. Its budget runs out and the dispatch reaches an
	// explicit terminal state instead of quietly ceasing to be picked up.
	for attempt := 1; attempt <= 3; attempt++ {
		got, err := service.AttemptDispatch(ctx, key, 2)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if got.Attempts != attempt {
			t.Fatalf("attempt %d recorded %d attempts", attempt, got.Attempts)
		}
		c.advance(time.Duration(attempt) * 5 * time.Second)
	}
	exhausted, err := service.AttemptDispatch(ctx, key, 2)
	if !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("the fourth attempt on a 3-attempt budget returned %v", err)
	}
	if exhausted.State != DispatchExhausted || exhausted.ExhaustedAtUnix == 0 {
		t.Fatalf("exhaustion was not recorded: %+v", exhausted)
	}
	stored, found, err := service.LookupDispatch(ctx, key, 2)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if stored.State != DispatchExhausted {
		t.Fatalf("the terminal state was not persisted: %+v", stored)
	}
	if _, err := service.AttemptDispatch(ctx, key, 2); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("an exhausted dispatch handed out another attempt: %v", err)
	}
	// A very late ack is refused rather than quietly erasing the evidence
	// that delivery to game 2 never worked.
	if _, err := service.AckDispatch(ctx, key, 2, stored.Token); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("an ack after exhaustion returned %v", err)
	}
}

// The ack token is the whole authorization, and a wrong guess must not teach
// the caller the right answer.
func TestDispatchAckRequiresItsToken(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	notify(t, service, key, 2)

	one, found, err := service.LookupDispatch(ctx, key, 1)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	two, found, err := service.LookupDispatch(ctx, key, 2)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}

	for _, testCase := range []struct {
		label string
		token string
	}{
		{"empty token", ""},
		{"another game's token", two.Token},
		{"guessed token", "00000000000000000000000000000000"},
	} {
		_, err := service.AckDispatch(ctx, key, 1, testCase.token)
		if !errors.Is(err, ErrDispatchToken) {
			t.Fatalf("%s: ack returned %v, want ErrDispatchToken", testCase.label, err)
		}
		if strings.Contains(err.Error(), one.Token) {
			t.Fatalf("%s: the refusal leaked the expected token: %v", testCase.label, err)
		}
	}
	current, _, err := service.LookupDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != DispatchPending {
		t.Fatalf("a refused ack changed the dispatch: %+v", current)
	}
	if _, err := service.AckDispatch(ctx, activityKey("nope"), 1, "x"); !errors.Is(err, ErrDispatchMissing) {
		t.Fatalf("acking an unknown dispatch returned %v", err)
	}
	if _, err := service.AttemptDispatch(ctx, activityKey("nope"), 1); !errors.Is(err, ErrDispatchMissing) {
		t.Fatalf("attempting an unknown dispatch returned %v", err)
	}
}

// --- concurrency ---

// Concurrent notifications from different games: the collecting snapshot must
// lose nothing, and exactly one caller must see the completion.
func TestConcurrentNotificationsLoseNothingAndCompleteOnce(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	const games = 8
	expected := make([]int32, games)
	for i := range expected {
		expected[i] = int32(i + 1)
	}
	openActivity(t, service, key, expected...)

	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		completes int
	)
	for game := int32(1); game <= games; game++ {
		wait.Add(1)
		go func(gameSID int32) {
			defer wait.Done()
			activity, err := service.NotifyPhase(ctx, key, gameSID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("game %d: %v", gameSID, err)
				return
			}
			if activity.Status == StatusComplete {
				completes++
			}
		}(game)
	}
	wait.Wait()

	if completes != 1 {
		t.Fatalf("%d callers saw the completion, want exactly 1", completes)
	}
	current, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if len(current.NotifiedGameSIDs) != games {
		t.Fatalf("the collecting snapshot holds %d of %d notifications: %+v",
			len(current.NotifiedGameSIDs), games, current.NotifiedGameSIDs)
	}
	seen := map[int32]bool{}
	for _, gameSID := range current.NotifiedGameSIDs {
		if seen[gameSID] {
			t.Fatalf("game %d was counted twice", gameSID)
		}
		seen[gameSID] = true
	}
	if current.Status != StatusComplete || current.CompletionReason != CompletedCollected {
		t.Fatalf("activity = %+v", current)
	}
	for _, gameSID := range expected {
		if _, found, err := service.LookupDispatch(ctx, key, gameSID); err != nil || !found {
			t.Fatalf("dispatch for game %d: found=%v err=%v", gameSID, found, err)
		}
	}
}

// Concurrent progress applies for one participant: distinct request ids must
// all land (nothing lost to a lost compare-and-set), and concurrent
// redeliveries of ONE request id must apply exactly once.
func TestConcurrentProgressAppliesLoseNothingAndReplayOnce(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)

	const writers, perWriter = 8, 5
	var wait sync.WaitGroup
	errs := make([]error, writers)
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for i := 0; i < perWriter; i++ {
				requestID := fmt.Sprintf("req-%d-%d", index, i)
				if _, err := service.ApplyProgress(ctx, key, "player-1", requestID, ProgressDelta{Score: 1, Progress: 1}); err != nil {
					errs[index] = err
					return
				}
			}
		}(writer)
	}
	wait.Wait()
	for writer, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", writer, err)
		}
	}
	current, found, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if current.Score != writers*perWriter {
		t.Fatalf("score = %d, want %d: a concurrent apply was lost", current.Score, writers*perWriter)
	}
	if current.Applies != uint64(writers*perWriter) {
		t.Fatalf("applies = %d, want %d", current.Applies, writers*perWriter)
	}

	// Now the same request id from many callers at once — the redelivery
	// storm the confirmed defect double-counted.
	before := current.Score
	var replayWait sync.WaitGroup
	replayErrs := make([]error, 12)
	for racer := 0; racer < 12; racer++ {
		replayWait.Add(1)
		go func(index int) {
			defer replayWait.Done()
			_, err := service.ApplyProgress(ctx, key, "player-1", "shared-req", ProgressDelta{Score: 100})
			replayErrs[index] = err
		}(racer)
	}
	replayWait.Wait()
	for racer, err := range replayErrs {
		if err != nil {
			t.Fatalf("racer %d: %v", racer, err)
		}
	}
	after, _, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Score != before+100 {
		t.Fatalf("score = %d, want %d: one request id applied more than once", after.Score, before+100)
	}
	if after.Applies != current.Applies+1 {
		t.Fatalf("applies = %d, want %d", after.Applies, current.Applies+1)
	}
}

// Concurrent back-stop runners must produce exactly one completion per
// activity: the sweep's advance is a compare-and-set, not a read-then-write,
// so no lock is needed and none would help across instances anyway.
func TestConcurrentAdvanceExpiredHasOneWinner(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	c.advance(31 * time.Second)

	const runners = 10
	var (
		wait  sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for runner := 0; runner < runners; runner++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			completed, err := service.AdvanceExpired(ctx, "group-a", 10)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("sweep: %v", err)
				return
			}
			total += len(completed)
		}()
	}
	wait.Wait()
	if total != 1 {
		t.Fatalf("%d runners completed the activity, want exactly 1", total)
	}
	current, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if current.Status != StatusComplete || current.CompletionReason != CompletedGraceExpired {
		t.Fatalf("activity = %+v", current)
	}
}

// A notify racing the back-stop must not produce two completions with
// different reasons, and must not lose the aggregation either way.
func TestConcurrentNotifyAndSweepAgreeOnOneCompletion(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	c.advance(31 * time.Second)

	var wait sync.WaitGroup
	wait.Add(2)
	var (
		mu           sync.Mutex
		notifyErr    error
		sweptCount   int
		notifiedSeen bool
	)
	go func() {
		defer wait.Done()
		_, err := service.NotifyPhase(ctx, key, 2)
		mu.Lock()
		defer mu.Unlock()
		notifyErr = err
		notifiedSeen = true
	}()
	go func() {
		defer wait.Done()
		completed, err := service.AdvanceExpired(ctx, "group-a", 10)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
		sweptCount = len(completed)
	}()
	wait.Wait()

	if !notifiedSeen {
		t.Fatal("the notify goroutine did not run")
	}
	// The notify is past the deadline, so it is refused either way — as late
	// (the sweep had not run) or as stale (it had). Both are audited.
	if !errors.Is(notifyErr, ErrNotifyLate) && !errors.Is(notifyErr, ErrStatus) {
		t.Fatalf("the late notify returned %v", notifyErr)
	}
	audits, err := service.NotifyAudits(ctx, key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("%d audits for one refused notify: %+v", len(audits), audits)
	}
	if sweptCount != 1 {
		t.Fatalf("the sweep completed %d activities, want 1", sweptCount)
	}
	current, _, err := service.LookupActivity(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusComplete {
		t.Fatalf("activity = %+v", current)
	}
	if len(current.NotifiedGameSIDs) != 1 {
		t.Fatalf("a refused late notify entered the snapshot: %+v", current.NotifiedGameSIDs)
	}
}

// Concurrent acks for one dispatch: exactly one write, and every caller sees
// the same ack time.
func TestConcurrentAcksAreIdempotent(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1)
	notify(t, service, key, 1)
	dispatch, found, err := service.LookupDispatch(ctx, key, 1)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}

	const racers = 10
	var wait sync.WaitGroup
	acked := make([]Dispatch, racers)
	for racer := 0; racer < racers; racer++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := service.AckDispatch(ctx, key, 1, dispatch.Token)
			if err != nil {
				t.Errorf("racer %d: %v", index, err)
				return
			}
			acked[index] = result
		}(racer)
	}
	wait.Wait()
	for racer, result := range acked {
		if result.State != DispatchAcked {
			t.Fatalf("racer %d saw state %q", racer, result.State)
		}
		if result.AckedAtUnix != acked[0].AckedAtUnix {
			t.Fatalf("racer %d saw ack time %d, racer 0 saw %d", racer, result.AckedAtUnix, acked[0].AckedAtUnix)
		}
	}
}

// --- aliasing ---

// The memory store keeps values as given rather than round-tripping them
// through a codec, so a returned slice that aliased stored state would let a
// caller mutate the store behind every compare-and-set. The Redis
// implementation would not show it, which is why this is asserted here.
func TestReturnedValuesDoNotAliasStoredState(t *testing.T) {
	service, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)

	activity, found, err := service.LookupActivity(ctx, key)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	activity.ExpectedGameSIDs[0] = 999
	activity.NotifiedGameSIDs[0] = 999

	if _, err := service.ApplyProgress(ctx, key, "player-1", "req-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatal(err)
	}
	participant, _, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil {
		t.Fatal(err)
	}
	participant.AppliedRequestIDs[0] = "tampered"

	fresh, _, err := service.LookupActivity(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Expects(1) || !fresh.Notified(1) {
		t.Fatalf("a caller's mutation reached stored state: %+v", fresh)
	}
	freshParticipant, _, err := service.LookupParticipant(ctx, key, "player-1")
	if err != nil {
		t.Fatal(err)
	}
	if !freshParticipant.Applied("req-1") {
		t.Fatalf("a caller's mutation reached the applied ring: %+v", freshParticipant)
	}
}

// --- error codes ---

// Every business failure has a code of its own. A client mistake that reaches
// the caller as the store-failure code is indistinguishable from an
// unreachable backend, and the client then retries forever a request that can
// never succeed.
func TestEveryClientMistakeHasItsOwnCode(t *testing.T) {
	if Code(nil) != CodeOK {
		t.Fatal("a nil error is not CodeOK")
	}
	codes := map[int32]string{}
	for _, testCase := range []struct {
		label string
		err   error
		want  int32
	}{
		{"invalid", ErrInvalid, CodeInvalid},
		{"missing", ErrMissing, CodeMissing},
		{"exists", ErrExists, CodeExists},
		{"status", ErrStatus, CodeStatus},
		{"unexpected", ErrNotifyUnexpected, CodeNotifyUnexpected},
		{"late", ErrNotifyLate, CodeNotifyLate},
		{"backlog", ErrBacklog, CodeBacklog},
		{"participant", ErrParticipantInvalid, CodeParticipantInvalid},
		{"request", ErrRequestInvalid, CodeRequestInvalid},
		{"dispatch missing", ErrDispatchMissing, CodeDispatchMissing},
		{"dispatch token", ErrDispatchToken, CodeDispatchToken},
		{"dispatch exhausted", ErrDispatchExhausted, CodeDispatchExhausted},
		{"dispatch not due", ErrDispatchNotDue, CodeDispatchNotDue},
		{"range", ErrRangeInvalid, CodeRangeInvalid},
		{"conflict", ErrConflict, CodeConflict},
		{"not resolvable", ErrNotResolvable, CodeNotResolvable},
		{"admin note", ErrAdminNoteRequired, CodeAdminNoteRequired},
	} {
		if got := Code(fmt.Errorf("wrapped: %w", testCase.err)); got != testCase.want {
			t.Fatalf("%s: code = %d, want %d", testCase.label, got, testCase.want)
		}
		if other, clash := codes[testCase.want]; clash {
			t.Fatalf("%s and %s share code %d", testCase.label, other, testCase.want)
		}
		codes[testCase.want] = testCase.label
		if testCase.want == errcode.CodeInternal || testCase.want == CodeOK {
			t.Fatalf("%s maps to a non-business code", testCase.label)
		}
	}
	// This package's own segment, contiguous from its first code and with
	// exactly as many entries as are declared.
	//
	// Both checks are needed and neither is redundant: contiguity catches a
	// hole in the middle, and the count catches a truncation at the end, which
	// contiguity cannot see because a shorter run is still contiguous. A
	// table-driven test cannot notice its own table shrinking.
	//
	// The segment is 6201xx rather than the 5701xx range these codes had while
	// the service lived inside package global. Sharing one range between two
	// services is what left global's segment with a permanent hole, and it
	// made "which package owns this number" a question with two answers.
	const (
		segmentFirst     = 620101
		segmentAllocated = 17
	)
	if len(codes) != segmentAllocated {
		t.Fatalf("%d codes are paired, want %d; a code was added or removed without updating "+
			"the count", len(codes), segmentAllocated)
	}
	ordered := make([]int, 0, len(codes))
	for code := range codes {
		ordered = append(ordered, int(code))
	}
	sort.Ints(ordered)
	for index, code := range ordered {
		if want := segmentFirst + index; code != want {
			t.Fatalf("the segment has a hole: expected %d at position %d, found %d (%s)",
				want, index, code, codes[int32(code)])
		}
	}
	if Code(errors.New("backend is down")) != errcode.CodeInternal {
		t.Fatal("an unrecognised error is not reported as a store failure")
	}
}

// Walks the real API's refusal paths (not just the sentinels) and asserts none
// of them reaches a caller as a store failure.
func TestNoAPIPathReturnsABareError(t *testing.T) {
	service, c := newActivityService(t)
	ctx := context.Background()
	key := activityKey("act-1")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)

	for _, testCase := range []struct {
		label string
		call  func() error
	}{
		{"reopen", func() error { _, err := service.OpenActivity(ctx, key, []int32{1, 2}); return err }},
		{"notify unexpected game", func() error { _, err := service.NotifyPhase(ctx, key, 77); return err }},
		{"notify unknown activity", func() error { _, err := service.NotifyPhase(ctx, activityKey("nope"), 1); return err }},
		{"notify zero game", func() error { _, err := service.NotifyPhase(ctx, key, 0); return err }},
		{"apply to unknown activity", func() error {
			_, err := service.ApplyProgress(ctx, activityKey("nope"), "p", "r", ProgressDelta{Score: 1})
			return err
		}},
		{"apply empty request", func() error {
			_, err := service.ApplyProgress(ctx, key, "p", "", ProgressDelta{Score: 1})
			return err
		}},
		{"sweep with zero limit", func() error { _, err := service.AdvanceExpired(ctx, "group-a", 0); return err }},
		{"sweep with no group", func() error { _, err := service.AdvanceExpired(ctx, "", 10); return err }},
		{"audits with zero limit", func() error { _, err := service.NotifyAudits(ctx, key, 0); return err }},
		{"dispatch missing", func() error { _, err := service.AttemptDispatch(ctx, key, 1); return err }},
		{"ack missing", func() error { _, err := service.AckDispatch(ctx, key, 1, "token"); return err }},
		{"ack zero game", func() error { _, err := service.AckDispatch(ctx, key, 0, "token"); return err }},
		{"lookup invalid key", func() error { _, _, err := service.LookupActivity(ctx, Key{}); return err }},
		{"due dispatches unknown activity", func() error {
			_, err := service.DueDispatches(ctx, activityKey("nope"), 10)
			return err
		}},
	} {
		err := testCase.call()
		if err == nil {
			t.Fatalf("%s: expected a refusal", testCase.label)
		}
		if code := Code(err); code == errcode.CodeInternal {
			t.Fatalf("%s: %v reached the caller as CodeInternal", testCase.label, err)
		}
	}
	_ = c
}

// --- test doubles ---

// unmarkableLedger is a ledger whose Update never lands: it stands in for a
// process that died between applying the delta and marking its reservation
// applied, which is the one window where the participant's own applied ring is
// the only thing standing between a replay and a double count.
type unmarkableLedger struct {
	versionstore.Store[RequestKey, ProgressReservation]
}

func (unmarkableLedger) Update(context.Context, RequestKey, versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	return versionstore.Versioned[ProgressReservation]{}, false, nil
}

// storeCallLog records the order of store calls, so a test can assert that a
// progress apply reserved before it applied. It records nothing about values:
// the ordering is the invariant.
type storeCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *storeCallLog) record(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *storeCallLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

func (l *storeCallLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.calls))
	copy(out, l.calls)
	return out
}

// tracedStore is a pass-through versionstore.Store that records which methods
// were called. It deliberately implements the same four methods and nothing
// else: there is no Set to record, because the contract has none.
type tracedStore[K comparable, T any] struct {
	inner versionstore.Store[K, T]
	name  string
	log   *storeCallLog
}

func (s tracedStore[K, T]) Get(ctx context.Context, key K) (versionstore.Versioned[T], bool, error) {
	s.log.record(s.name + ".Get")
	return s.inner.Get(ctx, key)
}

func (s tracedStore[K, T]) Update(ctx context.Context, key K, mutate versionstore.Mutate[T]) (versionstore.Versioned[T], bool, error) {
	s.log.record(s.name + ".Update")
	return s.inner.Update(ctx, key, mutate)
}

func (s tracedStore[K, T]) Create(ctx context.Context, key K, value T) (versionstore.Versioned[T], bool, error) {
	s.log.record(s.name + ".Create")
	return s.inner.Create(ctx, key, value)
}

func (s tracedStore[K, T]) Delete(ctx context.Context, key K, expect versionstore.Versioned[T]) error {
	s.log.record(s.name + ".Delete")
	return s.inner.Delete(ctx, key, expect)
}
