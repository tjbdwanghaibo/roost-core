package mail

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/bus"
	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// --- drift: the client, the handlers and the interface must agree ---

// Every method on Mail is in Methods, and every entry in Methods is a method
// on Mail.
//
// This is the drift guard, and it is the reason Methods exists as data. The
// client and the handlers are separate code: a method added to the interface
// and implemented on *Service and on *BusClient still compiles with no handler
// registered for it, and then fails only when another process calls it. Go
// makes the two implementations agree; nothing makes the TRANSPORT agree.
func TestMethodsCoversTheInterfaceExactly(t *testing.T) {
	iface := reflect.TypeOf((*Mail)(nil)).Elem()
	declared := map[string]bool{}
	for index := 0; index < iface.NumMethod(); index++ {
		declared["mail."+iface.Method(index).Name] = true
	}
	listed := map[string]bool{}
	for _, method := range Methods {
		if listed[method] {
			t.Fatalf("Methods lists %q twice", method)
		}
		listed[method] = true
	}
	for method := range declared {
		if !listed[method] {
			t.Fatalf("Mail declares %s but Methods does not list it; no handler will be "+
				"registered and a remote call will fail at run time", method)
		}
	}
	for method := range listed {
		if !declared[method] {
			t.Fatalf("Methods lists %s but Mail does not declare it; the name is dead and a "+
				"reader will look for a method that is not there", method)
		}
	}
	if len(declared) != len(listed) {
		t.Fatalf("Mail has %d methods, Methods lists %d", len(declared), len(listed))
	}
}

// RegisterHandlers publishes exactly the methods in Methods — no more, no
// fewer — and fails by NAME when one is missing.
func TestRegisterHandlersPublishesExactlyTheDeclaredMethods(t *testing.T) {
	h := newHarness(t)
	fake := newFakeBus()
	if err := RegisterHandlers(fake, h.service); err != nil {
		t.Fatal(err)
	}
	for _, method := range Methods {
		if _, ok := fake.handler(method); !ok {
			t.Fatalf("no handler was registered for %s", method)
		}
	}
	if got := fake.count(); got != len(Methods) {
		t.Fatalf("%d handlers were registered for %d methods", got, len(Methods))
	}
}

func TestRegisterHandlersRefusesANilBusOrService(t *testing.T) {
	h := newHarness(t)
	if err := RegisterHandlers(nil, h.service); err == nil {
		t.Fatal("RegisterHandlers accepted a nil bus")
	}
	if err := RegisterHandlers(newFakeBus(), nil); err == nil {
		t.Fatal("RegisterHandlers accepted a nil service")
	}
}

// --- the two implementations must behave the same ---

// The local service and the bus client are driven through the SAME assertions.
//
// This is what the interface is for: business logic written against Mail must
// behave identically whether mail runs beside it or in another process. A test
// that only exercised one of them would leave the other's error translation,
// wire encoding and status handling unverified — and those are exactly where a
// remote call diverges from a local one.
func TestBothImplementationsBehaveTheSame(t *testing.T) {
	for _, transport := range []string{"local", "bus"} {
		t.Run(transport, func(t *testing.T) {
			mail, clock := newTransport(t, transport)
			ctx := context.Background()

			// A send, then the mail is visible with an unread count.
			sent, err := mail.Send(ctx, SendRequest{
				Audience: AudienceDirect, Recipients: []int64{7},
				Subject: "reward", Attachment: []byte("100 gold"),
				ExpiresInSeconds: 3600, RequestID: "req-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if sent.ID == "" {
				t.Fatal("the send returned an envelope with no id")
			}
			page, err := mail.List(ctx, 7, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("the page holds %d items, want 1", len(page.Items))
			}
			if page.Unread != 1 {
				t.Fatalf("unread is %d, want 1", page.Unread)
			}
			// The attachment survives the round trip: it is bytes, and a codec
			// that mangled it would be a silently wrong reward.
			if got := string(page.Items[0].Envelope.Attachment); got != "100 gold" {
				t.Fatalf("the attachment came back as %q", got)
			}

			// A retried send is a replay, not a second mail.
			again, err := mail.Send(ctx, SendRequest{
				Audience: AudienceDirect, Recipients: []int64{7},
				Subject: "reward", Attachment: []byte("100 gold"),
				ExpiresInSeconds: 3600, RequestID: "req-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if again.ID != sent.ID {
				t.Fatalf("a retried send produced envelope %s, first was %s", again.ID, sent.ID)
			}

			// The claim's token is constant across reservations — the property
			// the whole claim design rests on, asserted through both transports.
			first, err := mail.ReserveClaim(ctx, 7, sent.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if string(first.Attachment) != "100 gold" {
				t.Fatalf("the claim carried %q", first.Attachment)
			}
			released, err := mail.CancelClaim(ctx, 7, sent.ID, first.Token)
			if err != nil {
				t.Fatal(err)
			}
			if !released {
				t.Fatal("the cancel reported that it released nothing")
			}
			second, err := mail.ReserveClaim(ctx, 7, sent.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if second.Token != first.Token {
				t.Fatalf("a re-reservation got token %q, want %q", second.Token, first.Token)
			}
			if _, err := mail.CommitClaim(ctx, 7, sent.ID, first.Token); err != nil {
				t.Fatal(err)
			}
			summary, err := mail.Summary(ctx, 7)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Unread != 0 {
				t.Fatalf("unread is %d after the claim, want 0", summary.Unread)
			}

			// A cancel that releases nothing reports false through both
			// transports, rather than collapsing into "no error".
			if released, err := mail.CancelClaim(ctx, 7, sent.ID, "not-the-token"); err != nil {
				t.Fatalf("a foreign cancel errored: %v", err)
			} else if released {
				t.Fatal("a foreign cancel reported that it released something")
			}
			_ = clock
		})
	}
}

// Error codes cross the bus intact. This is what the errcode work is for: a
// client mistake must arrive as a code the caller can match on, not as
// "server error".
func TestErrorCodesSurviveBothTransports(t *testing.T) {
	for _, transport := range []string{"local", "bus"} {
		t.Run(transport, func(t *testing.T) {
			mail, _ := newTransport(t, transport)
			ctx := context.Background()

			cases := []struct {
				name string
				run  func() error
				want int32
			}{
				{"keyless send", func() error {
					_, err := mail.Send(ctx, SendRequest{
						Audience: AudienceDirect, Recipients: []int64{7},
						Subject: "s", ExpiresInSeconds: 3600,
					})
					return err
				}, CodeRequestInvalid},
				{"negative page limit", func() error {
					_, err := mail.List(ctx, 7, "", -1)
					return err
				}, CodeRangeInvalid},
				{"claim a mail never delivered", func() error {
					_, err := mail.ReserveClaim(ctx, 7, "no-such-mail", "")
					return err
				}, CodeMailMissing},
				{"commit with no token", func() error {
					_, err := mail.CommitClaim(ctx, 7, "m1", "")
					return err
				}, CodeRequestInvalid},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					err := tc.run()
					if err == nil {
						t.Fatal("the call succeeded")
					}
					code := codeOfRemoteOrLocal(err)
					if code != tc.want {
						t.Fatalf("code %d, want %d; the error was %v", code, tc.want, err)
					}
					if code == errcode.CodeInternal {
						t.Fatal("a client mistake arrived as CodeInternal")
					}
				})
			}
		})
	}
}

// --- identity stays a parameter ---

// No wire struct may make the caller's identity look optional.
//
// The confirmed defect was not that a player id travelled on the wire —
// between server processes that is ordinary — but that it travelled as an
// optional-looking field whose zero value compiled fine, so a caller that
// forgot it addressed player 0. The interface keeps identity as a parameter so
// the compiler asks; this asserts the wire types cannot be constructed by a
// caller who skipped it, because every one of them is unexported-by-convention
// and only reachable through a method that takes the id.
func TestIdentityIsAParameterOnEveryMethodThatNeedsOne(t *testing.T) {
	iface := reflect.TypeOf((*Mail)(nil)).Elem()
	// Send addresses recipients inside its request; every other method acts on
	// behalf of one player and must take that player as an argument.
	perPlayer := map[string]bool{
		"List": true, "Summary": true, "MarkRead": true, "Delete": true,
		"ReserveClaim": true, "CommitClaim": true, "CancelClaim": true,
	}
	seen := 0
	for index := 0; index < iface.NumMethod(); index++ {
		method := iface.Method(index)
		if !perPlayer[method.Name] {
			continue
		}
		seen++
		// ctx is argument 0 on an interface method type; the player id must be
		// argument 1 and an int64.
		if method.Type.NumIn() < 2 {
			t.Fatalf("%s takes no identity argument", method.Name)
		}
		if got := method.Type.In(1); got.Kind() != reflect.Int64 {
			t.Fatalf("%s takes %s as its first argument after ctx, want int64: the caller's "+
				"identity must be a parameter the compiler asks for, not a struct field whose "+
				"zero value is a valid-looking player 0", method.Name, got)
		}
	}
	if seen != len(perPlayer) {
		t.Fatalf("checked %d per-player methods, expected %d; the interface changed", seen, len(perPlayer))
	}
}

// The handler takes the identity from the decoded request and passes it
// through unchanged — it does not default it, and it does not invent one.
//
// A handler that filled in a missing player id (from a header, from zero, from
// anywhere) would be a handler that makes the field optional again after the
// interface worked to make it required.
func TestAHandlerPassesTheIdentityThroughUnchanged(t *testing.T) {
	seen := make(chan int64, 1)
	spy := &spyMail{onSummary: func(playerID int64) { seen <- playerID }}
	fake := newFakeBus()
	if err := RegisterHandlers(fake, spy); err != nil {
		t.Fatal(err)
	}
	handler, ok := fake.handler(MethodSummary)
	if !ok {
		t.Fatal("no summary handler")
	}
	// A request that names player 9.
	resp, err := invoke(t, handler, MethodSummary, rpcSummaryRequest{PlayerID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != 9 {
		t.Fatalf("the handler called the service with player %d, want 9", got)
	}
	if status, ok := resp.(rpcSummaryResponse); !ok || status.Code != CodeOK {
		t.Fatalf("the handler answered %+v", resp)
	}

	// A request that names nobody must reach the service as nobody — so the
	// service's own check refuses it. The handler must not substitute a value.
	if _, err := invoke(t, handler, MethodSummary, rpcSummaryRequest{PlayerID: 0}); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != 0 {
		t.Fatalf("the handler substituted player %d for an absent id; the service's own "+
			"identity check is then bypassed", got)
	}
}

// --- the two Mods are mutually exclusive ---

// newTransport returns Mail either as the local service or as a bus client
// wired to that same service through the fake bus, so one set of assertions
// covers both.
func newTransport(t *testing.T, kind string) (Mail, *clock) {
	t.Helper()
	h := newHarness(t)
	switch kind {
	case "local":
		return h.service, h.clock
	case "bus":
		fake := newFakeBus()
		if err := RegisterHandlers(fake, h.service); err != nil {
			t.Fatal(err)
		}
		client, err := NewBusClient(fake, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return client, h.clock
	default:
		t.Fatalf("unknown transport %q", kind)
		return nil, nil
	}
}

// codeOfRemoteOrLocal reads the code off an error whether it came from the
// local service (a coded sentinel) or across the bus (a remote error built
// from the envelope status).
func codeOfRemoteOrLocal(err error) int32 {
	code, _ := Error(err)
	if code != errcode.CodeInternal {
		return code
	}
	return errcode.CodeOf(err)
}

// spyMail records what a handler passed through. Only the methods a test uses
// are implemented; the rest fail loudly rather than silently returning zero
// values.
type spyMail struct {
	onSummary func(int64)
}

func (s *spyMail) Send(context.Context, SendRequest) (Envelope, error) {
	return Envelope{}, errors.New("spyMail: Send not implemented")
}
func (s *spyMail) List(context.Context, int64, string, int) (Page, error) {
	return Page{}, errors.New("spyMail: List not implemented")
}
func (s *spyMail) Summary(_ context.Context, playerID int64) (Summary, error) {
	if s.onSummary != nil {
		s.onSummary(playerID)
	}
	return Summary{PlayerID: playerID}, nil
}
func (s *spyMail) MarkRead(context.Context, int64, string) (Entry, error) {
	return Entry{}, errors.New("spyMail: MarkRead not implemented")
}
func (s *spyMail) Delete(context.Context, int64, string) (Entry, error) {
	return Entry{}, errors.New("spyMail: Delete not implemented")
}
func (s *spyMail) ReserveClaim(context.Context, int64, string, string) (Claim, error) {
	return Claim{}, errors.New("spyMail: ReserveClaim not implemented")
}
func (s *spyMail) CommitClaim(context.Context, int64, string, string) (Entry, error) {
	return Entry{}, errors.New("spyMail: CommitClaim not implemented")
}
func (s *spyMail) CancelClaim(context.Context, int64, string, string) (bool, error) {
	return false, errors.New("spyMail: CancelClaim not implemented")
}

var _ Mail = (*spyMail)(nil)

func newFakeBus() *fakeBus { return &fakeBus{handlers: map[string]bus.RpcHandlerFunc{}} }

// fakeBus is an in-process bus that ROUTES: a Call is dispatched to the
// registered handler and its response is encoded and decoded exactly as the
// real bus would.
//
// It routes rather than short-circuits on purpose. A double that handed the
// response struct straight back would leave the wire encoding untested, and
// the encoding is where a remote call diverges from a local one — a field the
// codec drops looks identical to a field the service never set.
type fakeBus struct {
	mu       sync.Mutex
	handlers map[string]bus.RpcHandlerFunc
	calls    int
}

func (f *fakeBus) HandleRpc(method string, handler bus.RpcHandlerFunc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.handlers[method]; exists {
		return fmt.Errorf("fakeBus: %s already registered", method)
	}
	f.handlers[method] = handler
	return nil
}

func (f *fakeBus) handler(method string) (bus.RpcHandlerFunc, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	handler, ok := f.handlers[method]
	return handler, ok
}

func (f *fakeBus) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handlers)
}

// --- fakeBus: a routing in-process bus ---

// Call routes to the registered handler and puts the response through the
// codec, exactly as the real bus does.
//
// It ROUTES rather than short-circuits on purpose. A double that handed the
// response struct straight back would leave the wire encoding untested — and
// the encoding is where a remote call diverges from a local one: a field the
// codec drops is indistinguishable from a field the service never set. The
// repository's own standard is that a test double evaluates rather than
// accepts, and for a transport that means it must actually serialize.
func (f *fakeBus) Call(ctx context.Context, _ string, method string, req any, resp any) error {
	handler, ok := f.handler(method)
	if !ok {
		return fmt.Errorf("fakeBus: no handler for %s", method)
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	codec := bus.JSONCodec{}
	payload, err := codec.Marshal(req)
	if err != nil {
		return fmt.Errorf("fakeBus: encode %s request: %w", method, err)
	}
	answer, err := handler(bus.NewRPCContext(ctx, method, payload, codec))
	if err != nil {
		return err
	}
	encoded, err := codec.Marshal(answer)
	if err != nil {
		return fmt.Errorf("fakeBus: encode %s response: %w", method, err)
	}
	if err := codec.Unmarshal(encoded, resp); err != nil {
		return fmt.Errorf("fakeBus: decode %s response: %w", method, err)
	}
	return nil
}

func (f *fakeBus) CallTo(ctx context.Context, svcType string, _ int32, method string, req any, resp any) error {
	return f.Call(ctx, svcType, method, req, resp)
}

func (f *fakeBus) CallWithTimeout(svcType string, method string, req any, resp any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return f.Call(ctx, svcType, method, req, resp)
}

func (f *fakeBus) CallAsync(svcType string, method string, req any, cb func([]byte, error)) {
	go func() {
		var raw map[string]any
		err := f.Call(context.Background(), svcType, method, req, &raw)
		if cb == nil {
			return
		}
		if err != nil {
			cb(nil, err)
			return
		}
		encoded, encErr := bus.JSONCodec{}.Marshal(raw)
		cb(encoded, encErr)
	}()
}

// The send half is unused by mail's RPC surface and fails loudly rather than
// silently succeeding: a test that started depending on it should say so.
func (f *fakeBus) Send(int32, string, any) error {
	return errors.New("fakeBus: Send is not implemented")
}

func (f *fakeBus) SendByType(string, int32, string, any) error {
	return errors.New("fakeBus: SendByType is not implemented")
}

func (f *fakeBus) Broadcast(string, string, any) error {
	return errors.New("fakeBus: Broadcast is not implemented")
}

func (f *fakeBus) BroadcastAll(string, any) error {
	return errors.New("fakeBus: BroadcastAll is not implemented")
}

func (f *fakeBus) Handle(string, string, bus.HandlerFunc) error {
	return errors.New("fakeBus: Handle is not implemented")
}

var _ bus.IBus = (*fakeBus)(nil)

// invoke drives one handler the way the bus would: encode the request, call,
// and hand back the response value.
func invoke(t *testing.T, handler bus.RpcHandlerFunc, method string, req any) (any, error) {
	t.Helper()
	codec := bus.JSONCodec{}
	payload, err := codec.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return handler(bus.NewRPCContext(context.Background(), method, payload, codec))
}
