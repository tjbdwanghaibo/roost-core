package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	service   *Service
	clock     *clock
	envelopes *fakeEnvelopes
	mailboxes MailboxStore
	metrics   *servicemetrics.Recorder
}

// tokens hands out predictable claim tokens so a test can say which token it
// expects, and so "the same token came back" is an assertion rather than an
// observation about two random strings.
func tokens(prefix string) func() (string, error) {
	var mu sync.Mutex
	next := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("%s-%d", prefix, next), nil
	}
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	h := &harness{
		clock:     c,
		envelopes: newFakeEnvelopes(),
		mailboxes: versionstore.NewMemoryStore[int64, Mailbox](),
		metrics:   servicemetrics.NewRecorder(),
	}
	ids := tokens("mail")
	cfg := Config{
		Envelopes:     h.envelopes,
		Mailboxes:     h.mailboxes,
		Sends:         versionstore.NewMemoryStore[string, SentRecord](),
		ClaimLease:    30 * time.Second,
		NewMailID:     ids,
		NewClaimToken: tokens("token"),
		Now:           c.Now,
		Metrics:       h.metrics,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	h.service = service
	return h
}

func directTo(recipients ...int64) SendRequest {
	return SendRequest{
		Audience: AudienceDirect, Recipients: recipients,
		Subject: "reward", Body: "well played",
		ExpiresInSeconds: 604800, RequestID: "send-1",
	}
}

func withAttachment(req SendRequest, payload string) SendRequest {
	req.Attachment = []byte(payload)
	return req
}

func mustSend(t *testing.T, h *harness, req SendRequest) Envelope {
	t.Helper()
	envelope, err := h.service.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("send %q: %v", req.RequestID, err)
	}
	return envelope
}

// --- the claim token is the fix: it is constant per (player, mail) ---

// The confirmed defect: the claim idempotency key came from the client, and a
// lapsed reservation could be taken over under a DIFFERENT key. So a caller
// whose commit response was lost could wait out the lease, retry with a fresh
// id, and be handed the attachment again under a key the delivery side had
// never seen — a duplicate grant.
//
// Here the token is minted by the service and is constant for the life of the
// entry, so every retry presents the same key.
func TestAReReservationReturnsTheSameTokenAfterTheLeaseLapses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))

	first, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == "" {
		t.Fatal("the reservation returned an empty token")
	}
	if first.Attempts != 1 {
		t.Fatalf("the first reservation reported attempt %d, want 1", first.Attempts)
	}

	// The deliverer crashed: no commit, no cancel. Wait out the lease.
	h.clock.advance(time.Minute)

	second, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatalf("a lapsed reservation could not be retaken: %v", err)
	}
	if second.Token != first.Token {
		t.Fatalf("the retry got token %q, the first reservation had %q; a retry that "+
			"changes the delivery key is the duplicate-grant defect this design removes",
			second.Token, first.Token)
	}
	if second.Attempts != 2 {
		t.Fatalf("the second reservation reported attempt %d, want 2", second.Attempts)
	}
	// The attempt count is NOT part of the key: a delivery side deduping on
	// the token must see one key across both attempts.
	if second.Attempts == first.Attempts {
		t.Fatal("the attempt counter did not move, so a repeatedly failing delivery is invisible")
	}
}

// A reservation while another is in flight is refused, and refused
// distinguishably from "already claimed": "someone is delivering this right
// now" and "this was delivered" call for different client behaviour.
func TestAnInFlightReservationBlocksAnotherOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))

	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("a second reservation inside the lease returned %v, want ErrClaimHeld", err)
	}
	if errors.Is(err, ErrAlreadyClaimed) {
		t.Fatal("an in-flight claim was reported as already claimed, which tells the client to give up")
	}
}

// Committing with the wrong token is refused. Without this check the token
// would be decoration, which is exactly what the replaced implementation's
// version field was.
func TestACommitWithTheWrongTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, "someone-elses-token"); !errors.Is(err, ErrClaimTokenWrong) {
		t.Fatalf("a commit with a foreign token returned %v, want ErrClaimTokenWrong", err)
	}
	mailbox, _, err := h.service.Mailbox(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.Entries[envelope.ID].Status == StatusClaimed {
		t.Fatal("a commit with a foreign token still marked the mail claimed")
	}
}

// A retried commit succeeds and does not move anything, so a client retry
// after a lost response is not an error.
func TestARetriedCommitIsANoOp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		again, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token)
		if err != nil {
			t.Fatalf("retry %d failed: %v", attempt, err)
		}
		if again.UpdatedAtUnix != first.UpdatedAtUnix {
			t.Fatalf("retry %d moved the claim time from %d to %d",
				attempt, first.UpdatedAtUnix, again.UpdatedAtUnix)
		}
	}
	if got := h.metrics.Count("replayed:commit_claim"); got != 4 {
		t.Fatalf("four retried commits reported %d replays; %s", got, h.metrics.Events())
	}
}

// The token survives the commit, because it stays this mail's delivery key
// forever: a duplicate delivery attempt arriving late must still dedupe.
func TestTheClaimTokenSurvivesTheCommit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := h.service.Mailbox(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	entry := mailbox.Entries[envelope.ID]
	if entry.ClaimToken != claim.Token {
		t.Fatalf("the committed entry holds token %q, the claim had %q", entry.ClaimToken, claim.Token)
	}
	if entry.ClaimDeadlineUnix != 0 {
		t.Fatalf("a committed claim still holds a deadline (%d)", entry.ClaimDeadlineUnix)
	}
}

// Only one of many concurrent reservations may win, or two deliverers hand
// out the same attachment at once.
func TestOnlyOneConcurrentReservationWins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))

	const racers = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  []Claim
		refused int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			claims = append(claims, claim)
		}()
	}
	wg.Wait()

	if len(claims) != 1 {
		t.Fatalf("%d of %d concurrent reservations succeeded, want 1", len(claims), racers)
	}
	if refused != racers-1 {
		t.Fatalf("%d reservations were refused, want %d", refused, racers-1)
	}
}

// A deleted mail cannot be claimed. A player who could delete-then-claim
// would claim twice.
func TestADeletedMailCannotBeClaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	if _, err := h.service.Delete(ctx, 1, envelope.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrMailMissing) {
		t.Fatalf("a deleted mail was claimable: %v", err)
	}
}

// A cancel keeps the token and only releases the deadline, so a retry after a
// cancel still presents the same delivery key.
func TestACancelReleasesTheDeadlineButKeepsTheToken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	first, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	released, err := h.service.CancelClaim(ctx, 1, envelope.ID, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("the cancel reported that it released nothing")
	}
	// No clock advance: the point is that the cancel, not the lease, freed it.
	second, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatalf("a cancelled reservation could not be retaken: %v", err)
	}
	if second.Token != first.Token {
		t.Fatalf("a retry after a cancel got token %q, want %q", second.Token, first.Token)
	}
}

// A cancel that finds nothing to release says so. The implementation this
// replaces had two return branches with identical values, so a caller could
// not tell a released reservation from a no-op and neither was counted.
func TestACancelThatReleasesNothingReportsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	released, err := h.service.CancelClaim(ctx, 1, envelope.ID, "not-the-token")
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("a cancel with a foreign token released the reservation")
	}
	if got := h.metrics.Count("dropped:cancel_claim.nothing_to_release"); got != 1 {
		t.Fatalf("a cancel that did nothing reported %d drops; %s", got, h.metrics.Events())
	}
	// And the real reservation is untouched.
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("the foreign cancel released the real reservation: %v", err)
	}
	// The real token still works, so the foreign cancel changed nothing at all.
	if released, err := h.service.CancelClaim(ctx, 1, envelope.ID, claim.Token); err != nil || !released {
		t.Fatalf("the real cancel released=%v err=%v", released, err)
	}
}

// --- authority comes from the caller, never from a request field ---

// The confirmed defect: PlayerID came from the request body, so any caller
// could claim another player's attachments.
func TestAPlayerCannotClaimAMailAddressedToSomeoneElse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))

	if _, err := h.service.ReserveClaim(ctx, 2, envelope.ID, ""); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("player 2 reserved a mail addressed to player 1: %v", err)
	}
	if got := h.metrics.Count("refused:reserve_claim:not_recipient"); got != 1 {
		t.Fatalf("a foreign claim reported %d refusals; %s", got, h.metrics.Events())
	}
}

// A broadcast mail is addressed by scope, and a caller in another scope is
// not a recipient.
func TestABroadcastAddressesOnlyItsScope(t *testing.T) {
	h := newHarness(t, func(cfg *Config) {
		cfg.Broadcast = DelivererFunc(func(context.Context, Envelope) error { return nil })
	})
	ctx := context.Background()
	req := SendRequest{
		Audience: AudienceBroadcast, Scope: "server-1",
		Subject: "maintenance", ExpiresInSeconds: 3600, RequestID: "send-b",
	}
	envelope, err := h.service.Send(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Addresses(7, "server-1") {
		t.Fatal("a broadcast to server-1 does not address a player on server-1")
	}
	if envelope.Addresses(7, "server-2") {
		t.Fatal("a broadcast to server-1 addresses a player on server-2")
	}

	// And the scope check is what refuses a claim from another scope, so the
	// authority decision is not left to whatever fanout the caller wired.
	if err := h.service.Deliver(ctx, 7, envelope.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 7, envelope.ID, "server-2"); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("a player on server-2 reserved a server-1 broadcast: %v", err)
	}
}

// A broadcast with no deliverer is refused at send time rather than accepted
// and delivered to nobody.
func TestABroadcastWithoutADelivererIsRefused(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Send(context.Background(), SendRequest{
		Audience: AudienceBroadcast, Subject: "hello",
		ExpiresInSeconds: 3600, RequestID: "send-b",
	})
	if !errors.Is(err, ErrAudienceInvalid) {
		t.Fatalf("a broadcast with no deliverer returned %v, want ErrAudienceInvalid", err)
	}
	if got := h.metrics.Count("refused:send:no_deliverer"); got != 1 {
		t.Fatalf("the refusal reported %d times; %s", got, h.metrics.Events())
	}
}

// A broadcast carrying a recipient list is two addressing schemes at once.
func TestABroadcastMayNotCarryARecipientList(t *testing.T) {
	h := newHarness(t, func(cfg *Config) {
		cfg.Broadcast = DelivererFunc(func(context.Context, Envelope) error { return nil })
	})
	_, err := h.service.Send(context.Background(), SendRequest{
		Audience: AudienceBroadcast, Recipients: []int64{1},
		Subject: "hello", ExpiresInSeconds: 3600, RequestID: "send-b",
	})
	if !errors.Is(err, ErrAudienceInvalid) {
		t.Fatalf("a broadcast with recipients returned %v, want ErrAudienceInvalid", err)
	}
}

// --- the work is bounded, not just the output ---

// The confirmed defect: List clamped how many items it RETURNED, then looped
// fetching pages and issuing one state read per envelope until the page
// filled, with no bound on the iterations. A player with many deleted mails
// turned one protocol packet into an unbounded number of database calls.
//
// This asserts the round-trip count directly, because asserting on the number
// of returned items is exactly the assertion that passed while the defect was
// present.
func TestListIsTwoReadsRegardlessOfMailboxSize(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A mailbox with far more mail than one page, most of it deleted — the
	// shape that made the old loop iterate.
	for i := 0; i < 60; i++ {
		envelope := mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: fmt.Sprintf("mail %d", i), ExpiresInSeconds: 3600,
			RequestID: fmt.Sprintf("send-%d", i),
		})
		if i%2 == 0 {
			if _, err := h.service.Delete(ctx, 1, envelope.ID); err != nil {
				t.Fatal(err)
			}
		}
		h.clock.advance(time.Second)
	}

	h.envelopes.resetCounts()
	page, err := h.service.List(ctx, 1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 10 {
		t.Fatalf("the page holds %d items, want 10", len(page.Items))
	}
	gets, getManys := h.envelopes.roundTrips()
	if getManys != 1 {
		t.Fatalf("listing one page made %d batched envelope reads, want 1", getManys)
	}
	if gets != 0 {
		t.Fatalf("listing one page made %d single-envelope reads, want 0; "+
			"one read per envelope is the unbounded-round-trip defect", gets)
	}
}

// A zero limit means the default, never "no limit". That translation is what
// turned one rank protocol packet into a full-board read.
func TestAZeroLimitDoesNotMeanUnbounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < DefaultPageSize+15; i++ {
		mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		h.clock.advance(time.Second)
	}
	page, err := h.service.List(ctx, 1, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != DefaultPageSize {
		t.Fatalf("a zero limit returned %d items, want the default %d", len(page.Items), DefaultPageSize)
	}
}

// A limit above the cap is clamped, not honoured.
func TestAnOversizedLimitIsClamped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < MaxPageSize+10; i++ {
		mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		h.clock.advance(time.Second)
	}
	page, err := h.service.List(ctx, 1, "", MaxPageSize*10)
	if err != nil {
		t.Fatal(err)
	}
	// Clamped means exactly the cap: the mailbox holds more than MaxPageSize,
	// so anything less is a page that came back short, and "not above" is
	// the assertion that lets a short page through.
	if len(page.Items) != MaxPageSize {
		t.Fatalf("the page holds %d items, want the cap %d", len(page.Items), MaxPageSize)
	}
}

// Paging must survive the cursor mail being deleted between two pages. A
// cursor that names a mail by id, and stops when that id is gone, ends the
// listing early with no NextCursor — the client believes it saw everything.
// That is the short-page-with-a-full-count defect this package exists to
// remove, arriving through the pager instead of the reader.
func TestPagingSurvivesTheCursorMailBeingDeleted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const total = 25
	for i := 0; i < total; i++ {
		mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		h.clock.advance(time.Second)
	}
	first, err := h.service.List(ctx, 1, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 7 || first.NextCursor == "" {
		t.Fatalf("first page: %d items, cursor %q", len(first.Items), first.NextCursor)
	}
	seen := map[string]bool{}
	for _, item := range first.Items {
		seen[item.Envelope.ID] = true
	}
	// The player deletes the last mail on the page they just read.
	if _, err := h.service.Delete(ctx, 1, first.Items[len(first.Items)-1].Envelope.ID); err != nil {
		t.Fatal(err)
	}
	cursor := first.NextCursor
	for pages := 0; pages < 20 && cursor != ""; pages++ {
		page, err := h.service.List(ctx, 1, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.Envelope.ID] {
				t.Fatalf("mail %s was listed twice", item.Envelope.ID)
			}
			seen[item.Envelope.ID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paging reached %d of %d mails after the cursor mail was deleted; the rest were silently lost", len(seen), total)
	}
}

// A retried send whose delivery failed must deliver on the retry. Send's own
// comment promises it: "a retry is a replay that will re-attempt delivery".
// Without it a broadcast whose fanout failed once is recorded as sent forever
// and every retry answers success — a mail nobody receives reported as
// delivered, which is the silent-drop pattern with a ledger in front of it.
func TestARetriedSendReattemptsAFailedBroadcastDelivery(t *testing.T) {
	attempts := 0
	failFirst := true
	h := newHarness(t, func(cfg *Config) {
		cfg.Broadcast = DelivererFunc(func(context.Context, Envelope) error {
			attempts++
			if failFirst {
				failFirst = false
				return fmt.Errorf("fanout queue is down")
			}
			return nil
		})
	})
	ctx := context.Background()
	req := SendRequest{
		Audience: AudienceBroadcast, Scope: "server-1", Subject: "maintenance",
		ExpiresInSeconds: 3600, RequestID: "send-b",
	}
	if _, err := h.service.Send(ctx, req); err == nil {
		t.Fatal("a send whose delivery failed reported success")
	}
	if _, err := h.service.Send(ctx, req); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("delivery was attempted %d times across the send and its retry, want 2: "+
			"the replay path returned the envelope without delivering it", attempts)
	}
	// Once delivered, further retries are pure replays.
	if _, err := h.service.Send(ctx, req); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("a replay after a successful delivery delivered again (%d attempts)", attempts)
	}
}

// The same property for direct mail: a delivery that fails on one recipient
// leaves the envelope and the ledger in place, so the retry must reach the
// recipient it missed. Player 2's mailbox is full for the first send.
func TestARetriedSendReachesTheRecipientItMissed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < MaxMailboxEntries; i++ {
		if err := h.service.Deliver(ctx, 2, fmt.Sprintf("old-%03d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	req := directTo(1, 2)
	if _, err := h.service.Send(ctx, req); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("a send into a full mailbox returned %v, want ErrMailboxFull", err)
	}
	// Player 2 makes room, then the sender retries.
	if _, err := h.service.Delete(ctx, 2, "old-000"); err != nil {
		t.Fatal(err)
	}
	envelope, err := h.service.Send(ctx, req)
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	mailbox, _, err := h.service.Mailbox(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mailbox.Entries[envelope.ID]; !ok {
		t.Fatal("the retried send never reached the recipient the first attempt missed")
	}
}

// Paging walks the whole mailbox exactly once, with no repeats and no gaps.
func TestPagingCoversTheMailboxExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want := map[string]bool{}
	for i := 0; i < 25; i++ {
		envelope := mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		want[envelope.ID] = true
		h.clock.advance(time.Second)
	}

	seen := map[string]int{}
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		page, err := h.service.List(ctx, 1, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			seen[item.Envelope.ID]++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(want) {
		t.Fatalf("paging saw %d mails, the mailbox holds %d", len(seen), len(want))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("mail %s appeared %d times across pages", id, count)
		}
		if !want[id] {
			t.Fatalf("paging produced mail %s that was never sent", id)
		}
	}
}

// A mailbox entry whose envelope is gone is reported as a drop, not skipped
// silently. Skipping silently is what let the replaced implementation return
// short pages while writing a full count.
func TestAMissingEnvelopeIsCountedNotSilentlySkipped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, directTo(1))
	h.envelopes.drop(envelope.ID)

	page, err := h.service.List(ctx, 1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("the page holds %d items, want 0", len(page.Items))
	}
	if got := h.metrics.Count("dropped:list.missing_envelope"); got != 1 {
		t.Fatalf("a missing envelope reported %d drops; %s", got, h.metrics.Events())
	}
}

// --- the unread count moves with the statuses it summarizes ---

// The confirmed defect: the count came from an unbounded aggregation run
// separately from the status writes, so it could disagree with them. Here it
// moves in the same compare-and-set.
func TestUnreadCountTracksStatusesExactly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		envelope := mustSend(t, h, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		ids = append(ids, envelope.ID)
	}
	assertUnread := func(want int32, why string) {
		t.Helper()
		summary, err := h.service.Summary(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Unread != want {
			t.Fatalf("%s: unread is %d, want %d", why, summary.Unread, want)
		}
		mailbox, _, err := h.service.Mailbox(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		counted := int32(0)
		for _, entry := range mailbox.Entries {
			if entry.Status == StatusUnread {
				counted++
			}
		}
		if counted != summary.Unread {
			t.Fatalf("%s: the count says %d but %d entries are unread; the count and the "+
				"statuses it summarizes have diverged", why, summary.Unread, counted)
		}
	}

	assertUnread(5, "after five deliveries")
	if _, err := h.service.MarkRead(ctx, 1, ids[0]); err != nil {
		t.Fatal(err)
	}
	assertUnread(4, "after one read")

	// A retried read must not move the count again.
	for i := 0; i < 3; i++ {
		if _, err := h.service.MarkRead(ctx, 1, ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	assertUnread(4, "after three retried reads")

	if _, err := h.service.Delete(ctx, 1, ids[1]); err != nil {
		t.Fatal(err)
	}
	assertUnread(3, "after deleting an unread mail")

	if _, err := h.service.ReserveClaim(ctx, 1, ids[2], ""); !errors.Is(err, ErrNoAttachment) {
		t.Fatalf("a mail with no attachment was claimable: %v", err)
	}
	assertUnread(3, "after a refused claim")
}

// A redelivery does not make a mail unread again and does not double the
// count. The transport is at-least-once, so this is the normal path.
func TestARedeliveryDoesNotResurrectAReadMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, directTo(1))
	if _, err := h.service.MarkRead(ctx, 1, envelope.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := h.service.Deliver(ctx, 1, envelope.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := h.service.Summary(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != 0 {
		t.Fatalf("four redeliveries of a read mail left %d unread, want 0", summary.Unread)
	}
	if got := h.metrics.Count("replayed:deliver"); got != 4 {
		t.Fatalf("four redeliveries reported %d replays; %s", got, h.metrics.Events())
	}
}

// Concurrent deliveries of distinct mails must all land, and the count must
// equal the number of mails — the property a non-CAS read-modify-write loses.
func TestConcurrentDeliveriesAllLand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const mails = 16
	ids := make([]string, 0, mails)
	for i := 0; i < mails; i++ {
		envelope, err := h.service.Send(ctx, SendRequest{
			Audience: AudienceDirect, Recipients: []int64{99},
			Subject: "mail", ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("send-%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, envelope.ID)
	}

	var wg sync.WaitGroup
	wg.Add(len(ids))
	for _, id := range ids {
		go func(id string) {
			defer wg.Done()
			if err := h.service.Deliver(ctx, 99, id, 0); err != nil {
				t.Errorf("deliver %s: %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	summary, err := h.service.Summary(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != mails {
		t.Fatalf("%d concurrent deliveries produced an unread count of %d, want %d",
			mails, summary.Unread, mails)
	}
}

// --- the mailbox bound refuses rather than silently discards ---

// A full mailbox with nothing evictable refuses the delivery. Silently
// discarding mail the player has not seen is the loss the bound exists to
// prevent, not to cause.
func TestAFullMailboxRefusesInsteadOfDroppingUnreadMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < MaxMailboxEntries; i++ {
		if err := h.service.Deliver(ctx, 1, fmt.Sprintf("mail-%d", i), 0); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	err := h.service.Deliver(ctx, 1, "one-too-many", 0)
	if !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("delivery into a full mailbox returned %v, want ErrMailboxFull", err)
	}
	if got := h.metrics.Count("refused:deliver:mailbox_full"); got != 1 {
		t.Fatalf("a refused delivery reported %d refusals; %s", got, h.metrics.Events())
	}
	summary, err := h.service.Summary(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != MaxMailboxEntries {
		t.Fatalf("the refused delivery changed the unread count to %d, want %d",
			summary.Unread, MaxMailboxEntries)
	}
}

// A full mailbox with terminal entries evicts the oldest of THOSE — never an
// unread mail — and counts what it dropped, so a client that lost history can
// see that it did.
//
// The layout matters: the unread mails are the OLDEST entries and the terminal
// ones are the NEWEST. An arrangement where the oldest entry happens to be
// terminal cannot tell "evicts the oldest evictable entry" apart from "evicts
// the oldest entry", and the second one silently discards mail the player
// never saw. The first version of this test had that arrangement and passed
// against an implementation that evicted unread mail.
func TestEvictionNeverDropsUnreadMailWhileTerminalEntriesExist(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const unread = MaxMailboxEntries - 10
	for i := 0; i < unread; i++ {
		if err := h.service.Deliver(ctx, 1, fmt.Sprintf("old-unread-%03d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Ten newer entries, made terminal. They are the youngest things in the
	// mailbox, so an implementation that evicts by age alone will not pick
	// them.
	h.clock.advance(time.Hour)
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("new-deleted-%03d", i)
		if err := h.service.Deliver(ctx, 1, id, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.Delete(ctx, 1, id); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(time.Second)
	}

	h.clock.advance(time.Hour)
	if err := h.service.Deliver(ctx, 1, "fresh", 0); err != nil {
		t.Fatalf("delivery into an evictable mailbox failed: %v", err)
	}

	mailbox, _, err := h.service.Mailbox(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Entries) > MaxMailboxEntries {
		t.Fatalf("the mailbox holds %d entries, above the bound %d", len(mailbox.Entries), MaxMailboxEntries)
	}
	if mailbox.Evicted != 1 {
		t.Fatalf("eviction reported %d dropped entries, want 1", mailbox.Evicted)
	}
	if _, ok := mailbox.Entries["fresh"]; !ok {
		t.Fatal("the fresh mail was evicted to make room for itself")
	}
	// The assertion that matters: not one unread mail was dropped.
	for i := 0; i < unread; i++ {
		id := fmt.Sprintf("old-unread-%03d", i)
		if _, ok := mailbox.Entries[id]; !ok {
			t.Fatalf("eviction dropped unread mail %s while ten deleted entries were available; "+
				"discarding mail the player never saw is the loss this bound exists to prevent", id)
		}
	}
	// And what went was the oldest terminal entry.
	if _, ok := mailbox.Entries["new-deleted-000"]; ok {
		t.Fatal("eviction kept the oldest terminal entry and dropped something else")
	}
	if summary, err := h.service.Summary(ctx, 1); err != nil {
		t.Fatal(err)
	} else if summary.Unread != unread+1 {
		t.Fatalf("the unread count is %d after eviction, want %d", summary.Unread, unread+1)
	}
}

// --- send idempotency ---

// A retried send returns the envelope it already produced.
func TestSendIsIdempotentPerRequestID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := mustSend(t, h, directTo(1))
	for attempt := 0; attempt < 4; attempt++ {
		again := mustSend(t, h, directTo(1))
		if again.ID != first.ID {
			t.Fatalf("attempt %d produced a second envelope %s (first %s)", attempt, again.ID, first.ID)
		}
	}
	if got := h.metrics.Count("replayed:send"); got != 4 {
		t.Fatalf("four retried sends reported %d replays; %s", got, h.metrics.Events())
	}
	summary, err := h.service.Summary(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != 1 {
		t.Fatalf("five sends of one request id produced %d unread mails, want 1", summary.Unread)
	}
}

// Concurrent sends with one request id produce exactly one envelope. A
// ledger that is checked and then written is a ledger two racers both pass.
func TestConcurrentSendsWithOneRequestIDProduceOneEnvelope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const racers = 12
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			envelope, err := h.service.Send(ctx, directTo(1))
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[envelope.ID] = true
		}()
	}
	wg.Wait()
	if len(ids) != 1 {
		t.Fatalf("%d concurrent sends with one request id produced %d distinct envelopes, want 1",
			racers, len(ids))
	}
	summary, err := h.service.Summary(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != 1 {
		t.Fatalf("the racing sends left %d unread mails, want 1", summary.Unread)
	}
}

// A send with no idempotency key is refused. The transports this service is
// reached over are at-least-once, so a keyless send is a duplicate mail per
// redelivery.
func TestASendWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	req := directTo(1)
	req.RequestID = ""
	if _, err := h.service.Send(context.Background(), req); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("a keyless send returned %v, want ErrRequestInvalid", err)
	}
}

// --- envelopes must expire ---

// A mail with no expiry is a row nothing removes. The implementation this
// replaces had no TTL index and could not add one, because its timestamps
// were int64 milliseconds rather than dates.
func TestAMailMustExpire(t *testing.T) {
	h := newHarness(t)
	req := directTo(1)
	req.ExpiresInSeconds = 0
	if _, err := h.service.Send(context.Background(), req); !errors.Is(err, ErrMailInvalid) {
		t.Fatalf("a mail with no expiry returned %v, want ErrMailInvalid", err)
	}
}

// An expired mail is not listed and not claimable, before anything sweeps.
func TestAnExpiredMailIsNeitherListedNorClaimable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, func() SendRequest {
		req := withAttachment(directTo(1), "100 gold")
		req.ExpiresInSeconds = 3600
		return req
	}())

	h.clock.advance(2 * time.Hour)

	page, err := h.service.List(ctx, 1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("an expired mail is still listed (%d items)", len(page.Items))
	}
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("an expired mail was claimable: %v", err)
	}
	if got := h.metrics.Count("refused:reserve_claim:expired"); got != 1 {
		t.Fatalf("an expired claim reported %d refusals; %s", got, h.metrics.Events())
	}
}

// --- configuration must not lie ---

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	base := func() Config {
		return Config{
			Envelopes: newFakeEnvelopes(),
			Mailboxes: versionstore.NewMemoryStore[int64, Mailbox](),
			Sends:     versionstore.NewMemoryStore[string, SentRecord](),
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no envelope store", func(c *Config) { c.Envelopes = nil }},
		{"no mailbox store", func(c *Config) { c.Mailboxes = nil }},
		{"no send ledger", func(c *Config) { c.Sends = nil }},
		{"negative claim lease", func(c *Config) { c.ClaimLease = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted a configuration with %s", tc.name)
			}
		})
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.Metrics = nil })
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatalf("a service with no reporter failed to reserve: %v", err)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.List(ctx, 1, "", 10); err != nil {
		t.Fatal(err)
	}
}
