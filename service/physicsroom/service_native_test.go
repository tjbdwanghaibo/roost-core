//go:build physx && cgo

package physicsroom

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func testConfig() Config {
	return Config{MaxRooms: 2, MaxPlayers: 2, MaxBodies: 10, MaxFrameBytes: 4096, InputTTL: 250 * time.Millisecond, MemberTTL: time.Second, MaxLifetime: time.Minute, Radius: 12, Margin: 2, Commit: func(context.Context, Ref, Tick) (Settled, error) { return Settled{}, nil }}
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
	cfg.Commit = func(context.Context, Ref, Tick) (Settled, error) {
		close(entered)
		<-release
		return Settled{}, errors.New("commit failed")
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
	cfg.Commit = func(_ context.Context, _ Ref, t Tick) (Settled, error) { frames <- t.Frame; return Settled{}, nil }
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
	cfg := DefaultConfig(func(context.Context, Ref, Tick) (Settled, error) { return Settled{}, nil })
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
		v, ok := r.viewFrame.State(1)
		return ok && v.InputSequence == 1 && v.InputGeneration == member.InputGeneration && v.InputActive
	})
	wait(func() bool {
		if len(r.grabs) != 0 {
			return false
		}
		v, ok := r.viewFrame.State(1)
		return ok && v.InputSequence == 1 && !v.InputActive
	})
	if err = s.Submit(ctx, member, Input{Sequence: 2, GrabTarget: 100}); err != nil {
		t.Fatal(err)
	} // 静态地面不可抓。
	wait(func() bool {
		v, ok := r.viewFrame.State(1)
		return ok && v.InputSequence == 2
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
		v, ok := r.viewFrame.State(1)
		return ok && v.InputGeneration == next.InputGeneration && v.InputSequence == 1
	})
}

// 消费端也检查 jumpAt：慢提交后即使还有新移动，也不能执行已过期的跳跃。
func TestExpiredJumpIsNotConsumedAfterSlowCommit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cfg := testConfig()
	cfg.InputTTL = 50 * time.Millisecond
	cfg.Commit = func(_ context.Context, _ Ref, t Tick) (Settled, error) {
		if t.Frame.Tick == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return Settled{}, nil
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
		state, _ := r.viewFrame.State(1)
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
			self, _ := r.viewFrame.State(1)
			other, _ := r.viewFrame.State(2)
			ready, otherReady := self.InputSequence == seq, other.InputSequence == 22
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

func TestReplicationViewAdvancesOnlyWithAcknowledgement(t *testing.T) {
	cfg := testConfig()
	cfg.Replication = gamephysics.DefaultReplicationPolicy()
	s := newTestService(t, cfg)
	ctx := context.Background()
	spec := testSpec("view-ack", s)
	spec.Bodies = append(spec.Bodies, physx.Body{ID: 2, Shape: physx.Sphere, Position: physx.Vec3{11, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1})
	ref, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Poll(ctx, m, 0)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.lookup(ref)
	r.mu.Lock()
	v := r.members[m.PlayerID]
	pendingTick := v.nextView[2].Tick
	uncommitted := len(v.view) == 0 && len(v.nextView) == 2
	r.mu.Unlock()
	if !uncommitted {
		t.Fatal("view committed before ACK")
	}
	retry, err := s.Poll(ctx, m, 0)
	if err != nil || !bytes.Equal(first.Payload, retry.Payload) {
		t.Fatal("retry changed", err)
	}
	_, err = s.Poll(ctx, m, first.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	committed := len(v.view) == 2 && v.view[2].Tick == pendingTick
	r.mu.Unlock()
	if !committed {
		t.Fatal("ACK failed to commit exact view baseline")
	}
	if err = s.Leave(ctx, m); err != nil {
		t.Fatal(err)
	}
	rejoined, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := s.Poll(ctx, rejoined, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Payload) < 24 || reset.Payload[23] != 1 {
		t.Fatal("rejoin lacks full frame")
	}
}

// ---- 关卡流程：动作执行器、业务动作与流程视图 ----

// scriptedCommit 是提交替身：记下房间交来的每个 Tick，并按脚本决定交回什么。脚本在房间执行 goroutine 上运行。
type scriptedCommit struct {
	mu    sync.Mutex
	ticks []Tick
	reply func(Tick) (Settled, error)
}

// once 让下一次提交交回 settled，只交一次；返回那次提交的 Tick 序号。
func (c *scriptedCommit) once(t *testing.T, settled Settled) uint64 {
	t.Helper()
	fired := make(chan uint64, 1)
	var used bool // 只在房间执行 goroutine 上读写
	c.mu.Lock()
	previous := c.reply
	c.reply = func(tick Tick) (Settled, error) {
		if !used {
			used = true
			fired <- tick.Frame.Tick
			return settled, nil
		}
		if previous != nil {
			return previous(tick)
		}
		return Settled{}, nil
	}
	c.mu.Unlock()
	select {
	case n := <-fired:
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("no commit arrived")
		return 0
	}
}

func (c *scriptedCommit) commit(_ context.Context, _ Ref, tick Tick) (Settled, error) {
	c.mu.Lock()
	c.ticks = append(c.ticks, tick)
	reply := c.reply
	c.mu.Unlock()
	if reply == nil {
		return Settled{}, nil
	}
	return reply(tick)
}

// tick 等到编号为 n 的 Tick 被提交并返回它。
func (c *scriptedCommit) tick(t *testing.T, n uint64) Tick {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if uint64(len(c.ticks)) >= n {
			tick := c.ticks[n-1]
			c.mu.Unlock()
			return tick
		}
		c.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("tick %d was never committed", n)
	return Tick{}
}
func (c *scriptedCommit) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ticks)
}
func (c *scriptedCommit) receipts() []level.Receipt {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []level.Receipt
	for _, tick := range c.ticks {
		out = append(out, tick.Receipts...)
	}
	return out
}

func stateOf(t *testing.T, tick Tick, id uint64) physx.State {
	t.Helper()
	for _, s := range tick.Frame.States {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("body %d missing from tick %d", id, tick.Frame.Tick)
	return physx.State{}
}

const testFlow = `{"Version":1,"Name":"t","Revision":1,"TimeDomain":"tick","Initial":["run"],"Stages":[{"ID":"run","Triggers":[` +
	`{"ID":"fall","Event":"region.enter","Actions":[{"Kind":"request","Name":"physics_respawn","Args":{"actor":{"EventField":"actor"}}}]}]}]}`

// flowSpec：地面、两个玩家球（1、2）、一个箱子球（3）、一个运动学平台（4）、一个用铰链挂着的杆（6）。
func flowSpec(id string, s *Service) Spec {
	return Spec{ID: id, Instance: s.instance, LifetimeSeconds: 30,
		Bodies: []physx.Body{
			{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{25, .5, 25}},
			{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, .5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1},
			{ID: 2, Shape: physx.Sphere, Position: physx.Vec3{1.5, .5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1},
			{ID: 3, Shape: physx.Sphere, Position: physx.Vec3{0, .5, 1.5}, Dimensions: physx.Vec3{.5}, Mass: 1},
			{ID: 4, Shape: physx.Box, Motion: physx.Kinematic, Position: physx.Vec3{12, 1, 12}, Dimensions: physx.Vec3{1, .25, 1}, Mass: 10},
			{ID: 6, Shape: physx.Box, Motion: physx.Dynamic, Position: physx.Vec3{-12, 3, 0}, Dimensions: physx.Vec3{.3, 2, .3}, Mass: 10},
		},
		Joints:  []physx.Joint{{ID: 1, Kind: physx.Hinge, A: 6, AnchorA: physx.Vec3{0, 2, 0}, AnchorB: physx.Vec3{-12, 5, 0}, BreakForce: 10000}},
		Players: []uint64{1, 2},
		Regions: []gamephysics.Region{{ID: "pit", Center: physx.Vec3{0, -6, 0}, HalfExtents: physx.Vec3{60, 2, 60}}},
		Spawns: []gamephysics.Spawn{
			{ID: "a", Positions: []physx.Vec3{{6, .5, 6}, {8, .5, 6}}},
			{ID: "b", Positions: []physx.Vec3{{-6, .5, -6}}, Rotation: physx.Quat{0, 1, 0, 0}},
		},
		DefaultSpawn: "a", Flow: []byte(testFlow)}
}

func flowConfig(commit Commit) Config {
	cfg := testConfig()
	cfg.MaxFrameBytes = 16 << 10
	cfg.Commit = commit
	return cfg
}

// effect 造一个流程会交回的待执行动作。
func effect(sequence uint64, name string, args map[string]level.Value) level.Effect {
	return level.Effect{ID: "room:1:" + strconv.FormatUint(sequence, 10), Instance: "room", Epoch: 1, Sequence: sequence, Stage: "run", Generation: 1, Name: name, Args: args, Status: "pending"}
}
func teleportTo(sequence, actor uint64, spawn string) level.Effect {
	return effect(sequence, gamephysics.ActionTeleport, map[string]level.Value{"actor": level.Int(int64(actor)), "spawn": level.String(spawn)})
}
func near(got, want physx.Vec3, tolerance float32) bool {
	for i := range 3 {
		if d := got[i] - want[i]; d > tolerance || d < -tolerance {
			return false
		}
	}
	return true
}

// 动作只在提交成功之后执行，并且是在下一个 Tick：那一帧从出生点出发，回执与那一帧在同一次提交里。
// 提交失败时，即使替身同时交回了动作，房间也直接停止，不执行它。
func TestLevelActionsRunOnlyAfterTheFrameIsSettled(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	action := teleportTo(1, 1, "b")
	script.reply = func(tick Tick) (Settled, error) {
		if tick.Frame.Tick == 5 {
			return Settled{Actions: []level.Effect{action}}, nil
		}
		return Settled{}, nil
	}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics = metrics
	s := newTestService(t, cfg)
	if _, err := s.Create(ctx, flowSpec("settled", s)); err != nil {
		t.Fatal(err)
	}
	first := script.tick(t, 1)
	if first.Program == nil {
		t.Fatal("first tick of a room with a flow must carry the compiled program")
	}
	for n := uint64(2); n <= 5; n++ {
		tick := script.tick(t, n)
		if tick.Program != nil || len(tick.Receipts) != 0 {
			t.Fatalf("tick %d: program %v receipts %v", n, tick.Program != nil, tick.Receipts)
		}
		if body := stateOf(t, tick, 1); !near(body.Position, physx.Vec3{0, .5, 0}, .05) || body.Teleports != 0 {
			t.Fatalf("tick %d: body moved before the action was settled: %+v", n, body)
		}
	}
	// 第 6 帧：动作在 Step 之前执行，所以这一帧就是从出生点 b 出发、速度清零后走了一步的结果。
	moved := script.tick(t, 6)
	body := stateOf(t, moved, 1)
	if !near(body.Position, physx.Vec3{-6, .5, -6}, .05) || body.Teleports != 1 || !near(body.Velocity, physx.Vec3{}, .5) {
		t.Fatalf("tick 6 does not start from the spawn: %+v", body)
	}
	if body.Rotation[1] < .99 {
		t.Fatalf("spawn rotation not applied: %v", body.Rotation)
	}
	want := level.Receipt{Instance: action.Instance, Epoch: action.Epoch, ID: action.ID, Success: true}
	if len(moved.Receipts) != 1 || moved.Receipts[0] != want {
		t.Fatalf("receipt must be committed with the frame that shows the teleport: %+v", moved.Receipts)
	}
	if other := stateOf(t, moved, 2); other.Teleports != 0 || !near(other.Position, physx.Vec3{1.5, .5, 0}, .05) {
		t.Fatalf("another body was affected: %+v", other)
	}
	// 之后不再有回执，计数不再增加。
	for n := uint64(7); n <= 12; n++ {
		if tick := script.tick(t, n); len(tick.Receipts) != 0 || stateOf(t, tick, 1).Teleports != 1 {
			t.Fatalf("tick %d: action repeated: %+v", n, tick.Receipts)
		}
	}
	if metrics.Count("accepted:action") != 1 {
		t.Fatal(metrics.Events())
	}

	// 提交失败：交回的动作不执行，房间停止，不再有下一次提交。
	for _, c := range []struct {
		name   string
		err    error
		metric string
	}{
		{"commit failed", errors.New("commit failed"), "dropped:room_failed"},
		{"flow failed", errcode.Wrap(ErrLevel, errors.New("budget")), "dropped:level_failed"},
	} {
		failing := &scriptedCommit{}
		failing.reply = func(tick Tick) (Settled, error) {
			if tick.Frame.Tick == 3 {
				return Settled{Actions: []level.Effect{teleportTo(1, 1, "b")}}, c.err
			}
			return Settled{}, nil
		}
		record := servicemetrics.NewRecorder()
		cfg := flowConfig(failing.commit)
		cfg.Metrics = record
		failed := newTestService(t, cfg)
		ref, err := failed.Create(ctx, flowSpec("failing", failed))
		if err != nil {
			t.Fatal(err)
		}
		failing.tick(t, 3)
		// 房间停止后从表里摘掉：等到查不到它为止。
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err = failed.lookup(ref); errors.Is(err, ErrMissing) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: room kept running", c.name)
			}
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(60 * time.Millisecond)
		if n := failing.count(); n != 3 {
			t.Fatalf("%s: %d ticks committed after the failure", c.name, n-3)
		}
		if record.Count(c.metric) != 1 || record.Count("accepted:action") != 0 {
			t.Fatalf("%s: %s", c.name, record.Events())
		}
	}
}

// 流程每次交回的是全部待执行动作：同一个动作在回执提交之前又出现在列表里（包括回执正随那一帧提交的那一次），
// 只传送一次、只有一条回执。
func TestRepeatedPendingActionTeleportsOnce(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	action := teleportTo(1, 1, "b")
	other := teleportTo(2, 3, "a")
	script.reply = func(tick Tick) (Settled, error) {
		switch tick.Frame.Tick {
		case 4, 5: // 4：首次交回；5：回执正随这一帧提交，列表里仍带着它
			return Settled{Actions: []level.Effect{action}}, nil
		case 7:
			return Settled{Actions: []level.Effect{other}}, nil
		}
		return Settled{}, nil
	}
	s := newTestService(t, flowConfig(script.commit))
	if _, err := s.Create(ctx, flowSpec("repeat", s)); err != nil {
		t.Fatal(err)
	}
	last := script.tick(t, 14)
	if got := stateOf(t, last, 1).Teleports; got != 1 {
		t.Fatalf("body teleported %d times", got)
	}
	var ids []string
	for _, r := range script.receipts() {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{action.ID, other.ID}) {
		t.Fatalf("receipts %v", ids)
	}
	// 不是玩家的刚体用出生点的第一个位置。
	if crate := stateOf(t, last, 3); crate.Teleports != 1 || !near(crate.Position, physx.Vec3{6, .5, 6}, .1) {
		t.Fatalf("second action: %+v", crate)
	}
}

// 被传送的刚体连着的抓取弹簧在那个 Tick 的 Step 之前释放，并且不按传送前的位置重新抓上；
// 传送前按下的跳跃不带到新位置。
func TestTeleportReleasesGrabsAndWaitsForTheFrame(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		moved  uint64
		target physx.Vec3
	}{{"grabbed body", 3, physx.Vec3{-6, .5, -6}}, {"grabber", 1, physx.Vec3{-6, .5, -6}}} {
		script := &scriptedCommit{}
		cfg := flowConfig(script.commit)
		cfg.InputTTL = time.Second
		s := newTestService(t, cfg)
		ref, err := s.Create(ctx, flowSpec("grab", s))
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
		grabs := func() map[uint64]uint64 {
			r.mu.Lock()
			defer r.mu.Unlock()
			return maps.Clone(r.grabs)
		}
		if err = s.Submit(ctx, member, Input{Sequence: 1, GrabTarget: 3}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { return grabs()[1] == 3 })
		// 记下每次提交时（那一帧的 Step 之后）的抓取表：提交回调在房间执行 goroutine 上。
		var mu sync.Mutex
		seen := map[uint64]map[uint64]uint64{}
		script.mu.Lock()
		script.reply = func(tick Tick) (Settled, error) {
			mu.Lock()
			seen[tick.Frame.Tick] = grabs()
			mu.Unlock()
			return Settled{}, nil
		}
		script.mu.Unlock()
		at := script.once(t, Settled{Actions: []level.Effect{teleportTo(1, c.moved, "b")}})
		after := script.tick(t, at+4)
		mu.Lock()
		for n := at + 1; n <= at+3; n++ {
			if len(seen[n]) != 0 {
				t.Fatalf("%s: tick %d (teleport at %d) still holds %v", c.name, n, at+1, seen[n])
			}
		}
		mu.Unlock()
		if got := stateOf(t, after, c.moved); !near(got.Position, c.target, .1) {
			t.Fatalf("%s: dragged away from the spawn: %+v", c.name, got)
		}
		stayed, home := uint64(1), physx.Vec3{0, .5, 0}
		if c.moved == 1 {
			stayed, home = 3, physx.Vec3{0, .5, 1.5}
		}
		if got := stateOf(t, after, stayed); !near(got.Position, home, .2) {
			t.Fatalf("%s: the other end was dragged: %+v", c.name, got)
		}
		s.Shutdown(ctx)
	}

	// 跳跃：提交卡住期间按下跳跃，同一次提交交回传送。下一个 Tick 在出生点落地，不能起跳。
	script := &scriptedCommit{}
	cfg := flowConfig(script.commit)
	cfg.InputTTL = time.Second
	s := newTestService(t, cfg)
	ref, err := s.Create(ctx, flowSpec("jump", s))
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Submit(ctx, member, Input{Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	script.tick(t, 20) // 球已经落稳
	entered, release := make(chan uint64), make(chan struct{})
	var hold sync.Once
	script.mu.Lock()
	script.reply = func(tick Tick) (Settled, error) {
		settled := Settled{}
		hold.Do(func() {
			entered <- tick.Frame.Tick
			<-release
			settled.Actions = []level.Effect{teleportTo(1, 1, "b")}
		})
		return settled, nil
	}
	script.mu.Unlock()
	at := <-entered
	if err = s.Submit(ctx, member, Input{Sequence: 2, Jump: true}); err != nil {
		t.Fatal(err)
	}
	close(release)
	for n := at + 1; n <= at+6; n++ {
		if got := stateOf(t, script.tick(t, n), 1); got.Velocity[1] > .5 || !near(got.Position, physx.Vec3{-6, .5, -6}, .1) {
			t.Fatalf("tick %d: the jump pressed before the teleport fired at the spawn: %+v", n, got)
		}
	}
}

// 目标不合法是动作的失败：回执 Success=false，世界不变，房间继续。检查点与重生只对玩家有效。
func TestInvalidTeleportCompletesAsFailure(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics = metrics
	s := newTestService(t, cfg)
	if _, err := s.Create(ctx, flowSpec("invalid", s)); err != nil {
		t.Fatal(err)
	}
	actor := func(sequence uint64, name string, id int64) level.Effect {
		return effect(sequence, name, map[string]level.Value{"actor": level.Int(id)})
	}
	spawn := func(sequence uint64, name string, id int64, spawn string) level.Effect {
		return effect(sequence, name, map[string]level.Value{"actor": level.Int(id), "spawn": level.String(spawn)})
	}
	refused := []level.Effect{
		spawn(1, gamephysics.ActionTeleport, 6, "a"),    // 被配置的铰链连着
		spawn(2, gamephysics.ActionTeleport, 4, "a"),    // 运动学平台
		spawn(3, gamephysics.ActionTeleport, 100, "a"),  // 静态地面
		spawn(4, gamephysics.ActionTeleport, 999, "a"),  // 不存在
		spawn(5, gamephysics.ActionTeleport, -1, "a"),   // 负数
		spawn(6, gamephysics.ActionTeleport, 1, "gone"), // 出生点不存在
		spawn(7, gamephysics.ActionCheckpoint, 3, "b"),  // 箱子不是玩家
		spawn(8, gamephysics.ActionCheckpoint, 1, "gone"),
		actor(9, gamephysics.ActionRespawn, 3),
	}
	before := script.tick(t, script.once(t, Settled{Actions: refused}))
	after := script.tick(t, before.Frame.Tick+1)
	if len(after.Receipts) != len(refused) {
		t.Fatalf("receipts %+v", after.Receipts)
	}
	for i, r := range after.Receipts {
		if r.ID != refused[i].ID || r.Success {
			t.Fatalf("receipt %d = %+v, want the failure of %s in sequence order", i, r, refused[i].ID)
		}
	}
	for _, id := range []uint64{1, 2, 3, 4, 6} {
		a, b := stateOf(t, before, id), stateOf(t, after, id)
		if b.Teleports != 0 || !near(a.Position, b.Position, .05) {
			t.Fatalf("body %d moved by a refused action: %+v -> %+v", id, a, b)
		}
	}
	for reason, want := range map[string]int{"jointed": 1, "target": 4, "spawn": 2, "not_player": 2} {
		if got := metrics.Count("refused:action:" + reason); got != want {
			t.Fatalf("%s=%d want %d: %s", reason, got, want, metrics.Events())
		}
	}

	// 检查点只记不动；重生回到最近的检查点，没有检查点的玩家回缺省出生点里自己的位置。
	at := script.once(t, Settled{Actions: []level.Effect{spawn(10, gamephysics.ActionCheckpoint, 1, "b")}})
	marked := script.tick(t, at+1)
	if len(marked.Receipts) != 1 || !marked.Receipts[0].Success {
		t.Fatalf("checkpoint receipt %+v", marked.Receipts)
	}
	if got := stateOf(t, marked, 1); got.Teleports != 0 || !near(got.Position, physx.Vec3{0, .5, 0}, .05) {
		t.Fatalf("checkpoint moved the player: %+v", got)
	}
	at = script.once(t, Settled{Actions: []level.Effect{actor(11, gamephysics.ActionRespawn, 1), actor(12, gamephysics.ActionRespawn, 2)}})
	respawned := script.tick(t, at+1)
	if len(respawned.Receipts) != 2 || !respawned.Receipts[0].Success || !respawned.Receipts[1].Success {
		t.Fatalf("respawn receipts %+v", respawned.Receipts)
	}
	if got := stateOf(t, respawned, 1); got.Teleports != 1 || !near(got.Position, physx.Vec3{-6, .5, -6}, .05) {
		t.Fatalf("player 1 must return to its checkpoint b: %+v", got)
	}
	if got := stateOf(t, respawned, 2); got.Teleports != 1 || !near(got.Position, physx.Vec3{8, .5, 6}, .05) {
		t.Fatalf("player 2 must take its own slot of the default spawn: %+v", got)
	}
	if metrics.Count("accepted:action") != 3 || metrics.Count("dropped:room_failed") != 0 {
		t.Fatal(metrics.Events())
	}
}

// 动作已经被流程取消时回执作废：只记指标，房间继续。
func TestCancelledActionReceiptDoesNotStopTheRoom(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics = metrics
	s := newTestService(t, cfg)
	if _, err := s.Create(ctx, flowSpec("stale", s)); err != nil {
		t.Fatal(err)
	}
	at := script.once(t, Settled{StaleReceipts: 2})
	script.tick(t, at+5)
	if metrics.Count("dropped:receipt_stale") != 2 || metrics.Count("dropped:room_failed") != 0 || metrics.Count("dropped:level_failed") != 0 {
		t.Fatal(metrics.Events())
	}
}

// businessActions 是业务动作的替身。
type businessActions struct {
	mu           sync.Mutex
	calls        map[string]int
	active, peak int
	run          func(ctx context.Context, e level.Effect, attempt int) (bool, error)
}

func (b *businessActions) Registry() level.Registry {
	return level.Registry{Actions: map[string]level.ActionSpec{"grant": {Version: 1, Args: map[string]level.ValueKind{"actor": level.Integer}}}}
}
func (b *businessActions) Execute(ctx context.Context, _ Ref, e level.Effect) (bool, error) {
	b.mu.Lock()
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[e.ID]++
	attempt := b.calls[e.ID]
	b.active++
	b.peak = max(b.peak, b.active)
	run := b.run
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.active--
		b.mu.Unlock()
	}()
	return run(ctx, e, attempt)
}
func (b *businessActions) count(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}
func grant(sequence uint64) level.Effect {
	return effect(sequence, "grant", map[string]level.Value{"actor": level.Int(1)})
}

// 业务动作在自己的 goroutine 上执行：卡住的 Execute 不拖慢物理，同时在途的不超过上限，
// 停房时取消在途调用并等它们返回。
func TestBusinessActionsNeverBlockThePhysicsLoop(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	release := make(chan struct{})
	started := make(chan string, 16)
	actions := &businessActions{run: func(ctx context.Context, e level.Effect, _ int) (bool, error) {
		started <- e.ID
		select {
		case <-release:
			return e.Sequence%2 == 1, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics, cfg.Actions, cfg.ActionConcurrency = metrics, actions, 2
	s := newTestService(t, cfg)
	ref, err := s.Create(ctx, flowSpec("business", s))
	if err != nil {
		t.Fatal(err)
	}
	pending := []level.Effect{grant(1), grant(2), grant(3), grant(4), grant(5)}
	at := script.once(t, Settled{Actions: pending})
	first, second := <-started, <-started
	if first == second {
		t.Fatal("one action started twice")
	}
	// Execute 卡着的这段时间里物理照常走：50Hz 下 20 帧是 0.4s，给到 1s。
	begin := time.Now()
	script.tick(t, at+20)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("20 ticks took %v while Execute was blocked", elapsed)
	}
	// 流程每次状态变化都会把仍在途的动作再交回一遍：不重复派发。
	script.once(t, Settled{Actions: pending})
	script.tick(t, script.once(t, Settled{Actions: pending})+3)
	select {
	case id := <-started:
		t.Fatalf("%s started beyond the concurrency limit of 2", id)
	default:
	}
	if got := metrics.Count("depth:actions_inflight"); got != 2 {
		t.Fatalf("inflight gauge %d", got)
	}
	close(release)
	eventually(t, func() bool { return len(script.receipts()) == len(pending) })
	results := map[string]bool{}
	for _, r := range script.receipts() {
		if _, repeated := results[r.ID]; repeated {
			t.Fatalf("receipt of %s committed twice", r.ID)
		}
		results[r.ID] = r.Success
	}
	for _, e := range pending {
		if success, ok := results[e.ID]; !ok || success != (e.Sequence%2 == 1) || actions.count(e.ID) != 1 {
			t.Fatalf("%s: receipt %v/%v after %d calls", e.ID, success, ok, actions.count(e.ID))
		}
	}
	actions.mu.Lock()
	peak := actions.peak
	actions.mu.Unlock()
	if peak != 2 {
		t.Fatalf("peak concurrency %d, want exactly the limit 2", peak)
	}
	eventually(t, func() bool { return metrics.Count("depth:actions_inflight") == 0 })

	// 停房：在途的被取消并等到返回，没派发的也计入未了结。
	blocked := make(chan struct{})
	returned := make(chan struct{}, 4)
	actions.mu.Lock()
	actions.run = func(ctx context.Context, _ level.Effect, _ int) (bool, error) {
		blocked <- struct{}{}
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond) // 模拟收尾：Destroy 必须等它
		returned <- struct{}{}
		return false, ctx.Err()
	}
	actions.mu.Unlock()
	script.once(t, Settled{Actions: []level.Effect{grant(6), grant(7), grant(8)}})
	<-blocked
	<-blocked
	if err = s.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if len(returned) != 2 {
		t.Fatalf("Destroy returned while %d of 2 business calls were still running", 2-len(returned))
	}
	if got := metrics.Count("dropped:action_unsettled"); got != 3 {
		t.Fatalf("unsettled=%d want 3 (2 in flight, 1 never dispatched): %s", got, metrics.Events())
	}
	if metrics.Count("depth:actions_inflight") != 0 {
		t.Fatal(metrics.Events())
	}
}

// 超时、返回错误、panic 都表示结果未知：不给失败回执，带着同一个动作 ID 重试，直到拿到明确结果。
func TestUnknownBusinessResultIsRetriedWithTheSameID(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	var deadlines []time.Duration
	var mu sync.Mutex
	actions := &businessActions{run: func(ctx context.Context, e level.Effect, attempt int) (bool, error) {
		if e.Args["actor"].Int != 1 {
			return false, errors.New("arguments damaged between attempts")
		}
		e.Args["actor"] = level.Int(99) // 改自己拿到的参数不能影响下一次重试
		switch attempt {
		case 1:
			return false, errors.New("game unreachable")
		case 2:
			panic("handler bug")
		case 3:
			deadline, _ := ctx.Deadline()
			mu.Lock()
			deadlines = append(deadlines, time.Until(deadline))
			mu.Unlock()
			<-ctx.Done() // 超时
			return true, ctx.Err()
		}
		return true, nil
	}}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics, cfg.Actions, cfg.ActionTimeout = metrics, actions, 80*time.Millisecond
	s := newTestService(t, cfg)
	if _, err := s.Create(ctx, flowSpec("retry", s)); err != nil {
		t.Fatal(err)
	}
	action := grant(1)
	script.once(t, Settled{Actions: []level.Effect{action}})
	eventually(t, func() bool { return actions.count(action.ID) >= 3 })
	if got := script.receipts(); len(got) != 0 {
		t.Fatalf("an unknown result was reported to the flow: %+v", got)
	}
	eventually(t, func() bool { return len(script.receipts()) == 1 })
	want := level.Receipt{Instance: action.Instance, Epoch: action.Epoch, ID: action.ID, Success: true}
	if got := script.receipts(); got[0] != want || actions.count(action.ID) != 4 {
		t.Fatalf("receipt %+v after %d calls", got, actions.count(action.ID))
	}
	mu.Lock()
	if len(deadlines) != 1 || deadlines[0] <= 0 || deadlines[0] > 80*time.Millisecond {
		t.Fatalf("Execute deadline %v, want the configured 80ms", deadlines)
	}
	mu.Unlock()
	if metrics.Count("refused:action:retry") != 3 || metrics.Count("accepted:action") != 1 || metrics.Count("dropped:room_failed") != 0 {
		t.Fatal(metrics.Events())
	}
}

// 区域与成员事实：准备阶段攒着，开始后按序投递；每次提交最多 64 条，提交成功才算交付；结束后不再投递。
func TestLevelFactsReachTheFlowInOrder(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	s := newTestService(t, flowConfig(script.commit))
	spec := flowSpec("facts", s)
	// 7 号球在地面之外，直接掉下去：先进 drop，再出 drop。
	spec.Bodies = append(spec.Bodies, physx.Body{ID: 7, Shape: physx.Sphere, Position: physx.Vec3{40, 1, 0}, Dimensions: physx.Vec3{.5}, Mass: 1})
	spec.Regions = append(spec.Regions, gamephysics.Region{ID: "drop", Center: physx.Vec3{40, -3, 0}, HalfExtents: physx.Vec3{2, 1, 2}, Watch: []uint64{7}})
	ref, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	view := func(revision uint64, phase level.Phase) Settled {
		return Settled{View: &level.View{Revision: revision, Phase: phase}}
	}
	script.once(t, view(1, level.Preparing))
	// 准备阶段：50 次加入 / 离开，共 100 条成员事实。
	for range 50 {
		member, err := s.Join(ctx, ref, testBinding(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Leave(ctx, member); err != nil {
			t.Fatal(err)
		}
	}
	// 等 7 号球掉出 drop 区域（y < -4.25）。
	eventually(t, func() bool {
		script.mu.Lock()
		defer script.mu.Unlock()
		last := script.ticks[len(script.ticks)-1]
		for _, state := range last.Frame.States {
			if state.ID == 7 {
				return state.Position[1] < -6
			}
		}
		return false
	})
	script.mu.Lock()
	for _, tick := range script.ticks {
		if len(tick.Facts) != 0 {
			script.mu.Unlock()
			t.Fatalf("tick %d delivered facts before the flow was running: %+v", tick.Frame.Tick, tick.Facts)
		}
	}
	script.mu.Unlock()
	start := script.once(t, view(2, level.Running))
	a, b, c := script.tick(t, start+1), script.tick(t, start+2), script.tick(t, start+3)
	if len(a.Facts) != 64 || len(b.Facts) != 38 || len(c.Facts) != 0 {
		t.Fatalf("facts per tick %d/%d/%d, want 64/38/0", len(a.Facts), len(b.Facts), len(c.Facts))
	}
	facts := append(slices.Clone(a.Facts), b.Facts...)
	instance := LevelInstance(ref)
	for i, fact := range facts {
		if fact.Sequence != uint64(i+1) || fact.Source != FactSource || fact.Instance != instance || fact.Epoch != ref.Epoch {
			t.Fatalf("fact %d: %+v", i, fact)
		}
		want := gamephysics.EventMemberLeave
		if i%2 == 0 {
			want = gamephysics.EventMemberJoin
		}
		if i < 100 && (fact.Kind != want || fact.Fields["actor"] != level.Int(1)) {
			t.Fatalf("fact %d: %+v, want %s of body 1", i, fact, want)
		}
	}
	for i, kind := range []string{gamephysics.EventRegionEnter, gamephysics.EventRegionLeave} {
		fact := facts[100+i]
		if fact.Kind != kind || fact.Fields["actor"] != level.Int(7) || fact.Fields["region"] != level.String("drop") {
			t.Fatalf("region fact %d: %+v", i, fact)
		}
	}
	// 结束之后的事实丢弃。
	script.once(t, view(3, level.Finishing))
	member, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Leave(ctx, member); err != nil {
		t.Fatal(err)
	}
	end := script.count()
	script.tick(t, uint64(end)+5)
	script.mu.Lock()
	for _, tick := range script.ticks[end:] {
		if len(tick.Facts) != 0 {
			t.Errorf("tick %d delivered %+v after the flow finished", tick.Frame.Tick, tick.Facts)
		}
	}
	script.mu.Unlock()
}

// 事实攒到上限还交不出去时房间停止，记为流程失败。
func TestLevelFactBacklogStopsTheRoom(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics = metrics
	s := newTestService(t, cfg)
	ref, err := s.Create(ctx, flowSpec("backlog", s))
	if err != nil {
		t.Fatal(err)
	}
	script.once(t, Settled{View: &level.View{Revision: 1, Phase: level.Preparing}})
	for range maxFactBacklog/2 + 1 {
		member, err := s.Join(ctx, ref, testBinding(), 1)
		if errors.Is(err, ErrMissing) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Leave(ctx, member); err != nil && !errors.Is(err, ErrMissing) {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return metrics.Count("dropped:level_failed") == 1 })
	eventually(t, func() bool { _, err := s.lookup(ref); return errors.Is(err, ErrMissing) })

	// 流程结束之后事实直接丢弃、不再攒：同样多的进出，房间照常运行。
	finishedScript := &scriptedCommit{}
	quiet := servicemetrics.NewRecorder()
	cfg = flowConfig(finishedScript.commit)
	cfg.Metrics = quiet
	done := newTestService(t, cfg)
	if ref, err = done.Create(ctx, flowSpec("finished", done)); err != nil {
		t.Fatal(err)
	}
	finishedScript.tick(t, finishedScript.once(t, Settled{View: &level.View{Revision: 2, Phase: level.Finishing}})+1)
	for range maxFactBacklog/2 + 1 {
		member, err := done.Join(ctx, ref, testBinding(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = done.Leave(ctx, member); err != nil {
			t.Fatal(err)
		}
	}
	finishedScript.tick(t, uint64(finishedScript.count())+5)
	if _, err = done.lookup(ref); err != nil || quiet.Count("dropped:level_failed") != 0 {
		t.Fatalf("room stopped by facts produced after the flow finished: %v %s", err, quiet.Events())
	}
}

// 成员超时被清掉也是离开：流程收到 member.leave。
func TestExpiredMemberLeavesTheFlow(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	script.reply = func(tick Tick) (Settled, error) {
		if tick.Frame.Tick == 1 {
			return Settled{View: &level.View{Revision: 1, Phase: level.Running}}, nil
		}
		return Settled{}, nil
	}
	cfg := flowConfig(script.commit)
	cfg.InputTTL, cfg.MemberTTL = 50*time.Millisecond, 150*time.Millisecond
	s := newTestService(t, cfg)
	ref, err := s.Create(ctx, flowSpec("expiry", s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Join(ctx, ref, testBinding(), 2); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	eventually(t, func() bool {
		kinds = kinds[:0]
		script.mu.Lock()
		defer script.mu.Unlock()
		for _, tick := range script.ticks {
			for _, fact := range tick.Facts {
				if fact.Fields["actor"] == level.Int(2) {
					kinds = append(kinds, fact.Kind)
				}
			}
		}
		return len(kinds) >= 2
	})
	if !slices.Equal(kinds, []string{gamephysics.EventMemberJoin, gamephysics.EventMemberLeave}) {
		t.Fatalf("facts of the expired member: %v", kinds)
	}
}

// 流程视图搭帧下发：每个版本每个成员一次，未确认时原样重发，后加入的成员拿到最新的一份。
func TestPollCarriesLevelViewOncePerRevision(t *testing.T) {
	ctx := context.Background()
	script := &scriptedCommit{}
	s := newTestService(t, flowConfig(script.commit))
	ref, err := s.Create(ctx, flowSpec("view", s))
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	poll := func(m Member, ack uint64) Delivery {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			d, err := s.Poll(ctx, m, ack)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Payload) != 0 {
				return d
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no frame")
		return Delivery{}
	}
	first := poll(member, 0)
	if len(first.Level) != 0 {
		t.Fatalf("level view before the flow reported one: %x", first.Level)
	}
	publish := func(v level.View) {
		t.Helper()
		script.tick(t, script.once(t, Settled{View: &v})+1)
	}
	publish(level.View{Revision: 3, Phase: level.Running, Stages: []level.StageView{{ID: "run", Generation: 1}}, Variables: map[string]level.Value{"laps": level.Int(2)}})
	second := poll(member, first.Sequence)
	got, err := DecodeLevelView(second.Level)
	if err != nil || got.Revision != 3 || got.Phase != level.Running || got.Variables["laps"] != level.Int(2) || len(got.Stages) != 1 {
		t.Fatalf("view %+v %v", got, err)
	}
	// 没确认：重发同一份（同一帧、同一份视图），即使流程又有了新版本。
	publish(level.View{Revision: 4, Phase: level.Running})
	again, err := s.Poll(ctx, member, first.Sequence)
	if err != nil || again.Sequence != second.Sequence || !bytes.Equal(again.Level, second.Level) || !bytes.Equal(again.Payload, second.Payload) {
		t.Fatalf("unacknowledged delivery changed: %v", err)
	}
	// 确认之后拿到版本 4，再确认之后不再带。
	third := poll(member, second.Sequence)
	if got, err = DecodeLevelView(third.Level); err != nil || got.Revision != 4 {
		t.Fatalf("view %+v %v", got, err)
	}
	for ack := third.Sequence; ack < third.Sequence+3; ack++ {
		if d := poll(member, ack); len(d.Level) != 0 {
			t.Fatalf("revision 4 delivered again at sequence %d", d.Sequence)
		}
	}
	// 后加入的成员第一帧就带最新的一份。
	other := testBinding()
	other.PlayerID, other.SessionID = 8, "session-8"
	late, err := s.Join(ctx, ref, other, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = DecodeLevelView(poll(late, 0).Level); err != nil || got.Revision != 4 {
		t.Fatalf("late joiner view %+v %v", got, err)
	}
	// 视图编码超限按流程出错处理。
	metrics := servicemetrics.NewRecorder()
	cfg := flowConfig(script.commit)
	cfg.Metrics = metrics
	limited := newTestService(t, cfg)
	if _, err = limited.Create(ctx, flowSpec("big", limited)); err != nil {
		t.Fatal(err)
	}
	huge := map[string]level.Value{}
	for i := range 64 {
		huge["k"+strconv.Itoa(i)] = level.String(strings.Repeat("x", 300))
	}
	script.mu.Lock()
	script.reply = func(Tick) (Settled, error) {
		return Settled{View: &level.View{Revision: 9, Phase: level.Running, Variables: huge}}, nil
	}
	script.mu.Unlock()
	eventually(t, func() bool { return metrics.Count("dropped:level_failed") == 1 })
}

// 建房时校验与编译流程：玩家、区域、出生点、流程定义、业务能力任何一项不对都拒绝。
func TestCreateRejectsInvalidLevelSpecs(t *testing.T) {
	ctx := context.Background()
	commit := func(context.Context, Ref, Tick) (Settled, error) { return Settled{}, nil }
	s := newTestService(t, flowConfig(commit))
	business := `{"Version":1,"Name":"t","Revision":1,"TimeDomain":"tick","Initial":["run"],"Stages":[{"ID":"run","Triggers":[` +
		`{"ID":"pay","Event":"region.enter","Actions":[{"Kind":"request","Name":"grant","Args":{"actor":{"EventField":"actor"}}}]}]}]}`
	for name, change := range map[string]func(*Spec){
		"player is not a body":  func(sp *Spec) { sp.Players = []uint64{1, 55} },
		"player repeated":       func(sp *Spec) { sp.Players = []uint64{1, 1} },
		"player is kinematic":   func(sp *Spec) { sp.Players = []uint64{1, 4} },
		"default spawn unknown": func(sp *Spec) { sp.DefaultSpawn = "nowhere" },
		"flow is not JSON":      func(sp *Spec) { sp.Flow = []byte("{") },
		"flow uses wall clock":  func(sp *Spec) { sp.Flow = []byte(strings.Replace(testFlow, `"tick"`, `"wall"`, 1)) },
		"flow names an unknown spawn": func(sp *Spec) {
			sp.Flow = []byte(strings.Replace(testFlow, `"physics_respawn","Args":{"actor":{"EventField":"actor"}}`, `"physics_teleport","Args":{"actor":{"EventField":"actor"},"spawn":{"Literal":{"Kind":"string","Text":"nowhere"}}}`, 1))
		},
		"business action undeclared": func(sp *Spec) { sp.Flow = []byte(business) },
	} {
		spec := flowSpec("bad", s)
		change(&spec)
		if _, err := s.Create(ctx, spec); !errors.Is(err, ErrRequestInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 对照：同一份流程把出生点换成存在的 a 就能建，说明上面那条是因为出生点名字被拒绝的。
	known := flowSpec("known-spawn", s)
	known.Flow = []byte(strings.Replace(testFlow, `"physics_respawn","Args":{"actor":{"EventField":"actor"}}`, `"physics_teleport","Args":{"actor":{"EventField":"actor"},"spawn":{"Literal":{"Kind":"string","Text":"a"}}}`, 1))
	if _, err := s.Create(ctx, known); err != nil {
		t.Fatalf("flow teleporting to a known spawn rejected: %v", err)
	}
	// 登记了业务能力就能建；业务能力与物理房间自带的重名时拒绝。
	withActions := flowConfig(commit)
	withActions.Actions = &businessActions{}
	declared := newTestService(t, withActions)
	spec := flowSpec("business", declared)
	spec.Flow = []byte(business)
	if _, err := declared.Create(ctx, spec); err != nil {
		t.Fatalf("declared business action rejected: %v", err)
	}
	clash := flowConfig(commit)
	clash.Actions = clashingActions{}
	clashing := newTestService(t, clash)
	if _, err := clashing.Create(ctx, flowSpec("clash", clashing)); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("business registry redefining a physics action: %v", err)
	}
	// 没有流程的房间不需要这些字段，照常运行，提交里不带 Program、事实与回执。
	script := &scriptedCommit{}
	plain := newTestService(t, flowConfig(script.commit))
	bare := flowSpec("plain", plain)
	bare.Flow = nil
	ref, err := plain.Create(ctx, bare)
	if err != nil {
		t.Fatal(err)
	}
	member, err := plain.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = plain.Leave(ctx, member); err != nil {
		t.Fatal(err)
	}
	for n := uint64(1); n <= 5; n++ {
		if tick := script.tick(t, n); tick.Program != nil || len(tick.Facts) != 0 || len(tick.Receipts) != 0 {
			t.Fatalf("room without a flow committed level data: %+v", tick)
		}
	}
}

type clashingActions struct{}

func (clashingActions) Registry() level.Registry {
	return level.Registry{Actions: map[string]level.ActionSpec{gamephysics.ActionTeleport: {Version: 1}}}
}
func (clashingActions) Execute(context.Context, Ref, level.Effect) (bool, error) { return true, nil }

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
