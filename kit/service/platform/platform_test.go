package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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

// recordingDeliverer counts what it was asked to grant, per order. Counting
// per order rather than in total is the point: "delivered twice" is the
// defect, and a total would hide it behind a second order.
type recordingDeliverer struct {
	mu       sync.Mutex
	grants   map[string]int
	failWith error
	failFor  map[string]int
}

func newDeliverer() *recordingDeliverer {
	return &recordingDeliverer{grants: map[string]int{}, failFor: map[string]int{}}
}

func (d *recordingDeliverer) Deliver(_ context.Context, order Order) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if remaining := d.failFor[order.OrderID]; remaining > 0 {
		d.failFor[order.OrderID] = remaining - 1
		return fmt.Errorf("game is down")
	}
	if d.failWith != nil {
		return d.failWith
	}
	d.grants[order.OrderID]++
	return nil
}

func (d *recordingDeliverer) grantsFor(orderID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.grants[orderID]
}

const testPaymentSecret = "payment-secret-please-rotate"

type harness struct {
	service   *Service
	clock     *clock
	deliverer *recordingDeliverer
	orders    OrderStore
	metrics   *servicemetrics.Recorder
}

func acceptingVerifier() Verifier {
	return VerifierFunc(func(_ context.Context, credential Credential) (Verified, error) {
		if credential.Secret != "good" {
			return Verified{}, fmt.Errorf("bad credential")
		}
		return Verified{Channel: credential.Channel, OpenID: credential.OpenID}, nil
	})
}

// resolver maps a channel identity to a stable player id, the way an account
// service would.
func resolver() PlayerResolver {
	var mu sync.Mutex
	ids := map[string]int64{}
	next := int64(1000)
	return PlayerResolverFunc(func(_ context.Context, verified Verified) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		key := verified.Channel + ":" + verified.OpenID
		if id, ok := ids[key]; ok {
			return id, nil
		}
		next++
		ids[key] = next
		return next, nil
	})
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	h := &harness{
		clock:     c,
		deliverer: newDeliverer(),
		orders:    versionstore.NewMemoryStore[string, Order](),
		metrics:   servicemetrics.NewRecorder(),
	}
	cfg := Config{
		Orders:           h.orders,
		Deliver:          h.deliverer,
		Verifier:         acceptingVerifier(),
		Players:          resolver(),
		SessionSecret:    "session-secret-please-rotate",
		PaymentSecret:    testPaymentSecret,
		DeliveryAttempts: 3,
		DeliveryBackoff:  5 * time.Second,
		Now:              c.Now,
		Metrics:          h.metrics,
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

// callback builds a signed provider callback.
func callback(t *testing.T, orderID string, playerID int64, amount int64) ([]byte, string) {
	t.Helper()
	raw, err := json.Marshal(callbackPayload{
		OrderID: orderID, PlayerID: playerID, Channel: "store",
		ProductID: "gems-100", AmountMinor: amount, Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, SignPayload(raw, testPaymentSecret)
}

// --- defect 1: a session was minted from a player id in the request body ---

// The confirmed defect: AuthSession read a player id out of the request and
// signed a token for it. There was no credential in the request type at all.
// Anyone reaching the endpoint could obtain a valid session for any player.
//
// Here the player id is the OUTPUT of verification, so there is no way to
// express the vulnerable call.
func TestASessionRequiresACredentialTheChannelAccepts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.service.AuthSession(ctx, Credential{
		Channel: "store", OpenID: "u1", Secret: "forged",
	}); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a forged credential produced %v, want ErrIdentityDenied", err)
	}
	if got := h.metrics.Count("refused:auth_session:denied"); got != 1 {
		t.Fatalf("a denied credential reported %d refusals; %s", got, h.metrics.Events())
	}

	session, err := h.service.AuthSession(ctx, Credential{
		Channel: "store", OpenID: "u1", Secret: "good",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.PlayerID <= 0 {
		t.Fatalf("a verified credential produced player id %d", session.PlayerID)
	}
	if err := h.service.ValidateSession(session.PlayerID, session.Token); err != nil {
		t.Fatalf("the minted token does not validate: %v", err)
	}
	// And the token is bound to that player: it must not validate for another.
	if err := h.service.ValidateSession(session.PlayerID+1, session.Token); err == nil {
		t.Fatal("a token minted for one player validated for another")
	}
}

// A credential with no secret is the shape the replaced implementation
// accepted. Refusing it means the vulnerable call does not even validate.
func TestACredentialWithoutASecretIsRefused(t *testing.T) {
	h := newHarness(t)
	if _, err := h.service.AuthSession(context.Background(), Credential{
		Channel: "store", OpenID: "u1",
	}); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("a secretless credential produced %v, want ErrRequestInvalid", err)
	}
}

// A channel outage must not be reported to the player as a bad credential.
func TestAnUnreachableChannelIsNotADenial(t *testing.T) {
	h := newHarness(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(context.Context, Credential) (Verified, error) {
			return Verified{}, fmt.Errorf("dial store: %w", ErrVerifierDown)
		})
	})
	_, err := h.service.AuthSession(context.Background(), Credential{
		Channel: "store", OpenID: "u1", Secret: "good",
	})
	if !errors.Is(err, ErrVerifierDown) {
		t.Fatalf("an unreachable channel produced %v, want ErrVerifierDown", err)
	}
	if errors.Is(err, ErrIdentityDenied) {
		t.Fatal("an unreachable channel was reported as a denied identity")
	}
	if got := h.metrics.Count("refused:auth_session:verifier_down"); got != 1 {
		t.Fatalf("an outage reported %d refusals; %s", got, h.metrics.Events())
	}
}

// A verifier answering about a different channel than the one asked about is
// either misrouted or lying, and its answer is not about this request.
func TestAVerifierAnsweringForAnotherChannelIsRefused(t *testing.T) {
	h := newHarness(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(context.Context, Credential) (Verified, error) {
			return Verified{Channel: "some-other-store", OpenID: "u1"}, nil
		})
	})
	if _, err := h.service.AuthSession(context.Background(), Credential{
		Channel: "store", OpenID: "u1", Secret: "good",
	}); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a cross-channel answer produced %v, want ErrIdentityDenied", err)
	}
}

// --- defect 2: recharge delivery was stateless and not idempotent ---

// The confirmed defect: the callback was signature-verified and handed
// straight to the deliverer. A signature proves the payload came from the
// provider; it does NOT prove this is the first arrival. So replaying a
// captured callback — with its still-valid signature — granted the goods
// again.
//
// This is the test that would have caught it: the same signed bytes, replayed.
func TestAReplayedCallbackDeliversExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)

	first, err := h.service.HandleCallback(ctx, raw, signature)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Delivered {
		t.Fatal("the first callback did not deliver")
	}
	if first.Replayed {
		t.Fatal("the first callback was reported as a replay")
	}

	// The identical signed payload, ten more times. This is a captured
	// callback replayed, and it is also exactly what an at-least-once
	// transport in front of this endpoint does.
	for attempt := 0; attempt < 10; attempt++ {
		receipt, err := h.service.HandleCallback(ctx, raw, signature)
		if err != nil {
			t.Fatalf("replay %d failed: %v", attempt, err)
		}
		if !receipt.Replayed {
			t.Fatalf("replay %d was not reported as a replay", attempt)
		}
		if !receipt.Delivered {
			t.Fatalf("replay %d reported the order as undelivered", attempt)
		}
	}

	if grants := h.deliverer.grantsFor("order-1"); grants != 1 {
		t.Fatalf("eleven callbacks for one order granted the goods %d times, want 1; "+
			"a valid signature is necessary and not sufficient", grants)
	}
	if got := h.metrics.Count("replayed:callback"); got != 10 {
		t.Fatalf("ten replays reported %d; %s", got, h.metrics.Events())
	}
}

// Concurrent callbacks for one order deliver once. A read-then-write dedupe
// is a dedupe two racers both pass.
//
// What is asserted is the grant count and the final state, NOT that every
// racer got a nil error. ErrDeliveryHeld is a correct answer for a replay that
// arrives while the first delivery is still in flight — the order is recorded,
// someone is delivering it, come back later — and demanding that it never
// happens is a throughput claim wearing a correctness claim's name. It is also
// a claim that only held by luck: this test passed until the due-and-state
// check moved inside the compare-and-set, which is the change that made a
// second concurrent attempt correctly refuse instead of calling the deliverer
// a second time. It then failed 3 runs in 10.
func TestConcurrentCallbacksForOneOrderDeliverOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)

	const racers = 12
	var (
		wg       sync.WaitGroup
		outcomes sync.Map
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := h.service.HandleCallback(ctx, raw, signature)
			switch {
			case err == nil:
			case errors.Is(err, ErrDeliveryHeld):
				// The order is recorded and a delivery is in flight. A caller
				// retries; nothing is lost.
				outcomes.Store(i, "held")
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// The property that matters: the goods were granted once.
	if grants := h.deliverer.grantsFor("order-1"); grants != 1 {
		t.Fatalf("%d concurrent callbacks granted the goods %d times, want 1", racers, grants)
	}
	order, found, err := h.service.Order(ctx, "order-1")
	if err != nil || !found {
		t.Fatalf("order not found: %v", err)
	}
	if order.State != DeliveryDelivered {
		t.Fatalf("the order is %s after the race, want delivered", order.State)
	}
	// No racer consumed a second attempt: the in-flight refusal is what keeps
	// the budget from being spent by concurrency rather than by failure.
	//
	// This holds here but does not PROVE the refusal: whether a replay
	// overlaps the first delivery is a matter of timing, so a run where the
	// first finishes early would pass even without the refusal. The refusal
	// itself is established deterministically by
	// TestAnAttemptInFlightRefusesASecondOne, which re-enters AttemptDelivery
	// from inside the deliverer and therefore has no timing to depend on.
	if order.Attempts != 1 {
		t.Fatalf("the race consumed %d attempts, want 1; a concurrent replay must be refused "+
			"rather than claiming an attempt of its own", order.Attempts)
	}
}

// --- defect 3: the delivery error was discarded ---

// The confirmed defect: the error from a failed delivery was thrown away and
// replaced with a constant string, in a package with no logging and no
// metrics. A payment that failed to deliver left no trace on the server.
func TestAFailedDeliveryIsRecordedNotDiscarded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliverer.failFor["order-1"] = 1
	raw, signature := callback(t, "order-1", 1001, 499)

	if _, err := h.service.HandleCallback(ctx, raw, signature); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("a failed delivery produced %v, want ErrDeliveryFailed", err)
	}
	order, found, err := h.service.Order(ctx, "order-1")
	if err != nil || !found {
		t.Fatalf("a failed delivery left no order record: found=%v err=%v", found, err)
	}
	if order.LastError == "" {
		t.Fatal("a failed delivery recorded no error; a payment that fails silently is the defect this answers")
	}
	// The CAUSE is recorded, not a constant standing in for it: "game is
	// down" is what the deliverer said, and it is what an operator needs.
	// Replacing the recorded error with a fixed string kept this test green
	// until this assertion existed (U-0018).
	if !strings.Contains(order.LastError, "game is down") {
		t.Fatalf("the recorded error %q does not carry the deliverer's cause", order.LastError)
	}
	if order.Attempts != 1 {
		t.Fatalf("the order records %d attempts, want 1", order.Attempts)
	}
	if order.State != DeliveryReserved {
		t.Fatalf("the order is %s after one failure, want reserved", order.State)
	}
	if got := h.metrics.Count("refused:deliver:failed"); got != 1 {
		t.Fatalf("a failed delivery reported %d refusals; %s", got, h.metrics.Events())
	}
}

// A retry after a transient failure delivers, once.
func TestARetryAfterATransientFailureDeliversOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliverer.failFor["order-1"] = 1
	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); err == nil {
		t.Fatal("the failing delivery reported success")
	}

	// The backoff has to elapse first: a retry inside it is refused, which is
	// what keeps a down game from being hammered.
	if _, err := h.service.AttemptDelivery(ctx, "order-1"); !errors.Is(err, ErrDeliveryHeld) {
		t.Fatalf("a retry inside the backoff produced %v, want ErrDeliveryHeld", err)
	}
	h.clock.advance(time.Minute)

	receipt, err := h.service.AttemptDelivery(ctx, "order-1")
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !receipt.Delivered || receipt.Replayed {
		t.Fatalf("the successful retry reported delivered=%v replayed=%v, want delivered and not a replay",
			receipt.Delivered, receipt.Replayed)
	}
	order := receipt.Order
	if order.State != DeliveryDelivered {
		t.Fatalf("the order is %s after a successful retry, want delivered", order.State)
	}
	if grants := h.deliverer.grantsFor("order-1"); grants != 1 {
		t.Fatalf("the retry granted the goods %d times, want 1", grants)
	}
	if order.LastError != "" {
		t.Fatalf("a delivered order still carries an error: %q", order.LastError)
	}
}

// The attempt budget is bounded, and exhausting it is a terminal state that
// counts. An unbounded retry queue is a queue that never drains and never
// reports that it is not draining.
func TestTheAttemptBudgetIsBoundedAndExhaustionCounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliverer.failWith = fmt.Errorf("game is permanently down")
	raw, signature := callback(t, "order-1", 1001, 499)

	if _, err := h.service.HandleCallback(ctx, raw, signature); err == nil {
		t.Fatal("the failing delivery reported success")
	}
	for attempt := 0; attempt < 10; attempt++ {
		h.clock.advance(time.Hour)
		if _, err := h.service.AttemptDelivery(ctx, "order-1"); errors.Is(err, ErrDeliveryExpired) {
			break
		}
	}

	order, found, err := h.service.Order(ctx, "order-1")
	if err != nil || !found {
		t.Fatal("order not found")
	}
	if order.State != DeliveryExhausted {
		t.Fatalf("the order is %s after ten failures with a budget of 3, want exhausted", order.State)
	}
	if order.Attempts > order.MaxAttempts {
		t.Fatalf("the order made %d attempts against a budget of %d", order.Attempts, order.MaxAttempts)
	}
	if got := h.metrics.Count("dropped:order.exhausted"); got == 0 {
		t.Fatalf("an exhausted order reported no drops; %s", h.metrics.Events())
	}
	// And an exhausted order does not quietly become deliverable again.
	h.deliverer.failWith = nil
	h.clock.advance(time.Hour)
	if _, err := h.service.AttemptDelivery(ctx, "order-1"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("an exhausted order produced %v on retry, want ErrDeliveryExpired", err)
	}
	if grants := h.deliverer.grantsFor("order-1"); grants != 0 {
		t.Fatalf("an exhausted order was still delivered (%d grants)", grants)
	}
}

// An attempt is claimed BEFORE the deliverer runs, so a crash mid-flight
// consumes an attempt rather than being invisible. An attempt counter
// incremented after a successful call counts successes, not attempts.
func TestAnAttemptIsClaimedBeforeTheDelivererRuns(t *testing.T) {
	var seen int32
	h := newHarness(t, func(cfg *Config) {
		cfg.Deliver = DelivererFunc(func(_ context.Context, order Order) error {
			// What the deliverer sees is the order as recorded, so the
			// attempt must already be counted when it is called.
			seen = order.Attempts
			return nil
		})
	})
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("the deliverer saw attempt %d, want 1; an attempt counted after the "+
			"call counts successes rather than attempts", seen)
	}
}

// A delivered order's delivery time does not move on retry, or "when was
// order X delivered" becomes whenever it was last retried.
func TestADeliveredOrdersTimeDoesNotMoveOnRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); err != nil {
		t.Fatal(err)
	}
	first, _, _ := h.service.Order(ctx, "order-1")

	h.clock.advance(time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := h.service.AttemptDelivery(ctx, "order-1"); err != nil {
			t.Fatal(err)
		}
	}
	again, _, _ := h.service.Order(ctx, "order-1")
	if again.DeliveredAtUnix != first.DeliveredAtUnix {
		t.Fatalf("the delivery time moved from %d to %d on retry",
			first.DeliveredAtUnix, again.DeliveredAtUnix)
	}
	if grants := h.deliverer.grantsFor("order-1"); grants != 1 {
		t.Fatalf("retrying a delivered order granted the goods %d times, want 1", grants)
	}
}

// --- configuration must not lie ---

// Every one of these was absent in the replaced implementation, and the
// absence looked like a default. An empty payment secret in particular was
// checked at call time and turned every callback into an invalid-signature
// refusal: a silent outage that looks like an attack.
func TestNewRefusesAConfigurationThatCannotAuthenticateOrDedupe(t *testing.T) {
	base := func() Config {
		return Config{
			Orders:        versionstore.NewMemoryStore[string, Order](),
			Deliver:       newDeliverer(),
			Verifier:      acceptingVerifier(),
			Players:       resolver(),
			SessionSecret: "s",
			PaymentSecret: "p",
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no order store", func(c *Config) { c.Orders = nil }},
		{"no deliverer", func(c *Config) { c.Deliver = nil }},
		{"no verifier", func(c *Config) { c.Verifier = nil }},
		{"no player resolver", func(c *Config) { c.Players = nil }},
		{"no session secret", func(c *Config) { c.SessionSecret = "" }},
		{"no payment secret", func(c *Config) { c.PaymentSecret = "" }},
		{"negative attempt budget", func(c *Config) { c.DeliveryAttempts = -1 }},
		{"negative backoff", func(c *Config) { c.DeliveryBackoff = -time.Second }},
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
	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); err != nil {
		t.Fatalf("a service with no reporter failed to handle a callback: %v", err)
	}
	if _, err := h.service.AuthSession(ctx, Credential{Channel: "store", OpenID: "u1", Secret: "good"}); err != nil {
		t.Fatal(err)
	}
}

// The player is resolved from what the CHANNEL said, not from what the client
// claimed. These differ whenever a channel canonicalizes — an alias, a
// case-folded id, a migrated account — and resolving from the claim is how a
// caller picks its own identity while a verifier appears to be running.
func TestThePlayerIsResolvedFromTheVerifiedIdentityNotTheClaimedOne(t *testing.T) {
	var resolved []Verified
	h := newHarness(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(_ context.Context, credential Credential) (Verified, error) {
			if credential.Secret != "good" {
				return Verified{}, fmt.Errorf("bad credential")
			}
			// The channel says this credential is really account "canonical".
			return Verified{Channel: credential.Channel, OpenID: "canonical"}, nil
		})
		cfg.Players = PlayerResolverFunc(func(_ context.Context, verified Verified) (int64, error) {
			resolved = append(resolved, verified)
			return 1234, nil
		})
	})

	session, err := h.service.AuthSession(context.Background(), Credential{
		Channel: "store", OpenID: "whatever-the-client-typed", Secret: "good",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("the resolver ran %d times, want 1", len(resolved))
	}
	if resolved[0].OpenID != "canonical" {
		t.Fatalf("the player was resolved from open id %q, but the channel said %q; "+
			"resolving from the claim lets a caller pick its own identity",
			resolved[0].OpenID, "canonical")
	}
	if session.Channel != "store" {
		t.Fatalf("the session names channel %q, want store", session.Channel)
	}
}

// After the last permitted failure the order is ALREADY terminal, without
// waiting for another attempt to notice. An order that still looks retryable
// after its budget is spent is an order an operator listing "pending
// deliveries" will keep seeing forever.
func TestAnOrderIsTerminalImmediatelyAfterItsLastPermittedFailure(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.DeliveryAttempts = 2 })
	ctx := context.Background()
	h.deliverer.failWith = fmt.Errorf("game is permanently down")
	raw, signature := callback(t, "order-1", 1001, 499)

	// Attempt 1, via the callback.
	if _, err := h.service.HandleCallback(ctx, raw, signature); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("attempt 1 produced %v, want ErrDeliveryFailed", err)
	}
	if order, _, _ := h.service.Order(ctx, "order-1"); order.State != DeliveryReserved {
		t.Fatalf("after 1 of 2 attempts the order is %s, want reserved", order.State)
	}

	// Attempt 2 spends the budget. The order must be exhausted when this
	// call returns — not on the next one.
	h.clock.advance(time.Hour)
	final, err := h.service.AttemptDelivery(ctx, "order-1")
	if !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("attempt 2 produced %v, want ErrDeliveryExpired", err)
	}
	if final.Delivered {
		t.Fatal("an exhausted order was reported as delivered")
	}
	if final.Order.State != DeliveryExhausted {
		t.Fatalf("the order returned from the last attempt is %s, want exhausted", final.Order.State)
	}
	stored, _, _ := h.service.Order(ctx, "order-1")
	if stored.State != DeliveryExhausted {
		t.Fatalf("the stored order is %s right after its last permitted failure, want exhausted; "+
			"an order that still looks retryable is one an operator keeps seeing as pending",
			stored.State)
	}
	if stored.NextAttemptAtUnix != 0 {
		t.Fatalf("an exhausted order still carries a next-attempt time (%d)", stored.NextAttemptAtUnix)
	}
}

// Two delivery attempts racing must grant once. The early return on a
// delivered order cannot cover this — both attempts pass it before either
// commits — so the compare-and-set inside the commit is what has to hold.
//
// The order is put into the reserved state deterministically first (by a
// callback whose one delivery attempt fails), so the race under test is the
// two attempts and not whether the order exists yet.
func TestTwoRacingDeliveryAttemptsGrantOnce(t *testing.T) {
	var (
		mu     sync.Mutex
		grants int
	)
	h := newHarness(t, func(cfg *Config) {
		cfg.DeliveryAttempts = 5
		cfg.Deliver = DelivererFunc(func(context.Context, Order) error {
			mu.Lock()
			defer mu.Unlock()
			grants++
			if grants == 1 {
				// The setup attempt fails, so the order lands in reserved
				// deterministically and the race under test is the two
				// attempts rather than whether the order exists yet.
				return fmt.Errorf("not yet")
			}
			return nil
		})
	})
	ctx := context.Background()

	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("the setup callback produced %v, want ErrDeliveryFailed", err)
	}
	if order, found, _ := h.service.Order(ctx, "order-1"); !found || order.State != DeliveryReserved {
		t.Fatalf("the order is not reserved before the race (found=%v)", found)
	}
	mu.Lock()
	grants = 1 // the setup failure consumed the first call
	mu.Unlock()

	// Past the setup attempt's backoff, so the order is due again.
	h.clock.advance(time.Minute)

	// Both racers block on one barrier, so their compare-and-set calls race.
	// That is where the decision has to be: with the due-and-state check
	// before the CAS, both pass it and both reach the deliverer.
	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		outcomeMu sync.Mutex
		granters  int
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			receipt, err := h.service.AttemptDelivery(ctx, "order-1")
			if err != nil {
				return
			}
			if receipt.Delivered && !receipt.Replayed {
				outcomeMu.Lock()
				granters++
				outcomeMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	order, found, err := h.service.Order(ctx, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the order was not recorded")
	}
	if order.State != DeliveryDelivered {
		t.Fatalf("the order is %s after two racing attempts, want delivered", order.State)
	}
	if order.DeliveredAtUnix == 0 {
		t.Fatal("a delivered order carries no delivery time")
	}
	if order.Attempts > order.MaxAttempts {
		t.Fatalf("the racing attempts made %d attempts against a budget of %d",
			order.Attempts, order.MaxAttempts)
	}
	// Exactly one caller may believe IT granted the goods. Two would mean two
	// callers each running whatever they do after a successful grant.
	if granters != 1 {
		t.Fatalf("%d of two racing attempts reported that they granted the goods, want 1", granters)
	}
	// And the deliverer was called once past the setup. This is the assertion
	// the first version of this service failed: with the due-and-state check
	// outside the compare-and-set, both attempts passed it and both called
	// the granting side, leaving "the deliverer must dedupe" load-bearing
	// rather than belt-and-braces.
	mu.Lock()
	granted := grants
	mu.Unlock()
	if granted != 2 {
		t.Fatalf("the deliverer was called %d times in total (1 setup + 1 expected), want 2", granted)
	}
}

// While an attempt is in flight, another is refused — deterministically, with
// no reliance on goroutine interleaving.
//
// The deliverer re-enters AttemptDelivery for its own order, which is exactly
// the state a concurrent sweep would find: the attempt is claimed, the
// granting side has not answered yet. The refusal has to come from inside the
// compare-and-set that claimed the attempt; a due-and-state check performed
// before it would already have passed.
func TestAnAttemptInFlightRefusesASecondOne(t *testing.T) {
	var (
		calls     int
		reentrant error
		reentered bool
		// The deliverer has to call back into the service that is calling it,
		// so it captures a pointer that New fills in rather than the config
		// being edited after construction.
		service *Service
	)
	h := newHarness(t, func(cfg *Config) {
		cfg.Deliver = DelivererFunc(func(inner context.Context, order Order) error {
			calls++
			if !reentered {
				reentered = true
				_, reentrant = service.AttemptDelivery(inner, order.OrderID)
			}
			return nil
		})
	})
	service = h.service
	ctx := context.Background()

	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(reentrant, ErrDeliveryHeld) {
		t.Fatalf("a second attempt while one was in flight produced %v, want ErrDeliveryHeld; "+
			"a due check performed before the claim would already have passed", reentrant)
	}
	if calls != 1 {
		t.Fatalf("the deliverer was called %d times for one order, want 1", calls)
	}
}

// A callback for an order that already exists must not overwrite it.
//
// This is the insert-only reservation, asserted deterministically: an
// unconditional write would reset the attempt count and clear the recorded
// error, losing exactly the delivery history an operator needs. The
// concurrency test above covers the same property but only detects a
// read-then-write on an unlucky interleaving, which is not a test.
func TestACallbackForAnExistingOrderDoesNotOverwriteIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliverer.failFor["order-1"] = 1
	raw, signature := callback(t, "order-1", 1001, 499)

	// First callback: recorded, delivery fails, history written.
	if _, err := h.service.HandleCallback(ctx, raw, signature); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("the first callback produced %v, want ErrDeliveryFailed", err)
	}
	before, found, err := h.service.Order(ctx, "order-1")
	if err != nil || !found {
		t.Fatal("the order was not recorded")
	}
	if before.Attempts != 1 || before.LastError == "" {
		t.Fatalf("the first failure recorded attempts=%d error=%q", before.Attempts, before.LastError)
	}

	// A provider retry arrives before the backoff elapses. It must be
	// answered as a replay and must leave the record alone.
	receipt, err := h.service.HandleCallback(ctx, raw, signature)
	if !errors.Is(err, ErrDeliveryHeld) {
		t.Fatalf("a retry inside the backoff produced %v, want ErrDeliveryHeld", err)
	}
	if !receipt.Replayed {
		t.Fatal("a retry for a recorded order was not reported as a replay")
	}
	after, _, err := h.service.Order(ctx, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Attempts != before.Attempts {
		t.Fatalf("the retry reset the attempt count from %d to %d; an unconditional write "+
			"loses the delivery history the retry budget is computed from",
			before.Attempts, after.Attempts)
	}
	if after.LastError != before.LastError {
		t.Fatalf("the retry cleared the recorded error (%q -> %q)", before.LastError, after.LastError)
	}
	if after.CreatedAtUnix != before.CreatedAtUnix {
		t.Fatalf("the retry moved the creation time from %d to %d", before.CreatedAtUnix, after.CreatedAtUnix)
	}
}
