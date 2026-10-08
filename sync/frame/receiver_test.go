package frame

import (
	"errors"
	"testing"
)

func receiverPacket(t *testing.T, epoch, tick, base uint32) []byte {
	t.Helper()
	kind := Delta
	if base == 0 {
		kind = Full
	}
	raw, err := Encode(Frame{SnapshotMeta: SnapshotMeta{RoomID: 1, Epoch: epoch, Tick: tick, SchemaVersion: 1}, Kind: kind, BaseTick: base}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReceiverGapRequiresFullAndIgnoresOldEpoch(t *testing.T) {
	r := NewReceiver(DefaultLimits())
	applied := 0
	apply := func(Frame) error { applied++; return nil }
	for _, step := range []struct {
		epoch, tick, base uint32
		accepted, gap     bool
	}{
		{1, 2, 1, false, true}, // 未见全量不能从delta开始。
		{1, 1, 0, true, false},
		{1, 2, 1, true, false},
		{1, 2, 1, false, false}, // 重复不再调用业务。
		{1, 4, 3, false, true},
		{1, 3, 2, false, true}, // 已发现缺口，不能假设迟到一帧即可修复业务状态。
		{2, 1, 0, true, false}, // 服务端Hold/Ready打开新epoch。
		{1, 5, 4, false, false},
		{2, 2, 1, true, false},
	} {
		before := applied
		ok, err := r.Receive(receiverPacket(t, step.epoch, step.tick, step.base), apply)
		if ok != step.accepted || errors.Is(err, ErrBaselineMismatch) != step.gap || !step.gap && err != nil {
			t.Fatalf("step %+v: accepted=%v err=%v", step, ok, err)
		}
		if (applied > before) != step.accepted {
			t.Fatalf("step %+v: callback ran on a refused or duplicate frame", step)
		}
	}
}

func TestReceiverApplicationFailureRequiresRecovery(t *testing.T) {
	r := NewReceiver(DefaultLimits())
	accept := func(Frame) error { return nil }
	if _, err := r.Receive(receiverPacket(t, 1, 1, 0), accept); err != nil {
		t.Fatal(err)
	}
	want := errors.New("business decode failed after partial application")
	if ok, err := r.Receive(receiverPacket(t, 1, 2, 1), func(Frame) error { return want }); ok || !errors.Is(err, want) {
		t.Fatalf("application error lost: %v %v", ok, err)
	}
	if _, err := r.Receive(receiverPacket(t, 1, 2, 1), accept); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("failed application advanced/kept a usable baseline: %v", err)
	}
	if _, err := r.Receive(receiverPacket(t, 2, 1, 0), accept); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverPanicDoesNotLeaveUsableBaselineOrBusyReceiver(t *testing.T) {
	r := NewReceiver(DefaultLimits())
	full := receiverPacket(t, 1, 1, 0)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("application panic was swallowed")
			}
		}()
		_, _ = r.Receive(full, func(Frame) error { panic("partial application") })
	}()
	if _, err := r.Receive(receiverPacket(t, 1, 2, 1), func(Frame) error { return nil }); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("panic left usable/busy receiver: %v", err)
	}
	if _, err := r.Receive(full, func(Frame) error { return nil }); err != nil {
		t.Fatalf("full recovery after panic: %v", err)
	}
}

func TestReceiverResetInsideCallbackCannotRestoreOldBaseline(t *testing.T) {
	r := NewReceiver(DefaultLimits())
	full := receiverPacket(t, 1, 1, 0)
	if ok, err := r.Receive(full, func(Frame) error {
		if _, err := r.Receive(full, func(Frame) error { return nil }); !errors.Is(err, ErrReceiverBusy) {
			t.Fatalf("recursive Receive = %v", err)
		}
		r.Reset()
		return nil
	}); ok || !errors.Is(err, ErrReceiverReset) {
		t.Fatalf("old callback restored reset baseline: %v %v", ok, err)
	}
	if _, err := r.Receive(receiverPacket(t, 1, 2, 1), func(Frame) error { return nil }); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("new connection accepted old delta: %v", err)
	}
	if _, err := r.Receive(full, func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverRejectsStreamAndSchemaSwitchAndMalformedFrame(t *testing.T) {
	for _, what := range []string{"stream", "schema", "malformed"} {
		t.Run(what, func(t *testing.T) {
			r := NewReceiver(DefaultLimits())
			accept := func(Frame) error { return nil }
			if _, err := r.Receive(receiverPacket(t, 1, 1, 0), accept); err != nil {
				t.Fatal(err)
			}
			f := Frame{SnapshotMeta: SnapshotMeta{RoomID: 1, Epoch: 1, Tick: 2, SchemaVersion: 1}, Kind: Delta, BaseTick: 1}
			if what == "stream" {
				f.RoomID = 2
			}
			if what == "schema" {
				f.SchemaVersion = 2
			}
			raw, err := Encode(f, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if what == "malformed" {
				raw = raw[:3]
			}
			called := false
			if _, err := r.Receive(raw, func(Frame) error { called = true; return nil }); err == nil || called {
				t.Fatalf("unsafe application: called=%v err=%v", called, err)
			}
			if _, err := r.Receive(receiverPacket(t, 1, 2, 1), accept); !errors.Is(err, ErrBaselineMismatch) {
				t.Fatalf("invalid frame left usable baseline: %v", err)
			}
		})
	}
}
