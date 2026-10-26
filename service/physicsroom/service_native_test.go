//go:build physx && cgo

package physicsroom

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func testConfig() Config {
	return Config{MaxRooms: 2, MaxPlayers: 2, MaxBodies: 10, MaxFrameBytes: 4096, InputTTL: 250 * time.Millisecond, MemberTTL: time.Second, MaxLifetime: time.Minute, Radius: 12, Margin: 2, Commit: func(context.Context, Ref, gamephysics.Frame) error { return nil }}
}
func testSpec(id string, s *Service) Spec {
	return Spec{ID: id, Instance: s.instance, LifetimeSeconds: 30, Bodies: []physx.Body{{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{25, .5, 25}}, {ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1}}}
}
func testBinding() Binding {
	return Binding{PlayerID: 1, SessionID: "session", ConnectionNonce: "0123456789abcdef0123456789abcdef", BindID: "1123456789abcdef0123456789abcdef", Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "0123456789abcdef"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "1123456789abcdef"}}
}
func newTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func TestMembershipAndDelivery(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	metrics := servicemetrics.NewRecorder()
	cfg.Metrics = metrics
	s := newTestService(t, cfg)
	ref, err := s.Create(ctx, testSpec("room", s))
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.Create(ctx, testSpec("room", s))
	if err != nil || same != ref {
		t.Fatalf("create replay: %+v %v", same, err)
	}
	m, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	bad := m
	bad.Token = "stale"
	if err = s.Submit(ctx, bad, Input{Sequence: 1}); !errors.Is(err, ErrMember) {
		t.Fatal(err)
	}
	input := Input{Sequence: 1, Acceleration: physx.Vec3{10, 0, 0}}
	if err = s.Submit(ctx, m, input); err != nil {
		t.Fatal(err)
	}
	if err = s.Submit(ctx, m, input); err != nil {
		t.Fatal(err)
	}
	input.Acceleration[0] = 11
	if err = s.Submit(ctx, m, input); !errors.Is(err, ErrSequence) {
		t.Fatal(err)
	}
	frame, err := s.Poll(ctx, m, 0)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.Poll(ctx, m, 0)
	if err != nil || !bytes.Equal(frame.Payload, retry.Payload) || frame.Sequence != retry.Sequence {
		t.Fatalf("lost reply changed baseline: %v", err)
	}
	var replica gamephysics.RoomReplica
	if err = replica.Apply(frame.Payload); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Poll(ctx, m, 99); !errors.Is(err, ErrSequence) {
		t.Fatal(err)
	}
	if _, err = s.Poll(ctx, m, frame.Sequence); err != nil {
		t.Fatal(err)
	}
	if err = s.Leave(ctx, m); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil || replacement.Token == m.Token {
		t.Fatal(err)
	}
	if err = s.Leave(ctx, m); !errors.Is(err, ErrMember) {
		t.Fatal(err)
	}
	if err = s.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, testSpec("room", s)); !errors.Is(err, ErrMissing) {
		t.Fatalf("destroyed create replay reopened room: %v", err)
	}
	if metrics.Count("accepted:create") != 1 || metrics.Count("replayed:create") != 1 {
		t.Fatal(metrics.Events())
	}
}
func TestConcurrentCreateHonorsCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRooms = 1
	s := newTestService(t, cfg)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Create(context.Background(), testSpec(id, s)); results <- err }()
	}
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrCapacity) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("success=%d refused=%d", success, refused)
	}
}
func TestShutdownWaitsForCommit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	cfg := testConfig()
	cfg.Commit = func(context.Context, Ref, gamephysics.Frame) error {
		close(entered)
		<-release
		return errors.New("commit failed")
	}
	s := newTestService(t, cfg)
	ref, err := s.Create(context.Background(), testSpec("blocked", s))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("commit not entered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.mu.Lock()
	r := s.rooms[ref.ID]
	s.mu.Unlock()
	select {
	case <-r.done:
		t.Fatal("freed active native world")
	default:
	}
	close(release)
	if err = s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(context.Background(), testSpec("new", s)); !errors.Is(err, ErrStopping) {
		t.Fatal(err)
	}
}

func TestFrameBudgetAndStaleEpoch(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFrameBytes = 256
	s := newTestService(t, cfg)
	spec := testSpec("large", s)
	for _, id := range []uint64{2, 3, 4} {
		spec.Bodies = append(spec.Bodies, physx.Body{ID: id, Shape: physx.Sphere, Position: physx.Vec3{float32(id) * 2, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1})
	}
	if _, err := s.Create(context.Background(), spec); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	ref, err := s.Create(context.Background(), testSpec("small", s))
	if err != nil {
		t.Fatal(err)
	}
	wrong := ref
	wrong.Epoch++
	if _, err = s.Join(context.Background(), wrong, testBinding(), 1); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	if err = s.Destroy(context.Background(), wrong); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	if _, err = s.Join(context.Background(), ref, testBinding(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousOwnerCreateIsRejected(t *testing.T) {
	old := newTestService(t, testConfig())
	spec := testSpec("old-request", old)
	next := newTestService(t, testConfig())
	if _, err := next.Create(context.Background(), spec); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("old process request recreated a room: %v", err)
	}
}

func TestJumpConsumedOnceAndReplayDoesNotRearm(t *testing.T) {
	ctx := context.Background()
	frames := make(chan gamephysics.Frame, 256)
	cfg := testConfig()
	cfg.MemberTTL = 5 * time.Second
	cfg.Commit = func(_ context.Context, _ Ref, f gamephysics.Frame) error { frames <- f; return nil }
	s := newTestService(t, cfg)
	spec := testSpec("jump", s)
	spec.Bodies[1].Position[1] = .5
	ref, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Submit(ctx, m, Input{Sequence: 1, Acceleration: physx.Vec3{0, 1, 0}}); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("vertical input: %v", err)
	}
	in := Input{Sequence: 1, Jump: true}
	if err = s.Submit(ctx, m, in); err != nil {
		t.Fatal(err)
	}
	peak := float32(0)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-frames:
			peak = max(peak, f.States[0].Position[1])
			// 每拍重发相同 seq，不能重置 TTL，也不能在落地后再次触发。
			if err = s.Submit(ctx, m, in); err != nil {
				t.Fatal(err)
			}
			if f.Tick >= 100 {
				if peak < 2 || f.States[0].Position[1] > .6 {
					t.Fatalf("jump replay: peak=%f final=%+v", peak, f.States[0])
				}
				return
			}
		case <-deadline:
			t.Fatal("jump verification timed out")
		}
	}
}

func TestMechanismsGrabExpiryAndSampledInput(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig(func(context.Context, Ref, gamephysics.Frame) error { return nil })
	s := newTestService(t, cfg)
	l := gamephysics.ObstacleLevel()
	ref, err := s.Create(ctx, Spec{Instance: s.instance, ID: "ugc", Bodies: l.Bodies, Motions: l.Motions, Joints: l.Joints, LifetimeSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.lookup(ref)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			r.mu.Lock()
			ok := check()
			r.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("condition not reached")
	}
	if err = s.Submit(ctx, member, Input{Sequence: 1, GrabTarget: 3}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return r.grabs[1] == 3 })
	wait(func() bool {
		for _, v := range r.states {
			if v.ID == 1 {
				return v.InputSequence == 1 && v.InputGeneration == member.InputGeneration && v.InputActive
			}
		}
		return false
	})
	wait(func() bool {
		if len(r.grabs) != 0 {
			return false
		}
		for _, v := range r.states {
			if v.ID == 1 {
				return v.InputSequence == 1 && !v.InputActive
			}
		}
		return false
	})
	if err = s.Submit(ctx, member, Input{Sequence: 2, GrabTarget: 100}); err != nil {
		t.Fatal(err)
	} // 静态地面不可抓。
	wait(func() bool {
		for _, v := range r.states {
			if v.ID == 1 {
				return v.InputSequence == 2
			}
		}
		return false
	})
	r.mu.Lock()
	count := len(r.grabs)
	r.mu.Unlock()
	if count != 0 {
		t.Fatal("static grab accepted")
	}
	if err = s.Leave(ctx, member); err != nil {
		t.Fatal(err)
	}
	next, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if next.InputGeneration == member.InputGeneration || next.InputGeneration == 0 {
		t.Fatal("input generation reused")
	}
	if err = s.Submit(ctx, next, Input{Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		for _, v := range r.states {
			if v.ID == 1 {
				return v.InputGeneration == next.InputGeneration && v.InputSequence == 1
			}
		}
		return false
	})
}

// 消费端也检查 jumpAt：慢提交后即使还有新移动，也不能执行已过期的跳跃。
func TestExpiredJumpIsNotConsumedAfterSlowCommit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cfg := testConfig()
	cfg.InputTTL = 50 * time.Millisecond
	cfg.Commit = func(_ context.Context, _ Ref, f gamephysics.Frame) error {
		if f.Tick == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	s := newTestService(t, cfg)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	spec := testSpec("slow-jump", s)
	spec.Bodies[1].Position[1] = .51
	ref, err := s.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Join(context.Background(), ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("commit did not block")
	}
	if err = s.Submit(context.Background(), member, Input{Sequence: 1, Jump: true}); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(2); seq <= 6; seq++ {
		time.Sleep(20 * time.Millisecond)
		if err = s.Submit(context.Background(), member, Input{Sequence: seq}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	r, _ := s.lookup(ref)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		var state physx.State
		for _, v := range r.states {
			if v.ID == 1 {
				state = v
			}
		}
		r.mu.Unlock()
		if state.InputSequence == 6 {
			if state.Velocity[1] > 1 {
				t.Fatalf("late jump executed: %+v", state)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("input not sampled")
}

func TestPrivateInputAckAndRetryStayWithTheirFrame(t *testing.T) {
	s := newTestService(t, testConfig())
	ctx := context.Background()
	spec := testSpec("private-ack", s)
	spec.Bodies = append(spec.Bodies, physx.Body{ID: 2, Shape: physx.Sphere, Position: physx.Vec3{2, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1})
	ref, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	binding.PlayerID = 2
	binding.SessionID = "session2"
	b, err := s.Join(ctx, ref, binding, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Submit(ctx, a, Input{Sequence: 11}); err != nil {
		t.Fatal(err)
	}
	if err = s.Submit(ctx, b, Input{Sequence: 22}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.lookup(ref)
	wait := func(seq uint64) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			r.mu.Lock()
			ready, otherReady := false, false
			for _, v := range r.states {
				if v.ID == 2 && v.InputSequence == 22 {
					otherReady = true
				}
				if v.ID == 1 && v.InputSequence == seq {
					ready = true
				}
			}
			r.mu.Unlock()
			if ready && otherReady {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("input not sampled")
	}
	wait(11)
	first, err := s.Poll(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Poll(ctx, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ra, rb gamephysics.RoomReplica
	if err = ra.Apply(first.Payload); err != nil {
		t.Fatal(err)
	}
	if err = rb.Apply(other.Payload); err != nil {
		t.Fatal(err)
	}
	if ra.Ack.Sequence != 11 || ra.Ack.Generation != a.InputGeneration || rb.Ack.Sequence != 22 || rb.Ack.Generation != b.InputGeneration {
		t.Fatalf("ack crossed recipients: %+v %+v", ra.Ack, rb.Ack)
	}
	if len(ra.States) != 2 || ra.States[2].InputSequence != 0 || ra.States[2].InputGeneration != 0 {
		t.Fatal("other player's confirmation leaked")
	}
	if err = s.Submit(ctx, a, Input{Sequence: 12}); err != nil {
		t.Fatal(err)
	}
	wait(12)
	retry, err := s.Poll(ctx, a, 0)
	if err != nil || !bytes.Equal(first.Payload, retry.Payload) {
		t.Fatal("pending frame got a newer input acknowledgement", err)
	}
}
