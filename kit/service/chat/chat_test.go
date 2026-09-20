package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/errcode"
	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// --- test doubles ---

// clock is the injected clock. It can also run backwards, which one test
// requires: ordering must come from the sequence, never from a wall clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_700_000_000, 0)} }

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

// allowAllPolicy is a permissive policy. **It exists for tests only** — there
// is deliberately no permissive default in the production package, because a
// permissive default is precisely what the client-settable Trusted bool
// amounted to in the implementation this replaces.
type allowAllPolicy struct{}

func (allowAllPolicy) CanPublish(context.Context, Sender, Channel) error { return nil }
func (allowAllPolicy) CanRead(context.Context, Sender, Channel) error    { return nil }

// recordingPolicy counts calls and can refuse, so a test can prove the policy
// is consulted on every publish and every read.
type recordingPolicy struct {
	mu          sync.Mutex
	publishSeen int
	readSeen    int
	publishErr  error
	readErr     error
}

func (p *recordingPolicy) CanPublish(context.Context, Sender, Channel) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishSeen++
	return p.publishErr
}

func (p *recordingPolicy) CanRead(context.Context, Sender, Channel) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readSeen++
	return p.readErr
}

func (p *recordingPolicy) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishSeen, p.readSeen
}

// recorder collects the metric signals. Constraint 6: a silent path survives
// because nothing measures it.
// recorder is the shared test Recorder plus the per-signal names this
// package's assertions already speak. It exists so chat's existing tests keep
// reading as chat tests while there is still exactly one Reporter
// implementation under test.
type recorder struct{ *servicemetrics.Recorder }

func newRecorder() *recorder { return &recorder{Recorder: servicemetrics.NewRecorder()} }

// counters is a flat read of the signals this package asserts on. Publishes
// are summed across channel kinds, because most assertions here care about
// "did the append happen" and the per-kind breakdown has its own test.
type counters struct {
	stored    int
	replayed  int
	denied    int
	evicted   int
	gaps      int
	conflicts map[string]int
	ops       map[string]int
}

func (r *recorder) snapshot() counters {
	out := counters{conflicts: map[string]int{}, ops: r.Recorder.Snapshot()}
	for event, count := range out.ops {
		switch {
		case strings.HasPrefix(event, "accepted:publish."):
			out.stored += count
		case strings.HasPrefix(event, "replayed:publish."):
			out.replayed += count
		case strings.HasPrefix(event, "refused:publish."):
			out.denied += count
		case strings.HasPrefix(event, "dropped:message.evicted."):
			out.evicted += count
		case strings.HasPrefix(event, "dropped:history.gap."):
			out.gaps += count
		case strings.HasPrefix(event, "conflict:"):
			out.conflicts[strings.TrimPrefix(event, "conflict:")] += count
		}
	}
	return out
}

// brokenState fails the backend, so a test can prove a backend failure is
// reported as a store failure while a client mistake never is.
type brokenState struct {
	inner     StateStore
	updateErr error
	getErr    error
}

func (s brokenState) Get(ctx context.Context, key string) (versionstore.Versioned[channelState], bool, error) {
	if s.getErr != nil {
		return versionstore.Versioned[channelState]{}, false, s.getErr
	}
	return s.inner.Get(ctx, key)
}

func (s brokenState) Update(ctx context.Context, key string, mutate versionstore.Mutate[channelState]) (versionstore.Versioned[channelState], bool, error) {
	if s.updateErr != nil {
		return versionstore.Versioned[channelState]{}, false, s.updateErr
	}
	return s.inner.Update(ctx, key, mutate)
}

func (s brokenState) Create(ctx context.Context, key string, value channelState) (versionstore.Versioned[channelState], bool, error) {
	return s.inner.Create(ctx, key, value)
}

func (s brokenState) Delete(ctx context.Context, key string, expect versionstore.Versioned[channelState]) error {
	return s.inner.Delete(ctx, key, expect)
}

// --- message types the tests register ---
//
// Note that the package ships none of these. Every type a deployment uses is
// registered by the deployment, which is what keeps gameplay entities — the
// position share, the battle-report share, the guild invite that used to be in
// the storage schema — out of this package.
const (
	typeText     MessageType = "text"
	typeNotice   MessageType = "system.notice"
	typeSignal   MessageType = "signal"
	typeUnknown  MessageType = "gameplay.position_share"
	typeTinyBody MessageType = "tiny"
)

func testRegistry(t *testing.T) *BodyRegistry {
	t.Helper()
	registry := NewBodyRegistry()
	registry.MustRegister(typeText, BodySpec{Validator: TextValidator(200), MaxBytes: 1024})
	// A system-only type: the implementation this replaces had one, and a
	// client-set boolean unlocked it.
	registry.MustRegister(typeNotice, BodySpec{Validator: TextValidator(200), SystemOnly: true})
	registry.MustRegister(typeSignal, BodySpec{Validator: BodyValidatorFunc(func([]byte) error { return nil }), AllowEmpty: true})
	registry.MustRegister(typeTinyBody, BodySpec{Validator: BodyValidatorFunc(func([]byte) error { return nil }), MaxBytes: 8})
	return registry
}

type harness struct {
	store    Store
	service  *Service
	clock    *clock
	policy   ChannelPolicy
	metrics  *recorder
	registry *BodyRegistry
	state    StateStore
}

type option func(*Config, *ServiceConfig, *harness)

func withPolicy(policy ChannelPolicy) option {
	return func(cfg *Config, _ *ServiceConfig, h *harness) { cfg.Policy = policy; h.policy = policy }
}

func withRules(rules ...ChannelRule) option {
	return func(cfg *Config, _ *ServiceConfig, _ *harness) { cfg.Rules = rules }
}

func withRetentionAge(age time.Duration) option {
	return func(cfg *Config, _ *ServiceConfig, _ *harness) { cfg.RetentionAge = age }
}

func withSystemAuth(auth SystemAuthenticator) option {
	return func(_ *Config, svc *ServiceConfig, _ *harness) { svc.System = auth }
}

func withState(state StateStore) option {
	return func(_ *Config, _ *ServiceConfig, h *harness) { h.state = state }
}

// grantingAuth stands in for the transport: it is what turns an
// internal-listener or mTLS identity into a token. The token is minted here, in
// server code, and never from a request field.
func grantingAuth() SystemAuthenticator {
	return SystemAuthenticatorFunc(func(context.Context) (SystemToken, error) { return GrantSystem(), nil })
}

func newHarness(t *testing.T, options ...option) *harness {
	t.Helper()
	h := &harness{clock: newClock(), metrics: newRecorder(), registry: testRegistry(t)}
	h.policy = allowAllPolicy{}
	cfg := Config{Policy: h.policy, Bodies: h.registry, Now: h.clock.Now, Metrics: h.metrics}
	svcCfg := ServiceConfig{System: grantingAuth()}
	for _, apply := range options {
		apply(&cfg, &svcCfg, h)
	}
	cfg.Policy = h.policy
	if h.state == nil {
		h.state = versionstore.NewMemoryStore[string, channelState]()
	}
	store, err := NewStore(h.state, cfg)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	h.store = store
	svcCfg.Store = store
	service, err := NewService(svcCfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	h.service = service
	return h
}

func role(id int64) Sender { return Sender{RoleID: id, Name: fmt.Sprintf("role-%d", id)} }

func world() Channel { return Channel{Kind: ChannelWorld, Target: 7} }

func private(peer int64) Channel { return Channel{Kind: ChannelPrivate, Target: peer} }

func text(body string, requestID string, ch Channel) PublishRequest {
	return PublishRequest{Channel: ch, Type: typeText, Body: []byte(body), RequestID: requestID}
}

func mustPublish(t *testing.T, h *harness, from Sender, req PublishRequest) Message {
	t.Helper()
	message, err := h.service.Publish(context.Background(), from, req)
	if err != nil {
		t.Fatalf("publish %q: %v", req.RequestID, err)
	}
	return message
}

func bodies(page Page) []string {
	out := make([]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		out = append(out, string(message.Body))
	}
	return out
}

// --- defect 1: trust cannot be expressed in a request ---

// The request types must not be able to carry trust, identity or ordering. A
// reflection test rather than a prose comment, because the defect was a single
// extra field: re-adding one turns this red.
func TestRequestTypesCannotCarryTrustOrIdentity(t *testing.T) {
	for _, testCase := range []struct {
		label   string
		value   any
		allowed []string
	}{
		{"PublishRequest", PublishRequest{}, []string{"Channel", "Type", "Body", "RequestID"}},
		{"SystemPublishRequest", SystemPublishRequest{}, []string{"Channel", "Actor", "Type", "Body", "RequestID"}},
		{"HistoryQuery", HistoryQuery{}, []string{"Channel", "AfterSeq", "BeforeSeq", "Limit"}},
	} {
		want := map[string]bool{}
		for _, name := range testCase.allowed {
			want[name] = true
		}
		value := reflect.TypeOf(testCase.value)
		for i := 0; i < value.NumField(); i++ {
			name := value.Field(i).Name
			if !want[name] {
				t.Fatalf("%s gained field %q: a request must not carry trust, a sender, an origin or a timestamp", testCase.label, name)
			}
			delete(want, name)
		}
		for name := range want {
			t.Fatalf("%s lost field %q", testCase.label, name)
		}
	}
}

// A SystemToken cannot come off the wire. Decoding a body that tries to grant
// itself trust produces a token that grants nothing.
func TestSystemTokenCannotBeDecodedFromARequestBody(t *testing.T) {
	h := newHarness(t)
	var token SystemToken
	for _, payload := range []string{`{"granted":true}`, `{"Granted":true}`, `true`, `{}`} {
		if err := json.Unmarshal([]byte(payload), &token); err == nil && token.valid() {
			t.Fatalf("payload %s produced a valid system token", payload)
		}
		token = SystemToken{}
	}
	if reflect.TypeOf(SystemToken{}).NumField() != 1 || reflect.TypeOf(SystemToken{}).Field(0).IsExported() {
		t.Fatal("SystemToken must have exactly one unexported field, or a request body could construct one")
	}
	// The seam refuses a zero token even when called directly.
	_, err := h.store.AppendSystem(context.Background(), SystemToken{}, SystemPublishRequest{
		Channel: Channel{Kind: ChannelSystem}, Actor: "gm", Type: typeNotice, Body: []byte("hi"), RequestID: "r1",
	})
	if !errors.Is(err, ErrSystemDenied) {
		t.Fatalf("a zero token was accepted by the seam: %v", err)
	}
	if got := Code(err); got != CodeSystemDenied {
		t.Fatalf("code = %d, want %d", got, CodeSystemDenied)
	}
}

// The privileged entry point is the only way to send a system message.
func TestOnlyThePrivilegedEntryPointSendsSystemMessages(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// A role cannot publish a system-only type, on any channel.
	_, err := h.service.Publish(ctx, role(1), PublishRequest{
		Channel: world(), Type: typeNotice, Body: []byte("server restarting"), RequestID: "r1",
	})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a role published a system-only type: %v", err)
	}
	// A role cannot publish to a system-only channel, with any type.
	_, err = h.service.Publish(ctx, role(1), PublishRequest{
		Channel: Channel{Kind: ChannelSystem}, Type: typeText, Body: []byte("hello"), RequestID: "r2",
	})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a role published to a system-only channel: %v", err)
	}
	if denied := h.metrics.snapshot().denied; denied != 2 {
		t.Fatalf("denied publishes counted %d times, want 2", denied)
	}

	// A service wired without an authenticator has no system path at all.
	bare, err := NewService(ServiceConfig{Store: h.store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bare.PublishSystem(ctx, SystemPublishRequest{
		Channel: Channel{Kind: ChannelSystem}, Actor: "gm", Type: typeNotice, Body: []byte("hi"), RequestID: "r3",
	})
	if !errors.Is(err, ErrSystemDenied) {
		t.Fatalf("a service with no authenticator published a system message: %v", err)
	}

	// An authenticator that refuses, and one that grants nothing without
	// erroring, both fail closed.
	for label, auth := range map[string]SystemAuthenticator{
		"refusing": SystemAuthenticatorFunc(func(context.Context) (SystemToken, error) {
			return SystemToken{}, errors.New("peer is not internal")
		}),
		"silent": SystemAuthenticatorFunc(func(context.Context) (SystemToken, error) {
			return SystemToken{}, nil
		}),
	} {
		guarded := newHarness(t, withSystemAuth(auth))
		_, err := guarded.service.PublishSystem(ctx, SystemPublishRequest{
			Channel: Channel{Kind: ChannelSystem}, Actor: "gm", Type: typeNotice, Body: []byte("hi"), RequestID: "r4",
		})
		if !errors.Is(err, ErrSystemDenied) {
			t.Fatalf("%s authenticator: %v", label, err)
		}
	}

	// The granted path works, and records who spoke.
	message, err := h.service.PublishSystem(ctx, SystemPublishRequest{
		Channel: Channel{Kind: ChannelSystem}, Actor: "gm-console", Type: typeNotice, Body: []byte("server restarting"), RequestID: "r5",
	})
	if err != nil {
		t.Fatalf("privileged publish: %v", err)
	}
	if message.Origin != OriginSystem {
		t.Fatalf("origin = %q, want %q", message.Origin, OriginSystem)
	}
	if message.From.RoleID != 0 || message.From.Name != "gm-console" {
		t.Fatalf("from = %+v, want role 0 named gm-console", message.From)
	}
	// A privileged publish still needs an actor and an idempotency key.
	// Each refusal names what is missing with its own code; "any error" is
	// the assertion that lets a refusal for the wrong reason through.
	for label, testCase := range map[string]struct {
		req  SystemPublishRequest
		want error
	}{
		"no actor": {SystemPublishRequest{Channel: Channel{Kind: ChannelSystem}, Type: typeNotice, Body: []byte("x"), RequestID: "r6"}, ErrSenderInvalid},
		"no key":   {SystemPublishRequest{Channel: Channel{Kind: ChannelSystem}, Actor: "gm", Type: typeNotice, Body: []byte("x")}, ErrRequestInvalid},
	} {
		if _, err := h.service.PublishSystem(ctx, testCase.req); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: err = %v, want %v", label, err, testCase.want)
		}
	}
	// A role message never gets the system origin.
	roleMessage := mustPublish(t, h, role(1), text("hello", "r7", world()))
	if roleMessage.Origin != OriginRole {
		t.Fatalf("a role publish stored origin %q", roleMessage.Origin)
	}
}

// --- defect 2: private history ---

// A private conversation is one stream: the caller's own messages are in it,
// and it is scoped to one peer.
func TestPrivateHistoryIncludesOwnMessagesAndIsScopedToOnePeer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	mustPublish(t, h, role(1), text("1 to 2", "a", private(2)))
	mustPublish(t, h, role(2), text("2 to 1", "b", private(1)))
	mustPublish(t, h, role(1), text("1 to 3", "c", private(3)))
	mustPublish(t, h, role(3), text("3 to 1", "d", private(1)))

	page, err := h.service.Conversation(ctx, role(1), 2, 0, 10)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	got := bodies(page)
	want := []string{"1 to 2", "2 to 1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation with peer 2 = %v, want %v (the caller's own message must be included)", got, want)
	}
	// Scoped: the conversation with 3 is a different set.
	page, err = h.service.Conversation(ctx, role(1), 3, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(page), []string{"1 to 3", "3 to 1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation with peer 3 = %v, want %v", got, want)
	}
	// Symmetric: both participants see the same stream, with the same
	// sequences.
	mine, err := h.service.Conversation(ctx, role(1), 2, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := h.service.Conversation(ctx, role(2), 1, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mine.Messages, theirs.Messages) {
		t.Fatalf("the two participants see different conversations:\n%v\n%v", bodies(mine), bodies(theirs))
	}
	// A third party's conversation with 3 is a different stream, so it cannot
	// reach what 1 and 3 said: the isolation is structural, not a filter.
	page, err = h.service.Conversation(ctx, role(9), 3, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 {
		t.Fatalf("role 9 read %d messages from the 1-3 conversation", len(page.Messages))
	}
	// The same property at the ref level.
	forward, err := h.store.Resolve(private(2), 1)
	if err != nil {
		t.Fatal(err)
	}
	backward, err := h.store.Resolve(private(1), 2)
	if err != nil {
		t.Fatal(err)
	}
	if forward.Key() != backward.Key() {
		t.Fatalf("pair refs are asymmetric: %q vs %q", forward.Key(), backward.Key())
	}
	// A shared channel is one stream for everyone in it.
	mustPublish(t, h, role(1), text("hi world", "w1", world()))
	shared, err := h.service.History(ctx, role(5), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(shared); !reflect.DeepEqual(got, []string{"hi world"}) {
		t.Fatalf("world history for another role = %v", got)
	}
}

func TestPrivateChannelRequiresAPeerThatIsNotTheCaller(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, testCase := range []struct {
		label   string
		channel Channel
	}{
		{"no target", Channel{Kind: ChannelPrivate}},
		{"self target", private(1)},
		{"negative target", Channel{Kind: ChannelPrivate, Target: -1}},
	} {
		_, err := h.service.Publish(ctx, role(1), text("x", "k-"+testCase.label, testCase.channel))
		if !errors.Is(err, ErrChannelInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
		if got := Code(err); got != CodeChannelInvalid {
			t.Fatalf("%s: code = %d", testCase.label, got)
		}
	}
	if _, err := h.service.Conversation(ctx, role(1), 0, 0, 10); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("a conversation with peer 0 was accepted: %v", err)
	}
}

// --- defect 3: cursors and retention ---

func TestHistoryIsCursorBasedAndBounded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for i := 1; i <= 10; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), world()))
	}

	// No cursor: the newest page, not the oldest.
	newest, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(newest), []string{"m8", "m9", "m10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newest page = %v, want %v", got, want)
	}
	if !newest.HasMore || newest.NextCursor != 10 || newest.PrevCursor != 8 {
		t.Fatalf("newest page cursors = %+v", newest)
	}

	// Forward from a cursor returns only what the client has not seen — the
	// reconnect case the implementation this replaces could not express.
	forward, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 4, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(forward), []string{"m5", "m6", "m7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("forward page = %v, want %v", got, want)
	}
	if !forward.HasMore {
		t.Fatal("forward page must report more")
	}
	// Paging to the end, then again: an up-to-date client is not sent the same
	// messages a second time and does not lose its place.
	tail, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: forward.NextCursor, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(tail), []string{"m8", "m9", "m10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tail page = %v, want %v", got, want)
	}
	empty, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: tail.NextCursor, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Messages) != 0 {
		t.Fatalf("an up-to-date client was re-sent %d messages", len(empty.Messages))
	}
	if empty.NextCursor != tail.NextCursor || empty.HasMore {
		t.Fatalf("an empty page moved the cursor: %+v", empty)
	}

	// Scrollback reaches older messages, which was impossible before.
	back, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), BeforeSeq: newest.PrevCursor, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(back), []string{"m5", "m6", "m7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scrollback page = %v, want %v", got, want)
	}
	if !back.HasMore {
		t.Fatal("scrollback must report more")
	}

	// Bounds and cursor validity are errors, never silent clamping.
	for _, testCase := range []struct {
		label string
		query HistoryQuery
		want  error
	}{
		{"zero limit", HistoryQuery{Channel: world()}, ErrRangeInvalid},
		{"negative limit", HistoryQuery{Channel: world(), Limit: -1}, ErrRangeInvalid},
		{"limit above the cap", HistoryQuery{Channel: world(), Limit: MaxPageSize + 1}, ErrRangeInvalid},
		{"both cursors", HistoryQuery{Channel: world(), AfterSeq: 1, BeforeSeq: 5, Limit: 5}, ErrCursorInvalid},
		{"cursor beyond the channel", HistoryQuery{Channel: world(), AfterSeq: 999, Limit: 5}, ErrCursorInvalid},
	} {
		_, err := h.service.History(ctx, role(1), testCase.query)
		if !errors.Is(err, testCase.want) {
			t.Fatalf("%s: err = %v, want %v", testCase.label, err, testCase.want)
		}
		if code := Code(err); code == errcode.CodeInternal {
			t.Fatalf("%s: a client mistake returned the internal code", testCase.label)
		}
	}
}

// Retention is a bounded ring, enforced on every append, and a client that
// fell outside it is told so instead of silently losing messages.
func TestRetentionEvictsOldestAndReportsAGap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, withRules(ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: 4}))
	for i := 1; i <= 10; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), world()))
	}
	ref, err := h.store.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := h.service.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retained != 4 {
		t.Fatalf("retained %d messages, want 4", stats.Retained)
	}
	if stats.Evicted != 6 {
		t.Fatalf("evicted counter = %d, want 6", stats.Evicted)
	}
	if stats.OldestSeq != 7 || stats.LatestSeq != 10 {
		t.Fatalf("stats bracket = [%d,%d], want [7,10]", stats.OldestSeq, stats.LatestSeq)
	}
	if evicted := h.metrics.snapshot().evicted; evicted != 6 {
		t.Fatalf("the drop metric counted %d, want 6", evicted)
	}

	// A cursor older than the ring is a reported gap, not silence.
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Gap {
		t.Fatal("a cursor predating retention must report a gap")
	}
	if got, want := bodies(page), []string{"m7", "m8", "m9", "m10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page after the gap = %v, want %v", got, want)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 1 {
		t.Fatalf("the gap metric counted %d, want 1", gaps)
	}
	// A cursor inside the ring is not a gap.
	page, err = h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 8, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Gap {
		t.Fatal("a cursor inside the ring reported a gap")
	}
	// The sequence never rewinds, even though the messages it named are gone.
	if page.LatestSeq != 10 {
		t.Fatalf("latest seq = %d after eviction, want 10", page.LatestSeq)
	}
}

func TestPruneDropsMessagesPastTheRetentionAge(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, withRetentionAge(time.Hour))
	for i := 1; i <= 3; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("old%d", i), fmt.Sprintf("k%d", i), world()))
	}
	h.clock.advance(2 * time.Hour)
	mustPublish(t, h, role(1), text("fresh", "k4", world()))

	ref, err := h.store.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Bounded: a non-positive limit is an error, never "prune everything".
	for _, limit := range []int{0, -1, MaxRetain + 1} {
		if _, err := h.service.Prune(ctx, ref, limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("prune limit %d: %v", limit, err)
		}
	}
	// The limit is respected, so a sweep can work in batches.
	pruned, err := h.service.Prune(ctx, ref, 2)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned %d, want 2", pruned)
	}
	pruned, err = h.service.Prune(ctx, ref, 10)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d on the second pass, want 1", pruned)
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(page), []string{"fresh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after pruning, history = %v, want %v", got, want)
	}
	// Pruning again is a no-op: nothing left is old enough.
	if pruned, err := h.service.Prune(ctx, ref, 10); err != nil || pruned != 0 {
		t.Fatalf("pruned %d again (err %v), want 0", pruned, err)
	}
	// An unresolved ref cannot be pruned: a stream key is not something a
	// caller can name.
	if _, err := h.service.Prune(ctx, ChannelRef{}, 10); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("an empty ref was pruned: %v", err)
	}
}

// --- defect 4: idempotency, and the sequence tied to the insert ---

func TestPublishIsIdempotentPerRequestKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	first := mustPublish(t, h, role(1), text("hello", "delivery-1", world()))
	// max_deliver 5 on an at-least-once transport: five deliveries, one
	// message.
	for attempt := 0; attempt < 4; attempt++ {
		again := mustPublish(t, h, role(1), text("hello", "delivery-1", world()))
		if again.Seq != first.Seq {
			t.Fatalf("delivery %d stored sequence %d, first was %d", attempt+2, again.Seq, first.Seq)
		}
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("%d copies were stored for one idempotency key", len(page.Messages))
	}
	if snapshot := h.metrics.snapshot(); snapshot.stored != 1 || snapshot.replayed != 4 {
		t.Fatalf("metrics: stored %d, replayed %d; want 1 and 4", snapshot.stored, snapshot.replayed)
	}
	// A different key is a different message even with identical content.
	second := mustPublish(t, h, role(1), text("hello", "delivery-2", world()))
	if second.Seq == first.Seq {
		t.Fatal("two keys shared one sequence")
	}
}

func TestPublishRequiresAnIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, testCase := range []struct{ label, key string }{
		{"empty", ""},
		{"blank", "   "},
		{"oversized", strings.Repeat("k", MaxRequestIDBytes+1)},
	} {
		_, err := h.service.Publish(ctx, role(1), text("hello", testCase.key, world()))
		if !errors.Is(err, ErrRequestInvalid) {
			t.Fatalf("%s key: %v", testCase.label, err)
		}
		if got := Code(err); got != CodeRequestInvalid {
			t.Fatalf("%s key: code = %d", testCase.label, got)
		}
	}
}

// A replay whose message has fallen out of the retained window is refused
// rather than stored a second time. The window is finite and this is what its
// edge does.
func TestReplayOutsideTheRetainedWindowIsRefusedNotDuplicated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, withRules(ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: 3}))
	for i := 1; i <= 6; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), world()))
	}
	_, err := h.service.Publish(ctx, role(1), text("m1", "k1", world()))
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("a replay of an evicted message returned %v, want ErrAlreadyPublished", err)
	}
	if got := Code(err); got != CodeAlreadyPublished {
		t.Fatalf("code = %d, want %d", got, CodeAlreadyPublished)
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(page), []string{"m4", "m5", "m6"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the refused replay changed history to %v, want %v", got, want)
	}
}

// An idempotency key belongs to the sender that used it. Keys are scoped per
// channel, and clients mint them independently, so two players on one channel
// can collide on a key by accident — a per-client counter, a short random
// string. Before this, the second publish was answered with the FIRST sender's
// message and reported success: the second player's message was dropped, and
// nothing said so. A replay is only a replay when it comes from the same
// sender; anyone else reusing the key is refused.
func TestAnIdempotencyKeyIsBoundToItsSender(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	first := mustPublish(t, h, role(1), text("hello from 1", "shared-key", world()))

	_, err := h.service.Publish(ctx, role(2), text("hello from 2", "shared-key", world()))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("another sender reusing the key got %v, want ErrConflict; a nil error here means "+
			"role 2 was told its message was published when role 1's was returned instead", err)
	}
	if got := Code(err); got != CodeConflict {
		t.Fatalf("code = %d, want %d", got, CodeConflict)
	}
	// The original sender's replay is still a replay.
	again := mustPublish(t, h, role(1), text("hello from 1", "shared-key", world()))
	if again.Seq != first.Seq {
		t.Fatalf("the owner's replay stored a second copy (seq %d vs %d)", again.Seq, first.Seq)
	}
	// Nothing was stored for the intruding sender.
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(page); !reflect.DeepEqual(got, []string{"hello from 1"}) {
		t.Fatalf("history = %v", got)
	}
	// A role cannot claim a system message's key either, nor the reverse: the
	// game and a player are different senders even on the same channel.
	sys, err := h.service.PublishSystem(ctx, SystemPublishRequest{
		Channel: world(), Actor: "gm", Type: typeNotice, Body: []byte("notice"), RequestID: "sys-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Publish(ctx, role(1), text("mine", "sys-key", world())); !errors.Is(err, ErrConflict) {
		t.Fatalf("a role reused a system message's key: %v", err)
	}
	if _, err := h.service.PublishSystem(ctx, SystemPublishRequest{
		Channel: world(), Actor: "gm", Type: typeNotice, Body: []byte("x"), RequestID: "shared-key",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the game reused a role's key: %v", err)
	}
	_ = sys
	if got := h.metrics.Count("refused:" + publishOp(ChannelWorld) + ":key_reused"); got != 3 {
		t.Fatalf("key reuse reported %d refusals, want 3; %s", got, h.metrics.Events())
	}
}

// --- defect 5: ordering is the sequence, not the clock ---

// Ordering must come from the per-channel sequence. Publishing with a clock
// that runs backwards — the shape of clock skew between two replicas — must not
// reorder anything.
func TestOrderingComesFromTheSequenceNotTheClock(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const count = 8
	for i := 1; i <= count; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), world()))
		// Every publish is stamped earlier than the one before it.
		h.clock.advance(-time.Hour)
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: count})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != count {
		t.Fatalf("history holds %d messages, want %d", len(page.Messages), count)
	}
	for index, message := range page.Messages {
		if message.Seq != uint64(index+1) {
			t.Fatalf("position %d holds sequence %d: history must be in sequence order", index, message.Seq)
		}
		if got, want := string(message.Body), fmt.Sprintf("m%d", index+1); got != want {
			t.Fatalf("position %d holds %q, want %q", index, got, want)
		}
		if index > 0 && message.StoredAtUnix >= page.Messages[index-1].StoredAtUnix {
			t.Fatalf("position %d is not older than its predecessor; the test needs a backwards clock to be meaningful", index)
		}
	}
	// Stated as the property: sorting by the stored timestamp would produce the
	// exact opposite order, so a read path that sorted on it would fail here.
	if page.Messages[0].StoredAtUnix <= page.Messages[count-1].StoredAtUnix {
		t.Fatal("the clock did not run backwards, so this test proves nothing")
	}
}

// --- defect 6: the seam cannot lose the checks ---

func TestTheSeamEvaluatesThePolicyOnEveryPublishAndRead(t *testing.T) {
	ctx := context.Background()
	policy := &recordingPolicy{}
	h := newHarness(t, withPolicy(policy))

	mustPublish(t, h, role(1), text("hello", "k1", world()))
	if _, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if published, read := policy.counts(); published != 1 || read != 1 {
		t.Fatalf("policy consulted %d times for publish and %d for read, want 1 and 1", published, read)
	}
	// A replay still consults the policy: a muted player must not be able to
	// publish by replaying an old key.
	mustPublish(t, h, role(1), text("hello", "k1", world()))
	if published, _ := policy.counts(); published != 2 {
		t.Fatalf("a replay skipped the publish policy (%d calls)", published)
	}

	// A refusal carries the game's own reason and stores nothing.
	muted := errors.New("player is muted")
	policy.mu.Lock()
	policy.publishErr = muted
	policy.mu.Unlock()
	_, err := h.service.Publish(ctx, role(1), text("hello", "k2", world()))
	if !errors.Is(err, ErrNotPermitted) || !errors.Is(err, muted) {
		t.Fatalf("denial = %v, want both ErrNotPermitted and the policy's reason", err)
	}
	if got := Code(err); got != CodeNotPermitted {
		t.Fatalf("code = %d, want %d", got, CodeNotPermitted)
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("a refused publish stored a message: history holds %d", len(page.Messages))
	}

	// A read refusal returns no messages at all.
	policy.mu.Lock()
	policy.readErr = errors.New("not a member")
	policy.mu.Unlock()
	if _, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 5}); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("history ignored a read refusal: %v", err)
	}
	if _, err := h.service.Conversation(ctx, role(1), 2, 0, 5); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a conversation ignored a read refusal: %v", err)
	}
}

// An unset half of a policy denies. A missing decision must not read as
// permission.
func TestPolicyFuncsDenyWhenAHalfIsMissing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, withPolicy(PolicyFuncs{}))
	if _, err := h.service.Publish(ctx, role(1), text("hello", "k1", world())); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a nil publish policy permitted a publish: %v", err)
	}
	if _, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 5}); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a nil read policy permitted a read: %v", err)
	}
	// The half that is set is used.
	half := PolicyFuncs{Publish: func(context.Context, Sender, Channel) error { return nil }}
	permissive := newHarness(t, withPolicy(half))
	if _, err := permissive.service.Publish(ctx, role(1), text("hello", "k1", world())); err != nil {
		t.Fatalf("the configured publish half was not used: %v", err)
	}
}

// --- defect 7: bodies are bounded ---

func TestBodyBoundsAreEnforced(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, testCase := range []struct {
		label string
		req   PublishRequest
	}{
		{"empty body", PublishRequest{Channel: world(), Type: typeText, RequestID: "k1"}},
		{"over the type's own limit", PublishRequest{Channel: world(), Type: typeTinyBody, Body: []byte("123456789"), RequestID: "k2"}},
		{"over the package ceiling", PublishRequest{Channel: world(), Type: typeText, Body: make([]byte, MaxBodyBytes+1), RequestID: "k3"}},
		{"too many runes", PublishRequest{Channel: world(), Type: typeText, Body: []byte(strings.Repeat("é", 201)), RequestID: "k4"}},
		{"invalid utf-8", PublishRequest{Channel: world(), Type: typeText, Body: []byte{0xff, 0xfe}, RequestID: "k5"}},
		{"control characters", PublishRequest{Channel: world(), Type: typeText, Body: []byte("bell\x07"), RequestID: "k6"}},
	} {
		_, err := h.service.Publish(ctx, role(1), testCase.req)
		if !errors.Is(err, ErrMessageInvalid) {
			t.Fatalf("%s: err = %v, want ErrMessageInvalid", testCase.label, err)
		}
		if got := Code(err); got != CodeMessageInvalid {
			t.Fatalf("%s: code = %d, want %d", testCase.label, got, CodeMessageInvalid)
		}
	}
	// A type that opts into an empty body gets one; a type at its exact bound
	// is accepted.
	if _, err := h.service.Publish(ctx, role(1), PublishRequest{Channel: world(), Type: typeSignal, RequestID: "ok1"}); err != nil {
		t.Fatalf("an empty body was refused for a type that allows it: %v", err)
	}
	if _, err := h.service.Publish(ctx, role(1), PublishRequest{Channel: world(), Type: typeTinyBody, Body: []byte("12345678"), RequestID: "ok2"}); err != nil {
		t.Fatalf("a body at the type's exact bound was refused: %v", err)
	}
	// The registry refuses a bound that is not a bound, and a type with no
	// validator.
	registry := NewBodyRegistry()
	if err := registry.Register("x", BodySpec{Validator: TextValidator(10), MaxBytes: MaxBodyBytes + 1}); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("a max above the ceiling was registered: %v", err)
	}
	if err := registry.Register("x", BodySpec{}); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("a type with no validator was registered: %v", err)
	}
	// A stored body cannot be mutated through the caller's slice, or through a
	// page handed back.
	body := []byte("mutable")
	stored := mustPublish(t, h, role(1), PublishRequest{Channel: world(), Type: typeText, Body: body, RequestID: "ok3"})
	body[0] = 'X'
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: stored.Seq - 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(page.Messages[0].Body); got != "mutable" {
		t.Fatalf("the stored body aliases the caller's slice: %q", got)
	}
	page.Messages[0].Body[0] = 'Y'
	again, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: stored.Seq - 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(again.Messages[0].Body); got != "mutable" {
		t.Fatalf("a returned page aliases stored state: %q", got)
	}
}

func TestSenderIsValidatedAndBounded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, testCase := range []struct {
		label  string
		sender Sender
	}{
		{"zero role", Sender{}},
		{"negative role", Sender{RoleID: -1}},
		{"oversized name", Sender{RoleID: 1, Name: strings.Repeat("n", MaxSenderNameBytes+1)}},
	} {
		_, err := h.service.Publish(ctx, testCase.sender, text("hello", "k-"+testCase.label, world()))
		if !errors.Is(err, ErrSenderInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
		if _, err := h.service.History(ctx, testCase.sender, HistoryQuery{Channel: world(), Limit: 5}); !errors.Is(err, ErrSenderInvalid) {
			t.Fatalf("%s reading: %v", testCase.label, err)
		}
	}
}

// --- defect 8: no gameplay in the schema ---

func TestUnregisteredMessageTypesAreRefused(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	_, err := h.service.Publish(ctx, role(1), PublishRequest{
		Channel: world(), Type: typeUnknown, Body: []byte(`{"x":1,"y":2}`), RequestID: "k1",
	})
	if !errors.Is(err, ErrTypeUnknown) {
		t.Fatalf("an unregistered gameplay type was accepted: %v", err)
	}
	if got := Code(err); got != CodeTypeUnknown {
		t.Fatalf("code = %d, want %d", got, CodeTypeUnknown)
	}
	if _, err := h.service.PublishSystem(ctx, SystemPublishRequest{
		Channel: Channel{Kind: ChannelSystem}, Actor: "gm", Type: typeUnknown, Body: []byte("x"), RequestID: "k2",
	}); !errors.Is(err, ErrTypeUnknown) {
		t.Fatalf("the privileged path accepted an unregistered type: %v", err)
	}

	// The package ships no types at all: with an empty registry nothing can be
	// published, which is what "the service knows nothing about gameplay"
	// means concretely.
	bare, err := NewStore(versionstore.NewMemoryStore[string, channelState](), Config{
		Policy: allowAllPolicy{}, Bodies: NewBodyRegistry(), Now: newClock().Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, messageType := range []MessageType{typeText, typeNotice, "chat", "message", ""} {
		if _, err := bare.Append(ctx, role(1), PublishRequest{
			Channel: world(), Type: messageType, Body: []byte("x"), RequestID: "k",
		}); !errors.Is(err, ErrTypeUnknown) {
			t.Fatalf("type %q was known to an empty registry: %v", messageType, err)
		}
	}

	// A body is opaque: the validator the game registered is what decides, and
	// this package never looks inside.
	registry := NewBodyRegistry()
	seen := []byte(nil)
	registry.MustRegister("gameplay.opaque", BodySpec{Validator: BodyValidatorFunc(func(body []byte) error {
		seen = append([]byte(nil), body...)
		return nil
	})})
	game := newHarnessWithRegistry(t, registry)
	if _, err := game.service.Publish(ctx, role(1), PublishRequest{
		Channel: world(), Type: "gameplay.opaque", Body: []byte{0x00, 0x01, 0x02}, RequestID: "k3",
	}); err != nil {
		t.Fatalf("opaque bytes were refused: %v", err)
	}
	if !reflect.DeepEqual(seen, []byte{0x00, 0x01, 0x02}) {
		t.Fatalf("the validator saw %v, want the exact bytes", seen)
	}
	// Re-registering a type is refused rather than silently overwritten.
	if err := registry.Register("gameplay.opaque", BodySpec{Validator: TextValidator(5)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a redefinition was accepted: %v", err)
	}
}

func newHarnessWithRegistry(t *testing.T, registry *BodyRegistry) *harness {
	t.Helper()
	h := &harness{clock: newClock(), metrics: newRecorder(), registry: registry}
	state := versionstore.NewMemoryStore[string, channelState]()
	store, err := NewStore(state, Config{Policy: allowAllPolicy{}, Bodies: registry, Now: h.clock.Now, Metrics: h.metrics})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceConfig{Store: store, System: grantingAuth()})
	if err != nil {
		t.Fatal(err)
	}
	h.store, h.service, h.state = store, service, state
	return h
}

// Channel behaviour is data: a game adds a kind by registering a rule, and
// nothing in this package switches on the built-in kinds.
func TestChannelRulesAreData(t *testing.T) {
	ctx := context.Background()
	tradeShared := ChannelRule{Kind: "trade", RequiresTarget: true, Scope: ScopeShared, Retain: 2}
	whisperPair := ChannelRule{Kind: "whisper", RequiresTarget: true, Scope: ScopePair, Retain: 10}
	h := newHarness(t, withRules(tradeShared, whisperPair))

	mustPublish(t, h, role(1), text("wts", "k1", Channel{Kind: "trade", Target: 3}))
	page, err := h.service.History(ctx, role(2), HistoryQuery{Channel: Channel{Kind: "trade", Target: 3}, Limit: 5})
	if err != nil {
		t.Fatalf("a registered custom kind was refused: %v", err)
	}
	if got := bodies(page); !reflect.DeepEqual(got, []string{"wts"}) {
		t.Fatalf("custom shared kind history = %v", got)
	}
	// The custom kind's retention is its own row, not a global constant.
	for i := 2; i <= 5; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), Channel{Kind: "trade", Target: 3}))
	}
	ref, err := h.store.Resolve(Channel{Kind: "trade", Target: 3}, 1)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := h.service.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retained != 2 {
		t.Fatalf("the custom kind retained %d messages, want its own rule's 2", stats.Retained)
	}
	// A custom pair-scoped kind gets the symmetric stream, with no code in
	// this package naming it.
	mustPublish(t, h, role(1), text("psst", "w1", Channel{Kind: "whisper", Target: 2}))
	mine, err := h.service.History(ctx, role(2), HistoryQuery{Channel: Channel{Kind: "whisper", Target: 1}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(mine); !reflect.DeepEqual(got, []string{"psst"}) {
		t.Fatalf("custom pair kind history = %v", got)
	}
	// An override replaces a built-in row by kind, rather than adding a second.
	overridden := newHarness(t, withRules(ChannelRule{Kind: ChannelWorld, RequiresTarget: false, Scope: ScopeShared, Retain: 5}))
	if _, err := overridden.service.Publish(ctx, role(1), text("global", "k1", Channel{Kind: ChannelWorld})); err != nil {
		t.Fatalf("the override did not take effect: %v", err)
	}
	// An unregistered kind is a client error with its own code.
	_, err = h.service.Publish(ctx, role(1), text("x", "k9", Channel{Kind: "nope", Target: 1}))
	if !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("an unregistered kind was accepted: %v", err)
	}
	if got := Code(err); got != CodeChannelInvalid {
		t.Fatalf("code = %d, want %d", got, CodeChannelInvalid)
	}
	// A world channel still requires its target under the default rules.
	plain := newHarness(t)
	if _, err := plain.service.Publish(ctx, role(1), text("x", "k1", Channel{Kind: ChannelWorld})); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("a world channel with no target was accepted: %v", err)
	}
	// A pair-scoped kind has no privileged publisher: the game is not a role.
	if _, err := h.service.PublishSystem(ctx, SystemPublishRequest{
		Channel: Channel{Kind: "whisper", Target: 2}, Actor: "gm", Type: typeNotice, Body: []byte("x"), RequestID: "s1",
	}); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("a privileged publish reached a pair-scoped kind: %v", err)
	}
}

// --- concurrency ---

// Concurrent publishes to one channel must lose nothing and must never reuse a
// sequence. The sequence is allocated inside the same compare-and-set as the
// append, so this is a property of the write, not of a separate counter
// document.
func TestConcurrentPublishesLoseNothingAndDoNotReuseASequence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const publishers = 32

	var wait sync.WaitGroup
	var mu sync.Mutex
	seqs := map[uint64]string{}
	for i := 0; i < publishers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			key := fmt.Sprintf("k%d", i)
			message, err := h.service.Publish(ctx, role(int64(i%4)+1), text(key, key, world()))
			if err != nil {
				t.Errorf("publish %s: %v", key, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if other, clash := seqs[message.Seq]; clash {
				t.Errorf("sequence %d was issued to both %s and %s", message.Seq, other, key)
			}
			seqs[message.Seq] = key
		}(i)
	}
	wait.Wait()

	if len(seqs) != publishers {
		t.Fatalf("%d distinct sequences for %d publishes", len(seqs), publishers)
	}
	for seq := uint64(1); seq <= publishers; seq++ {
		if _, ok := seqs[seq]; !ok {
			t.Fatalf("sequence %d was never issued: the allocation has a hole", seq)
		}
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != publishers {
		t.Fatalf("history holds %d of %d messages: a publish was lost", len(page.Messages), publishers)
	}
	for index, message := range page.Messages {
		if message.Seq != uint64(index+1) {
			t.Fatalf("history position %d holds sequence %d", index, message.Seq)
		}
	}
	if stored := h.metrics.snapshot().stored; stored != publishers {
		t.Fatalf("the stored metric counted %d, want %d", stored, publishers)
	}
}

// Concurrent replays of one idempotency key must store exactly one message —
// the redelivery case the implementation this replaces answered by storing a
// copy per delivery.
func TestConcurrentReplaysOfOneKeyStoreOneMessage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const racers = 16

	var wait sync.WaitGroup
	var mu sync.Mutex
	seen := map[uint64]int{}
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			message, err := h.service.Publish(ctx, role(1), text("hello", "one-key", world()))
			if err != nil {
				t.Errorf("publish: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			seen[message.Seq]++
		}()
	}
	wait.Wait()

	if len(seen) != 1 {
		t.Fatalf("one idempotency key produced %d distinct sequences: %v", len(seen), seen)
	}
	if seen[1] != racers {
		t.Fatalf("sequence 1 was returned %d times, want %d", seen[1], racers)
	}
	page, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("%d messages were stored for one key", len(page.Messages))
	}
	if snapshot := h.metrics.snapshot(); snapshot.stored != 1 || snapshot.replayed != racers-1 {
		t.Fatalf("metrics: stored %d, replayed %d; want 1 and %d", snapshot.stored, snapshot.replayed, racers-1)
	}
}

// Concurrent publishes into different private conversations must not leak
// across streams.
func TestConcurrentPrivateConversationsStayIsolated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const peers = 12

	var wait sync.WaitGroup
	for peer := int64(2); peer <= peers; peer++ {
		wait.Add(1)
		go func(peer int64) {
			defer wait.Done()
			key := fmt.Sprintf("k%d", peer)
			if _, err := h.service.Publish(ctx, role(1), text(key, key, private(peer))); err != nil {
				t.Errorf("publish to %d: %v", peer, err)
				return
			}
			if _, err := h.service.Publish(ctx, role(peer), text("reply-"+key, "r"+key, private(1))); err != nil {
				t.Errorf("reply from %d: %v", peer, err)
			}
		}(peer)
	}
	wait.Wait()

	for peer := int64(2); peer <= peers; peer++ {
		page, err := h.service.Conversation(ctx, role(1), peer, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{fmt.Sprintf("k%d", peer), fmt.Sprintf("reply-k%d", peer)}
		if got := bodies(page); !reflect.DeepEqual(got, want) {
			t.Fatalf("conversation with %d = %v, want %v", peer, got, want)
		}
		if page.Messages[0].Seq != 1 || page.Messages[1].Seq != 2 {
			t.Fatalf("conversation with %d has sequences %d,%d; each stream numbers itself",
				peer, page.Messages[0].Seq, page.Messages[1].Seq)
		}
	}
}

// --- error codes and configuration ---

// Every business failure has its own code, and no client mistake gets the
// internal one.
func TestCodeMapsEveryBusinessErrorToItsOwnCode(t *testing.T) {
	cases := []struct {
		err  error
		want int32
	}{
		{nil, CodeOK},
		{ErrChannelInvalid, CodeChannelInvalid},
		{ErrSenderInvalid, CodeSenderInvalid},
		{ErrMessageInvalid, CodeMessageInvalid},
		{ErrTypeUnknown, CodeTypeUnknown},
		{ErrNotPermitted, CodeNotPermitted},
		{ErrRequestInvalid, CodeRequestInvalid},
		{ErrCursorInvalid, CodeCursorInvalid},
		{ErrRangeInvalid, CodeRangeInvalid},
		{ErrSystemDenied, CodeSystemDenied},
		{ErrAlreadyPublished, CodeAlreadyPublished},
		{ErrConflict, CodeConflict},
		{versionstore.ErrConflict, CodeConflict},
	}
	seen := map[int32]error{}
	for _, testCase := range cases {
		got := Code(testCase.err)
		if got != testCase.want {
			t.Fatalf("Code(%v) = %d, want %d", testCase.err, got, testCase.want)
		}
		if testCase.err == nil {
			continue
		}
		if got == errcode.CodeInternal {
			t.Fatalf("business error %v maps to the internal code", testCase.err)
		}
		if got < 580101 || got > 580199 {
			t.Fatalf("Code(%v) = %d, outside this package's 580101+ block", testCase.err, got)
		}
		if other, clash := seen[got]; clash && !errors.Is(testCase.err, other) && !errors.Is(other, testCase.err) {
			if got != CodeConflict {
				t.Fatalf("code %d is shared by %v and %v", got, other, testCase.err)
			}
		}
		seen[got] = testCase.err
	}
	// A wrapped error still maps, and an unclassified one is the only thing
	// that reaches the internal code.
	if got := Code(fmt.Errorf("publish: %w", ErrCursorInvalid)); got != CodeCursorInvalid {
		t.Fatalf("a wrapped error mapped to %d", got)
	}
	if got := Code(errors.New("redis: connection refused")); got != errcode.CodeInternal {
		t.Fatalf("an unclassified error mapped to %d, want CodeInternal (%d)", got, errcode.CodeInternal)
	}
	// And a foreign sentinel this package maps deliberately does NOT fall
	// through to the internal code: compare-and-set exhaustion is a real,
	// retryable outcome a caller can act on.
	if got := Code(fmt.Errorf("append: %w", versionstore.ErrConflict)); got != CodeConflict {
		t.Fatalf("versionstore contention mapped to %d, want %d", got, CodeConflict)
	}
	// A denial that wraps another sentinel is still reported as a denial.
	if got := Code(denied(ErrChannelInvalid)); got != CodeNotPermitted {
		t.Fatalf("a denial wrapping a channel error mapped to %d", got)
	}
}

func TestBackendFailuresAreReportedAsStoreFailuresAndConflictsAreCounted(t *testing.T) {
	ctx := context.Background()
	inner := versionstore.NewMemoryStore[string, channelState]()
	failure := errors.New("redis: connection refused")
	h := newHarness(t, withState(brokenState{inner: inner, updateErr: failure, getErr: failure}))

	_, err := h.service.Publish(ctx, role(1), text("hello", "k1", world()))
	if !errors.Is(err, failure) {
		t.Fatalf("publish error = %v, want the backend failure", err)
	}
	if got := Code(err); got != errcode.CodeInternal {
		t.Fatalf("a backend failure mapped to %d, want CodeInternal (%d)", got, errcode.CodeInternal)
	}
	if _, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 5}); !errors.Is(err, failure) {
		t.Fatalf("history error = %v, want the backend failure", err)
	}

	// Compare-and-set exhaustion is distinguishable from a dead backend, and it
	// is counted: one channel is one entry, so contention is the expected
	// failure mode under load.
	contended := newHarness(t, withState(brokenState{inner: inner, updateErr: versionstore.ErrConflict}))
	_, err = contended.service.Publish(ctx, role(1), text("hello", "k1", world()))
	if !errors.Is(err, versionstore.ErrConflict) {
		t.Fatalf("publish error = %v, want a conflict", err)
	}
	if got := Code(err); got != CodeConflict {
		t.Fatalf("a conflict mapped to %d, want %d", got, CodeConflict)
	}
	if got := contended.metrics.snapshot().conflicts["append"]; got != 1 {
		t.Fatalf("the conflict metric counted %d for append, want 1", got)
	}
	ref, err := contended.store.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contended.service.Prune(ctx, ref, 10); !errors.Is(err, versionstore.ErrConflict) {
		t.Fatalf("prune error = %v, want a conflict", err)
	}
	if got := contended.metrics.snapshot().conflicts["prune"]; got != 1 {
		t.Fatalf("the conflict metric counted %d for prune, want 1", got)
	}
}

func TestNewStoreRejectsAnIncompleteOrUnsafeConfig(t *testing.T) {
	state := versionstore.NewMemoryStore[string, channelState]()
	registry := NewBodyRegistry()
	for _, testCase := range []struct {
		label string
		state StateStore
		cfg   Config
	}{
		{"no state store", nil, Config{Policy: allowAllPolicy{}, Bodies: registry}},
		{"no policy", state, Config{Bodies: registry}},
		{"no registry", state, Config{Policy: allowAllPolicy{}}},
		{"negative retention age", state, Config{Policy: allowAllPolicy{}, Bodies: registry, RetentionAge: -time.Second}},
		{"rule with no kind", state, Config{Policy: allowAllPolicy{}, Bodies: registry,
			Rules: []ChannelRule{{Scope: ScopeShared}}}},
		{"rule with an unknown scope", state, Config{Policy: allowAllPolicy{}, Bodies: registry,
			Rules: []ChannelRule{{Kind: "x", Scope: "sideways"}}}},
		{"pair rule that does not require a target", state, Config{Policy: allowAllPolicy{}, Bodies: registry,
			Rules: []ChannelRule{{Kind: "x", Scope: ScopePair}}}},
		{"retain above the cap", state, Config{Policy: allowAllPolicy{}, Bodies: registry,
			Rules: []ChannelRule{{Kind: "x", Scope: ScopeShared, Retain: MaxRetain + 1}}}},
		{"negative retain", state, Config{Policy: allowAllPolicy{}, Bodies: registry,
			Rules: []ChannelRule{{Kind: "x", Scope: ScopeShared, Retain: -1}}}},
	} {
		if _, err := NewStore(testCase.state, testCase.cfg); err == nil {
			t.Fatalf("%s was accepted", testCase.label)
		}
	}
	// A complete config works, and the defaults are the documented ones.
	store, err := NewStore(state, Config{Policy: allowAllPolicy{}, Bodies: registry})
	if err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Fatal("a service with no store was accepted")
	}
	service, err := NewService(ServiceConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if service.cfg.ConversationKind != ChannelPrivate {
		t.Fatalf("conversation kind defaulted to %q", service.cfg.ConversationKind)
	}
	if _, err := NewRedisStateStore(nil, "chat"); err == nil {
		t.Fatal("a redis state store with no client was accepted")
	}
}

func TestStatsAndDefaultRules(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ref, err := h.store.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// An untouched channel has no stats and is not an error.
	stats, err := h.service.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("an untouched channel reported %+v", stats)
	}
	if _, err := h.service.Stats(ctx, ChannelRef{}); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("an empty ref was accepted by Stats: %v", err)
	}
	mustPublish(t, h, role(1), text("hello", "k1", world()))
	mustPublish(t, h, role(1), text("hello", "k1", world()))
	stats, err = h.service.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retained != 1 || stats.LatestSeq != 1 || stats.RequestKeys != 1 {
		t.Fatalf("stats after a publish and a replay = %+v", stats)
	}
	// Every default rule is usable and bounded.
	for _, rule := range DefaultChannelRules() {
		if rule.Retain <= 0 || rule.Retain > MaxRetain {
			t.Fatalf("default rule %q retains %d", rule.Kind, rule.Retain)
		}
		if err := rule.validate(); err != nil {
			t.Fatalf("default rule %q is invalid: %v", rule.Kind, err)
		}
	}
	// The registry lists what a deployment registered, and nothing else.
	if got := len(h.registry.Types()); got != 4 {
		t.Fatalf("the registry holds %d types, want the 4 the test registered", got)
	}
}

func TestRefKeysDistinguishKindAndTarget(t *testing.T) {
	h := newHarness(t)
	keys := map[string]string{}
	for _, testCase := range []struct {
		label       string
		channel     Channel
		participant int64
	}{
		{"world 7", world(), 1},
		{"world 8", Channel{Kind: ChannelWorld, Target: 8}, 1},
		{"group 7", Channel{Kind: ChannelGroup, Target: 7}, 1},
		{"private 1-2", private(2), 1},
		{"private 1-3", private(3), 1},
		{"system", Channel{Kind: ChannelSystem}, 0},
	} {
		ref, err := h.store.Resolve(testCase.channel, testCase.participant)
		if err != nil {
			t.Fatalf("%s: %v", testCase.label, err)
		}
		if other, clash := keys[ref.Key()]; clash {
			t.Fatalf("%s and %s share the stream key %q", testCase.label, other, ref.Key())
		}
		keys[ref.Key()] = testCase.label
	}
}

// The shared reporting seam takes an operation name where chat's own
// interface took a typed ChannelKind. That is only an acceptable trade if the
// kind actually survives in the name, so it is asserted rather than assumed:
// an operator breaking down publishes by channel kind is the reason the
// parameter existed.
func TestReportedOperationNamesCarryTheChannelKind(t *testing.T) {
	h := newHarness(t)
	mustPublish(t, h, role(1), text("hi", "req-world", world()))
	mustPublish(t, h, role(1), text("hey", "req-private", private(2)))

	for _, kind := range []ChannelKind{ChannelWorld, ChannelPrivate} {
		op := publishOp(kind)
		if got := h.metrics.Count("accepted:" + op); got != 1 {
			t.Fatalf("publishes under %q reported %d, want 1; %s", op, got, h.metrics.Events())
		}
		if !strings.Contains(op, string(kind)) {
			t.Fatalf("operation name %q does not name the channel kind", op)
		}
	}
}

// A refusal must say why. The interface this replaces counted denials without
// a reason, which cannot distinguish "a client is probing a system-only
// channel" from "the policy is rejecting everyone" — two situations with
// nothing in common but the counter.
func TestARefusalReportsItsReason(t *testing.T) {
	h := newHarness(t, withRules(ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: 8, SystemOnly: true}))
	if _, err := h.service.Publish(context.Background(), role(1),
		text("hi", "req-1", world())); err == nil {
		t.Fatal("a role published into a system-only channel")
	}
	want := "refused:" + publishOp(ChannelWorld) + ":system_only_channel"
	if got := h.metrics.Count(want); got != 1 {
		t.Fatalf("refusal not reported under %q; %s", want, h.metrics.Events())
	}
}
