package nettransport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// U-0131 · C2（空洞测试）· nightly gap map core `nettransport` 10/20。
//
// 准入对会话状态的三种拒绝——未注册、正在排空（RemoveSession 后下游仍在发）、
// 已失败（可靠通道出错后队列进入终态）——各有自己的哨兵，且失败态要带上下游
// 的原因；nil 传输的 AdmitBatch / session 报 ErrTransportClosed。控制面：无目标不
// 能构造、nil 面 / 零会话拒绝、非控制载荷在 UDP 处理器里是 ErrInvalidControl、
// ServeUDP 缺任一方拒绝。`inspectDatagramBatch:679` 的批量上限在 AdmitBatch 路径
// 上被 656 行同一条件先挡住，记冗余；`validateDatagramBatch` 无调用方，删除。

// failingReliableTransport keeps the datagram lane stuck (ignoring ctx) and
// fails every reliable send, so the session reaches its failed state while it
// is still registered.
type failingReliableTransport struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (transport *failingReliableTransport) SendDatagram(context.Context, core.SessionID, []byte) error {
	transport.once.Do(func() { close(transport.started) })
	<-transport.release
	return nil
}

func (*failingReliableTransport) SendReliable(context.Context, core.SessionID, []byte) error {
	return errors.New("stream reset by peer")
}

func waitStats(t *testing.T, transport *AsyncTransport, ready func(AsyncTransportStats) bool) AsyncTransportStats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats := transport.Stats()
		if ready(stats) {
			return stats
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport never reached the expected state: %+v", stats)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdmitBatchRefusesUnregisteredDrainingAndFailedSessions(t *testing.T) {
	ctx := context.Background()
	var none *AsyncTransport
	if err := none.AdmitBatch(ctx, []OutboundFrame{{Session: 1, Reliable: []byte("x")}}); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("AdmitBatch on a nil transport = %v", err)
	}
	if _, err := none.session(1); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("session on a nil transport = %v", err)
	}

	// 未注册。
	transport := admissionTransport(t, nil)
	if err := transport.AdmitBatch(ctx, []OutboundFrame{{Session: 99, Reliable: []byte("x")}}); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("AdmitBatch to an unregistered session = %v", err)
	}
	if _, err := transport.session(99); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("session(99) = %v", err)
	}

	// 排空中：下游忽略 ctx，RemoveSession 后会话仍在表里但 closing。
	stubborn := &stubbornDatagramTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.AllowOpaqueDatagrams = true
	config.SendTimeout = time.Second
	draining, err := NewAsyncTransport(stubborn, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := draining.RegisterSession(core.SessionInfo{ID: 41}); err != nil {
		t.Fatal(err)
	}
	if err := draining.SendDatagram(ctx, 41, []byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, stubborn.started)
	if !draining.RemoveSession(41) {
		t.Fatal("session should begin draining")
	}
	waitStats(t, draining, func(s AsyncTransportStats) bool { return s.DrainingSessions == 1 })
	err = draining.AdmitBatch(ctx, []OutboundFrame{{Session: 41, Reliable: []byte("late")}})
	var admission AdmissionError
	if !errors.As(err, &admission) || admission.Session != 41 || !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("AdmitBatch to a draining session = %v, want AdmissionError{41, ErrSessionNotRegistered}", err)
	}
	if draining.Stats().ReliableQueued != 0 {
		t.Fatal("a draining session accepted new reliable work")
	}
	close(stubborn.release)
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := draining.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	// 已失败：可靠通道出错后队列进入终态，准入要带上下游原因。
	failing := &failingReliableTransport{started: make(chan struct{}), release: make(chan struct{})}
	failedTransport, err := NewAsyncTransport(failing, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := failedTransport.RegisterSession(core.SessionInfo{ID: 51}); err != nil {
		t.Fatal(err)
	}
	if err := failedTransport.SendDatagram(ctx, 51, []byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, failing.started)
	if err := failedTransport.SendReliable(ctx, 51, []byte("first")); err != nil {
		t.Fatal(err)
	}
	waitStats(t, failedTransport, func(s AsyncTransportStats) bool { return s.SendErrors == 1 && s.DrainingSessions == 1 })
	err = failedTransport.AdmitBatch(ctx, []OutboundFrame{{Session: 51, Reliable: []byte("second")}})
	if !errors.Is(err, ErrSessionFailed) || err == nil || !strings.Contains(err.Error(), "stream reset by peer") {
		t.Fatalf("AdmitBatch to a failed session = %v, want ErrSessionFailed carrying the downstream cause", err)
	}
	if failedTransport.Stats().ReliableQueued != 1 {
		t.Fatal("a failed session accepted new reliable work")
	}
	close(failing.release)
	closeCtx2, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	if err := failedTransport.Close(closeCtx2); err != nil {
		t.Fatal(err)
	}
}

type recordingControlTarget struct {
	sessions []core.SessionID
	payloads [][]byte
}

func (target *recordingControlTarget) HandleControl(session core.SessionID, payload []byte) error {
	target.sessions = append(target.sessions, session)
	target.payloads = append(target.payloads, append([]byte(nil), payload...))
	return nil
}

func TestControlPlaneRefusesMissingPartsAndNonControlPayloads(t *testing.T) {
	if plane, err := NewControlPlane(nil); !errors.Is(err, ErrProtocolConfig) || plane != nil {
		t.Fatalf("NewControlPlane(nil) = (%v, %v)", plane, err)
	}
	target := &recordingControlTarget{}
	plane, err := NewControlPlane(target)
	if err != nil {
		t.Fatal(err)
	}
	control, err := core.EncodeControl(core.ControlMessage{Type: core.ControlAck, RoomID: 1, Epoch: 1, Tick: 1, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	var none *ControlPlane
	if _, err := none.TryHandle(7, control); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("TryHandle on a nil plane = %v", err)
	}
	if _, err := (&ControlPlane{}).TryHandle(7, control); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("TryHandle on a plane without a target = %v", err)
	}
	if _, err := plane.TryHandle(0, control); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("TryHandle for session 0 = %v", err)
	}
	if handled, err := plane.TryHandle(7, []byte("not control")); handled || err != nil {
		t.Fatalf("TryHandle with a business payload = (%v, %v), want (false, nil)", handled, err)
	}
	if len(target.sessions) != 0 {
		t.Fatal("a refused control message reached the target")
	}
	if handled, err := plane.TryHandle(7, control); !handled || err != nil || len(target.sessions) != 1 || target.sessions[0] != 7 {
		t.Fatalf("TryHandle with a control message = (%v, %v), target=%v", handled, err, target.sessions)
	}

	handler := plane.UDPHandler()
	if err := handler(context.Background(), 7, []byte("not control"), nil); !errors.Is(err, core.ErrInvalidControl) {
		t.Fatalf("UDP handler with a business payload = %v, want ErrInvalidControl", err)
	}
	if err := handler(context.Background(), 7, control, nil); err != nil || len(target.sessions) != 2 {
		t.Fatalf("UDP handler with a control message = %v, target=%v", err, target.sessions)
	}
	if err := none.ServeUDP(context.Background(), &UDPTransport{}); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("ServeUDP on a nil plane = %v", err)
	}
	if err := plane.ServeUDP(context.Background(), nil); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("ServeUDP without a transport = %v", err)
	}
}
