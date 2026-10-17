package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

func bindRequest(binding Binding) ControlRequest {
	binding.BindID = ""
	return ControlRequest{Version: InternalVersion, Operation: Bind, Binding: binding, Ticket: "ticket"}
}

func TestConcurrentBindSharesOneOwnerAndOneIdentity(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	request := bindRequest(identity)
	var owners atomic.Int32
	var wait sync.WaitGroup
	records := make(chan *bindingRecord, 32)
	for range 32 {
		wait.Go(func() {
			record, owner, err := table.reserve(request)
			if err != nil {
				t.Error(err)
				return
			}
			if owner {
				owners.Add(1)
			}
			records <- record
		})
	}
	wait.Wait()
	close(records)
	if owners.Load() != 1 {
		t.Fatalf("owners=%d", owners.Load())
	}
	var first *bindingRecord
	for record := range records {
		if first == nil {
			first = record
		}
		if first != record {
			t.Fatal("duplicate Bind allocated another record")
		}
	}
	_, result := table.finish(first, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := table.await(ctx, first)
	if err != nil || got.Binding != result.Binding || got.Outcome != Completed {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	altered := request
	altered.Ticket = "another-ticket"
	if _, _, err := table.reserve(altered); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("changed proof=%v", err)
	}
}

func TestBindingActivationReplacementAndOldCloseAreIsolated(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	old, _, err := table.reserve(bindRequest(identity))
	if err != nil {
		t.Fatal(err)
	}
	_, result := table.finish(old, true)
	renew := ControlRequest{Version: InternalVersion, Operation: Renew, Binding: result.Binding}
	if _, _, err := table.control(renew); !errors.Is(err, ErrBindingHeld) {
		t.Fatalf("held renewal=%v", err)
	}
	activate := renew
	activate.Operation = Activate
	_, activated, err := table.control(activate)
	if err != nil {
		t.Fatal(err)
	}
	_, duplicate, err := table.control(activate)
	if err != nil || duplicate.LeaseUntilUnixNano != activated.LeaseUntilUnixNano {
		t.Fatalf("duplicate activation extended lease: %+v %v", duplicate, err)
	}
	nextIdentity := identity
	nextIdentity.ConnectionNonce = newBindingToken()
	next, _, err := table.reserve(bindRequest(nextIdentity))
	if err != nil {
		t.Fatal(err)
	}
	previous, result := table.finish(next, true)
	if previous != old || old.state != bindingClosed || result.Binding.BindID == activated.Binding.BindID {
		t.Fatal("replacement did not terminate exact old binding")
	}
	if _, changed := table.close(activated.Binding); changed {
		t.Fatal("old close changed replacement")
	}
	if table.bySession[identity.SessionID] != next {
		t.Fatal("old close removed current session")
	}
	unbind := ControlRequest{Version: InternalVersion, Operation: Unbind, Binding: result.Binding}
	for range 2 {
		if _, result, err := table.control(unbind); err != nil || result.Outcome != Completed {
			t.Fatalf("idempotent unbind=%+v %v", result, err)
		}
	}
	if _, _, err := table.reserve(bindRequest(nextIdentity)); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("late Bind resurrected closed connection: %v", err)
	}
}

func TestBindingTerminalCapacityIsReservedBeforeAdmission(t *testing.T) {
	identity := packetTestBinding()
	config := DefaultConfig()
	config.MaxBindings = 2
	config.MaxTombstones = 2
	table := newBindingTable(config, identity.Game)
	first, _, err := table.reserve(bindRequest(identity))
	if err != nil {
		t.Fatal(err)
	}
	table.finish(first, false)
	secondIdentity := identity
	secondIdentity.ConnectionNonce = newBindingToken()
	second, _, err := table.reserve(bindRequest(secondIdentity))
	if err != nil {
		t.Fatal(err)
	}
	table.finish(second, false)
	thirdIdentity := identity
	thirdIdentity.ConnectionNonce = newBindingToken()
	if _, _, err := table.reserve(bindRequest(thirdIdentity)); !errors.Is(err, ErrAdmissionFull) {
		t.Fatalf("active terminal evicted: %v", err)
	}
	first.terminalUntil = time.Now().Add(-time.Second)
	third, _, err := table.reserve(bindRequest(thirdIdentity))
	if err != nil {
		t.Fatal(err)
	}
	if third.receiverID <= second.receiverID {
		t.Fatal("receiver ID reused")
	}
	if _, _, err := table.reserve(bindRequest(secondIdentity)); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("retained terminal resurrected: %v", err)
	}
}

func TestSessionReplacementDoesNotCrossPlayerIdentity(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	first, _, err := table.reserve(bindRequest(identity))
	if err != nil {
		t.Fatal(err)
	}
	table.finish(first, true)
	other := identity
	other.PlayerID++
	other.ConnectionNonce = newBindingToken()
	second, _, err := table.reserve(bindRequest(other))
	if err != nil {
		t.Fatal(err)
	}
	previous, result := table.finish(second, true)
	if previous != nil || result.Outcome != NotAdmitted || table.bySession[identity.SessionID] != first {
		t.Fatal("another player replaced existing session")
	}
}

// Game Ingress 的激活回调（OnActive）执行期间，这个玩家已经可以推送：Gate 写出鉴权 ACK、准备好接收出站之后
// 才发 Activate。回调里登记连接的推送方（自身视图会立刻唤醒同步）若在回调返回前推送，不能得到
// “玩家没有连接”，否则同步会话被丢、连接却还在，没有任何东西再打开它。激活失败的绑定随即不再可推送。
func TestAnIngressBindingIsPushableWhileItsActivationCallbackRuns(t *testing.T) {
	identity := packetTestBinding()
	var game *GameIngress
	var duringActivation error
	connectionsDuringActivation := -1
	refuse := false
	newGame := func() {
		var err error
		game, err = NewGameIngress(DefaultConfig(), identity.Game, GameDependencies{
			Client: payloadClient{limit: 1 << 20}, Authenticator: AuthenticatorFunc(nil), Ready: func() bool { return true },
			Dispatch: func(context.Context, Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) { return nil, nil },
			QueueFactory: func(OutboundLimits, func(context.Context, uint64, []byte) error, func(uint64, error)) (ReliableQueue, error) {
				return benchGateQueue{}, nil
			},
			Matches: func(context.Context, string, ProcessIdentity) (bool, error) { return true, nil },
			OnActive: func(binding Binding, _ uint64) error {
				duringActivation = game.PushSyncPlayer(context.Background(), binding.PlayerID, 9, []byte("first full"))
				connectionsDuringActivation = game.ActiveSessions(binding.PlayerID)
				if refuse {
					return errors.New("activation refused")
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	activate := func() *bindingRecord {
		t.Helper()
		request := bindRequest(identity)
		request.Binding.ConnectionNonce = newBindingToken()
		record, _, err := game.bindings.reserve(request)
		if err != nil {
			t.Fatal(err)
		}
		_, result := game.bindings.finish(record, true)
		if err := game.register(record, Principal{PlayerID: identity.PlayerID, SessionID: identity.SessionID}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := game.bindings.control(ControlRequest{Version: InternalVersion, Operation: Activate, Binding: result.Binding}); err != nil {
			t.Fatal(err)
		}
		_ = game.activate(context.Background(), record)
		return record
	}

	newGame()
	activate()
	if duringActivation != nil || connectionsDuringActivation != 1 {
		t.Fatalf("inside OnActive: push = %v, connections = %d; want the push admitted and 1 connection", duringActivation, connectionsDuringActivation)
	}
	if got := game.ActiveSessions(identity.PlayerID); got != 1 {
		t.Fatalf("after activation: connections = %d, want 1", got)
	}

	refuse = true
	newGame()
	activate()
	if got := game.ActiveSessions(identity.PlayerID); got != 0 {
		t.Fatalf("after a refused activation: connections = %d, want 0", got)
	}
	if err := game.PushSyncPlayer(context.Background(), identity.PlayerID, 9, []byte("late")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("push after a refused activation = %v, want ErrSessionNotFound", err)
	}
}
