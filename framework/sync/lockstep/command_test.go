package lockstep

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
)

func TestCommandCodecAndOwnership(t *testing.T) {
	for _, c := range []Command{{Operation: OpInput, Frame: 1, Payload: []byte{8, 42}}, {Operation: OpInput, Frame: ^FrameID(0)}, {Operation: OpHash, Frame: 2, Hash: ^uint64(0)}, {Operation: OpCatchup, Frame: 3}} {
		data, err := EncodeCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCommand(data)
		if err != nil || !reflect.DeepEqual(got, c) {
			t.Fatalf("roundtrip %+v %v", got, err)
		}
		if len(c.Payload) > 0 {
			data[len(data)-1] = 0
			if got.Payload[1] != 42 {
				t.Fatal("aliases receive buffer")
			}
		}
	}
	for _, data := range [][]byte{{}, {0xc8, 1, 1}, {0xc8, 2, 1, 1, 0}, {0xc8, 1, 9, 1}, {0xc8, 1, 3, 0}, {0xc8, 1, 3, 1, 0}, {0xc8, 1, 1, 1, 2, 9}, {0xc8, 1, 2, 1, 255, 255, 255, 255, 255, 255, 255, 255, 255, 2}, {0xc8, 1, 3, 128, 128, 128, 128, 16}} {
		if _, err := DecodeCommand(data); err == nil {
			t.Fatalf("accepted %x", data)
		}
	}
	for _, c := range []Command{{Operation: OpInput, Frame: 1, Hash: 1}, {Operation: OpHash, Frame: 1, Payload: []byte{1}}, {Operation: OpCatchup, Frame: 1, Hash: 1}, {Operation: OpInput, Frame: 1, Payload: make([]byte, 1025)}} {
		if _, err := EncodeCommand(c); err == nil {
			t.Fatalf("ambiguous command %+v", c)
		}
	}
}

func TestTCPSenderRejectsFastWorkerBeforeResolverOrSocket(t *testing.T) {
	calls := 0
	sender, err := NewTCPSender(50002, 0, func(nettransport.SessionID) (string, bool) { calls++; return "s", true }, func(context.Context, string, uint32, []byte) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	func() {
		_, release := fctx.NewContext(fctx.WithFastWorker())
		defer release()
		for _, send := range []func(context.Context, nettransport.SessionID, []byte) error{sender.SendDatagram, sender.SendReliable} {
			if err := send(context.Background(), 7, nil); !errors.Is(err, fctx.ErrBlockingInFastWorker) {
				t.Errorf("fast worker accepted: %v", err)
			}
		}
	}()
	if calls != 0 {
		t.Fatalf("fast refusal already touched resolver/socket: %d", calls)
	}
	if err := sender.SendDatagram(context.Background(), 7, nil); err != nil || calls != 2 {
		t.Fatalf("normal caller refused: %v calls=%d", err, calls)
	}
}

func TestRoomCommandUsesCurrentAuthenticatedBinding(t *testing.T) {
	transport := newRecordingTransport()
	r := newTestRoom(t, transport, nil)
	defer r.Close()
	if err := r.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := r.AttachSpectator(102); err != nil {
		t.Fatal(err)
	}
	input := Command{Operation: OpInput, Frame: 1, Payload: []byte{42}}
	if err := r.HandleCommand(999, input); !errors.Is(err, ErrPlayerDetached) {
		t.Fatal(err)
	}
	if err := r.HandleCommand(102, input); !errors.Is(err, ErrPlayerUnknown) {
		t.Fatal(err)
	}
	if err := r.HandleCommand(101, input); err != nil {
		t.Fatal(err)
	}
	frame, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Inputs) != 1 || frame.Inputs[0].Player != 1 || !bytes.Equal(frame.Inputs[0].Payload, []byte{42}) {
		t.Fatalf("authority %+v", frame)
	}
	if err := r.HandleCommand(101, Command{Operation: OpHash, Frame: 1, Hash: 7}); err != nil {
		t.Fatal(err)
	}
	if err := r.HandleCommand(102, Command{Operation: OpHash, Frame: 1, Hash: 7}); !errors.Is(err, ErrPlayerUnknown) {
		t.Fatal(err)
	}
	if err := r.HandleCommand(102, Command{Operation: OpCatchup, Frame: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Attach(1, 103); err != nil {
		t.Fatal(err)
	}
	if err := r.HandleCommand(101, input); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("old connection admitted %v", err)
	}
	if err := r.HandleCommand(103, Command{Operation: OpInput, Frame: 2, Payload: []byte{9}}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := r.HandleCommand(103, input); !errors.Is(err, ErrRoomClosed) {
		t.Fatal(err)
	}
}

func TestTCPSenderBothLanesUseSameRouteAndBudget(t *testing.T) {
	calls := 0
	fail := errors.New("write failed")
	s, err := NewTCPSender(50002, 8, func(id nettransport.SessionID) (string, bool) { return "s", id == 7 }, func(ctx context.Context, id string, msg uint32, p []byte) error {
		if id != "s" || msg != 50002 || !bytes.Equal(p, []byte{0xc7, 1, 0}) {
			t.Fatal("wrong route")
		}
		calls++
		return fail
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, send := range []func(context.Context, nettransport.SessionID, []byte) error{s.SendDatagram, s.SendReliable} {
		if err := send(context.Background(), 7, []byte{0xc7, 1, 0}); !errors.Is(err, fail) {
			t.Fatal(err)
		}
		if err := send(context.Background(), 8, nil); !errors.Is(err, ErrPlayerDetached) {
			t.Fatal(err)
		}
		if err := send(context.Background(), 7, make([]byte, 9)); !errors.Is(err, wire.ErrPacketTooBig) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := send(ctx, 7, nil); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if calls != 2 || s.MaxDatagramPayload() != 8 {
		t.Fatal(calls)
	}
}
