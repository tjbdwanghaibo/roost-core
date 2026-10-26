package physicsroom

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestRoomInputProtobuf(t *testing.T) {
	payload := protowire.AppendTag(nil, 1, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 300)
	payload = protowire.AppendTag(payload, 2, protowire.Fixed32Type)
	payload = protowire.AppendFixed32(payload, math.Float32bits(-10))
	payload = protowire.AppendTag(payload, 4, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	got, err := decodeInput(payload)
	if err != nil || got.Sequence != 300 || got.Acceleration[0] != -10 || !got.Jump {
		t.Fatalf("input=%+v err=%v", got, err)
	}
	for _, bad := range [][]byte{nil, {8}, {8, 0}, {8, 1, 8, 2}, {8, 1, 32, 2}, {8, 1, 21, 0}, {8, 1, 18, 0}} {
		if _, err := decodeInput(bad); err == nil {
			t.Fatalf("invalid protobuf accepted: %x", bad)
		}
	}
}

// fakeRooms 记录调用；Poll 返回空帧，pollErr 非空时让 Relay 以错误结束。
type fakeRooms struct {
	domain.Rooms
	joins, submits, leaves atomic.Int64
	pollErr                atomic.Pointer[error]
}

func (f *fakeRooms) Join(_ context.Context, room domain.Ref, binding gateway.Binding, _ uint64) (domain.Member, error) {
	f.joins.Add(1)
	return domain.Member{Room: room, PlayerID: binding.PlayerID, Token: "token"}, nil
}
func (f *fakeRooms) Submit(context.Context, domain.Member, domain.Input) error {
	f.submits.Add(1)
	return nil
}
func (f *fakeRooms) Poll(_ context.Context, m domain.Member, ack uint64) (domain.Delivery, error) {
	if err := f.pollErr.Load(); err != nil {
		return domain.Delivery{}, *err
	}
	return domain.Delivery{Room: m.Room, Sequence: ack}, nil
}
func (f *fakeRooms) Leave(context.Context, domain.Member) error { f.leaves.Add(1); return nil }

type fakeGame struct{ binding gateway.Binding }

func (fakeGame) PushBound(context.Context, gateway.Binding, uint32, wire.PayloadKind, []byte) error {
	return nil
}
func (g fakeGame) SessionReceiver(id string) (gateway.Binding, uint64, bool) {
	return g.binding, 1, id == g.binding.SessionID
}

type fakeSession struct {
	id     string
	closed chan error
}

func (s fakeSession) Principal() gateway.Principal {
	return gateway.Principal{PlayerID: 1, ServerID: 2, SessionID: s.id}
}
func (fakeSession) Reply(context.Context, any) error { return nil }
func (s fakeSession) Close(err error) error {
	select {
	case s.closed <- err:
	default:
	}
	return nil
}

func ingressBinding() gateway.Binding {
	return gateway.Binding{PlayerID: 1, SessionID: "session", ConnectionNonce: "0123456789abcdef0123456789abcdef", BindID: "1123456789abcdef0123456789abcdef", Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "0123456789abcdef"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "1123456789abcdef"}}
}

func inputPayload(seq uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), seq)
}

// 连接生命周期：首个输入 Join 一次并启动唯一 Relay；断开取消 Relay 并 Leave；Relay 出错关闭连接；Close 排空并拒绝后续。
func TestPlayerIngressJoinsOnceAndReleasesWithTheConnection(t *testing.T) {
	ctx := context.Background()
	if _, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 1, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) { return domain.Ref{}, 0, nil }}); err == nil {
		t.Fatal("same input and frame message id accepted")
	}
	var seats atomic.Int64
	room := domain.Ref{ID: "room", Epoch: 1}
	p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
		seats.Add(1)
		return room, 51, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding := ingressBinding()
	session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
	if _, err = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, domain.ErrStopping) || p.Ready() {
		t.Fatalf("dispatch before attach: %v ready=%v", err, p.Ready())
	}
	rooms := &fakeRooms{}
	if err = p.Attach(rooms, fakeGame{binding: binding}); err != nil || !p.Ready() {
		t.Fatalf("attach: %v ready=%v", err, p.Ready())
	}
	if _, err = p.Dispatch(ctx, session, 3, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("foreign message id: %v", err)
	}
	if _, err = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("input before OnActive: %v", err)
	}
	if err = p.OnActive(binding, 1); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		reply, err := p.Dispatch(ctx, session, 2, uint32(seq), wire.PayloadProtobuf, inputPayload(seq))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := reply.(*gateway.TCPResponse); !ok || r.MessageID != 2 || r.Sequence != uint32(seq) {
			t.Fatalf("reply=%+v", reply)
		}
	}
	if seats.Load() != 1 || rooms.joins.Load() != 1 || rooms.submits.Load() != 3 {
		t.Fatalf("seats=%d joins=%d submits=%d", seats.Load(), rooms.joins.Load(), rooms.submits.Load())
	}
	p.OnClosed(binding, 1)
	waitFor(t, func() bool { return rooms.leaves.Load() == 1 })
	if _, err = p.Dispatch(ctx, session, 2, 4, wire.PayloadProtobuf, inputPayload(4)); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("input after close: %v", err)
	}

	// 新连接重新 Join；Relay 因 RPC 错误结束时关闭该连接。
	if err = p.OnActive(binding, 2); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("poll failed")
	rooms.pollErr.Store(&failure)
	if _, err = p.Dispatch(ctx, session, 2, 5, wire.PayloadProtobuf, inputPayload(5)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-session.closed:
		if !errors.Is(got, failure) {
			t.Fatalf("session closed with %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("relay failure did not close the session")
	}
	if rooms.joins.Load() != 2 {
		t.Fatalf("joins=%d", rooms.joins.Load())
	}

	stop, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = p.Close(stop); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("ready after close")
	}
	if err = p.OnActive(binding, 3); !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("OnActive after close: %v", err)
	}
	if _, err = p.Dispatch(ctx, session, 2, 6, wire.PayloadProtobuf, inputPayload(6)); !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("dispatch after close: %v", err)
	}
}

// 关闭与输入并发：Close 之后不会再启动新的 Relay（WaitGroup 不在 Wait 之后 Add），在途 Join 立即 Leave。
func TestPlayerIngressCloseRacesInput(t *testing.T) {
	ctx := context.Background()
	for range 20 {
		rooms := &fakeRooms{}
		binding := ingressBinding()
		p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
			return domain.Ref{ID: "room", Epoch: 1}, 51, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Attach(rooms, fakeGame{binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err = p.OnActive(binding, 1); err != nil {
			t.Fatal(err)
		}
		session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1))
		}()
		stop, cancel := context.WithTimeout(ctx, time.Second)
		if err = p.Close(stop); err != nil {
			t.Fatal(err)
		}
		cancel()
		wg.Wait()
		waitFor(t, func() bool { return rooms.leaves.Load() == rooms.joins.Load() })
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
