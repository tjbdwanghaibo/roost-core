package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"

	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

// Both Mods publish the same capability NAME, which is what makes them
// mutually exclusive in one process: the registry refuses a duplicate, so a
// process cannot end up holding both a local service and a client to itself
// with the winner decided by registration order.
//
// The name equality is asserted here because it is the property that produces
// the exclusion. The exclusion itself, and the owning Mod's own wiring, need a
// real Redis and are covered in integration/.
func TestBothModsPublishTheSameCapabilityName(t *testing.T) {
	owner, client := NewMod(nil, nil), NewClientMod()
	if owner.Name() != client.Name() {
		t.Fatalf("the two Mods publish different names (%q and %q); a consumer would have to "+
			"know which deployment it is in, and a process could hold both",
			owner.Name(), client.Name())
	}
	if owner.Name() != mods.ModMail {
		t.Fatalf("the Mods publish %q, want %q", owner.Name(), mods.ModMail)
	}
}

// The client Mod publishes the capability as the INTERFACE, so a consumer's
// lookup is the one it writes for the owning deployment too.
func TestTheClientModPublishesTheInterfaceNotTheConcreteType(t *testing.T) {
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModBus, newFakeBus()); err != nil {
		t.Fatal(err)
	}
	mod := NewClientMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	// The lookup a consumer writes, once, for both deployments.
	if service, ok := app.Lookup[Mail](registry, mods.ModMail); !ok || service == nil {
		t.Fatal("app.Lookup[Mail] did not resolve; the capability is not published as the interface")
	}
	// And NOT as either concrete type.
	//
	// Both halves matter. A consumer that asserted on *BusClient would break
	// in the owning process; one that asserted on *Service would break in
	// every other process — and that second one compiles and passes today,
	// failing only on the day mail is split out, which is the plan.
	//
	// Neither is reachable, because the capability's dynamic type is a wrapper
	// that satisfies Mail and nothing else. Converting to the interface at the
	// call site would NOT achieve this: `Value: Mail(x)` stores an any whose
	// dynamic type is still x's, so the assertion succeeds anyway.
	if _, concrete := app.Lookup[*Service](registry, mods.ModMail); concrete {
		t.Fatal("the capability resolves as *Service; a consumer can bind to the local type " +
			"and will break when mail moves into its own process")
	}
	if _, concrete := app.Lookup[*BusClient](registry, mods.ModMail); concrete {
		t.Fatal("the capability resolves as *BusClient; a consumer can bind to the remote type " +
			"and will break in the process that owns mail")
	}
}

// The client Mod needs the bus and says so by name when it is absent.
func TestTheClientModFailsWithoutTheBus(t *testing.T) {
	cfg := viper.New()
	mod := NewClientMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	err := mod.Provide(app.NewRegistry(cfg))
	if err == nil {
		t.Fatal("the client Mod provided with no bus capability")
	}
	if !strings.Contains(err.Error(), string(mods.ModBus)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
}

// A process that serves mail must hold the local service, not a client to
// itself — otherwise it forwards every request to itself.
func TestTheServerRefusesToRunOnAClientCapability(t *testing.T) {
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModBus, newFakeBus()); err != nil {
		t.Fatal(err)
	}
	client, err := NewBusClient(newFakeBus(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Registered the way ClientMod registers it — through Capability.
	//
	// The first version of this test registered an unwrapped client, so it
	// passed while the real path was broken: the Server asked whether the
	// value was a *BusClient, and the wrapper that exists to hide the concrete
	// type defeated exactly that question. A test that does not go through the
	// production registration is a test of something else.
	if err := registry.Register(CapabilityName, Capability(client)); err != nil {
		t.Fatal(err)
	}
	err = NewServer().Init(registry)
	if err == nil {
		t.Fatal("the server started on a bus client; it would forward every request to itself")
	}
	if !strings.Contains(err.Error(), string(LocalCapabilityName)) {
		t.Fatalf("the error does not name the owner-only capability: %v", err)
	}
}

func TestTheServerRefusesAMissingCapability(t *testing.T) {
	cfg := viper.New()
	// No mail capability at all.
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModBus, newFakeBus()); err != nil {
		t.Fatal(err)
	}
	if err := NewServer().Init(registry); err == nil {
		t.Fatal("the server started with no mail capability")
	}
	// And with mail but no bus: a process that serves mail over a bus needs one.
	h := newHarness(t)
	registry2 := app.NewRegistry(cfg)
	for _, capability := range OwnerCapabilities(h.service) {
		if err := registry2.Register(capability.Name, capability.Value); err != nil {
			t.Fatal(err)
		}
	}
	err := NewServer().Init(registry2)
	if err == nil {
		t.Fatal("the server started with no bus")
	}
	if !strings.Contains(err.Error(), string(mods.ModBus)) {
		t.Fatalf("the error does not name the missing bus: %v", err)
	}
}

// --- helpers ---

// The transport tests (Methods drift, RegisterHandlers, both implementations,
// error codes, identity) moved to roost-core/service/mail with the transport
// half (M-11). What stays is the Mod / Server shape.

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
