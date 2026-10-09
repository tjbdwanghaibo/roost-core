package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

func packetTestBinding() Binding {
	return Binding{PlayerID: 7, SessionID: "account-session", Gate: ProcessIdentity{ServerID: 1, Incarnation: "gate_instance_001"}, Game: ProcessIdentity{ServerID: 2, Incarnation: "game_instance_002"}, ConnectionNonce: newBindingToken(), BindID: newBindingToken()}
}

func TestBindingPacketHasCompleteIdentityAndCurrentFormat(t *testing.T) {
	binding := packetTestBinding()
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	request := ForwardRequest{Version: InternalVersion, Binding: binding, RequestID: newBindingToken(), MessageID: 42, Sequence: 2, Kind: wire.PayloadProtobuf, Payload: []byte{0, 255, 7}}
	if err := request.Validate(1024); err != nil {
		t.Fatal(err)
	}
	codec := bus.MessagePackCodec{}
	data, err := codec.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ForwardRequest
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Binding != binding || string(decoded.Payload) != string(request.Payload) {
		t.Fatalf("roundtrip=%+v", decoded)
	}
	for _, mutate := range []func(*ForwardRequest){
		func(r *ForwardRequest) { r.Version++ }, func(r *ForwardRequest) { r.Binding.Game.Incarnation = "" }, func(r *ForwardRequest) { r.Binding.BindID = "" }, func(r *ForwardRequest) { r.Kind = wire.PayloadSync }, func(r *ForwardRequest) { r.MessageID = 0 }, func(r *ForwardRequest) { r.Sequence = 0 }, func(r *ForwardRequest) { r.Payload = make([]byte, 1025) },
	} {
		invalid := request
		mutate(&invalid)
		if err := invalid.Validate(1024); err == nil {
			t.Fatalf("invalid packet admitted: %+v", invalid)
		}
	}
	// Bind 的随机 ID 由 Game 签发，客户端/Gate 不能自选接管旧绑定。
	bind := ControlRequest{Version: InternalVersion, Operation: Bind, Binding: binding, Ticket: "ticket"}
	if err := bind.Validate(); err == nil {
		t.Fatal("caller supplied BindID")
	}
	bind.Binding.BindID = ""
	if err := bind.Validate(); err != nil {
		t.Fatal(err)
	}
	bind.Operation = Activate
	if err := bind.Validate(); err == nil {
		t.Fatal("activation without confirmed binding")
	}
}

func TestBudgetConsumesElapsedTimeAndDoesNotRestartAtReceiver(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	budget := Budget{DeadlineUnixNano: time.Now().Add(20 * time.Millisecond).UnixNano(), RemainingNanos: int64(time.Second)}
	ctx, stop, err := budget.Context(parent, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	deadline, _ := ctx.Deadline()
	if time.Until(deadline) > 25*time.Millisecond {
		t.Fatal("receiver restarted remaining budget")
	}
	budget.DeadlineUnixNano = time.Now().Add(-time.Nanosecond).UnixNano()
	if _, _, err := budget.Context(parent, time.Second, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired=%v", err)
	}
	budget.DeadlineUnixNano = time.Now().Add(time.Second).UnixNano()
	budget.RemainingNanos = int64(time.Millisecond)
	ctx, stop, err = budget.Context(parent, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	deadline, _ = ctx.Deadline()
	if time.Until(deadline) > 2*time.Millisecond {
		t.Fatal("wall clock extended remaining budget")
	}
	if _, err := BudgetFromContext(context.Background()); err == nil {
		t.Fatal("missing source deadline")
	}
}

func TestSubjectSourceAndAudienceAreExplicit(t *testing.T) {
	binding := packetTestBinding()
	subject, err := ChannelSubject("roost.access", "game", binding.Game, "gate", binding.Gate, "forward")
	if err != nil {
		t.Fatal(err)
	}
	if subject != "roost.access.game.2.game_instance_002.from.gate.1.gate_instance_001.forward" {
		t.Fatalf("subject=%s", subject)
	}
	for _, namespace := range []string{"roost..access", "roost.*", "roost.>", "roost access"} {
		if err := ValidateNamespace(namespace); err == nil {
			t.Fatalf("namespace=%q", namespace)
		}
	}
	identity := binding.Gate
	identity.Incarnation = strings.Repeat("x", 129)
	if err := identity.Validate(); err == nil {
		t.Fatal("unbounded identity")
	}
	for _, audience := range []Audience{{Kind: AllOnline}, {Kind: GameOnline, GameID: 2}, {Kind: Players, PlayerIDs: []int64{7, 7}}} {
		if err := audience.Validate(16); err != nil {
			t.Fatal(err)
		}
	}
	for _, audience := range []Audience{{Kind: 0}, {Kind: AllOnline, GameID: 2}, {Kind: GameOnline}, {Kind: Players}, {Kind: Players, PlayerIDs: []int64{0}}} {
		if err := audience.Validate(16); err == nil {
			t.Fatalf("audience=%+v", audience)
		}
	}
}
