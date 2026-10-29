package physicsroom

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

type Service struct {
	instance string
	mu       sync.Mutex
	cfg      Config
	rooms    map[string]*room
	retired  map[string]retiredRoom
	stopping bool
	metrics  servicemetrics.Sink
}
type retiredRoom struct {
	ref   Ref
	until time.Time
}

type room struct {
	mu          sync.Mutex
	ref         Ref
	spec        Spec
	world       *physx.World
	cancel      context.CancelFunc
	done        chan struct{}
	stopped     bool
	members     map[int64]*memberState
	latest      *gamephysics.SpatialFrame
	replication *gamephysics.ReplicationScene
	// viewFrame 是最近一个已提交帧的只读索引；Poll 规划视图与 updateGrabs 校验距离共用它。
	viewFrame *gamephysics.ReplicationFrame
	// dynamic 是建房时固定的可控刚体集合，建房后只读，不随 Tick 重建。
	dynamic map[uint64]bool
	grabs   map[uint64]uint64
	// visibleObjects / viewObjects 是 Poll 规划视图的临时对象表，只在 r.mu 下使用。
	// Cursor.PlanFields 返回后它们不再被引用（游标与视图账本都复制对象值），
	// 清零后留给本房间下一次 Poll；Poll 串行持有 r.mu，不会两次规划同时使用。
	visibleObjects, viewObjects []chunksync.Object
}
type memberState struct {
	member                          Member
	binding                         gateway.Binding
	body                            uint64
	input                           Input
	jumpPending                     bool
	inputAt, jumpAt, seenAt, sentAt time.Time
	cursor                          chunksync.Cursor
	pending                         *chunksync.Plan
	pendingPayload                  []byte
	sequence                        uint64
	visible, nextVisible            map[uint64]bool
	view, nextView                  map[uint64]gamephysics.ViewEntry
	// spareVisible / spareView 是 ACK 时被替换下来的上一代视野与视图账本，
	// 只由本成员在 r.mu 下持有，下一次规划清空后复用；交给规划后即成为 next*。
	spareVisible map[uint64]bool
	spareView    map[uint64]gamephysics.ViewEntry
}

func New(cfg Config) (*Service, error) {
	// 所有资源上限在启动时检查，0 不代表无限容量。
	if err := cfg.Replication.Validate(); err != nil {
		return nil, err
	}
	switch {
	case cfg.MaxRooms < 1 || cfg.MaxRooms > 1024,
		cfg.MaxPlayers < 1 || cfg.MaxPlayers > 1024,
		cfg.MaxBodies < 1 || cfg.MaxBodies > 4096:
		return nil, ErrRequestInvalid
	case cfg.MaxFrameBytes < 256 || cfg.MaxFrameBytes > chunksync.MaxPayload:
		return nil, ErrRequestInvalid
	case cfg.Commit == nil,
		cfg.InputTTL <= 0 || cfg.InputTTL > time.Second,
		cfg.MemberTTL < cfg.InputTTL || cfg.MemberTTL > time.Minute,
		cfg.MaxLifetime < time.Second || cfg.MaxLifetime > 24*time.Hour:
		return nil, ErrRequestInvalid
	case cfg.Radius <= 0 || cfg.Margin < 0,
		math.IsNaN(cfg.Radius+cfg.Margin) || math.IsInf(cfg.Radius+cfg.Margin, 0),
		cfg.Radius+cfg.Margin > 100:
		return nil, ErrRequestInvalid
	}

	if physx.Version() == "unavailable" {
		return nil, physx.ErrUnavailable
	}
	return &Service{instance: token(), cfg: cfg, rooms: make(map[string]*room), retired: make(map[string]retiredRoom), metrics: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// Describe 返回 owner 进程代际，调用方保留在 Create 请求中，超时重试不重新获取。
func (s *Service) Describe(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return "", ErrStopping
	}
	return s.instance, nil
}

func epoch() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:]) | 1
}
func token() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

// 创建在准入锁内完成：并发开房不能越过容量，失败释放已分配资源。模拟不占此锁。
func (s *Service) Create(ctx context.Context, spec Spec) (Ref, error) {
	if err := fctx.BlockingError("physicsroom.Create"); err != nil {
		return Ref{}, err
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if spec.Instance != s.instance || spec.ID == "" || len(spec.ID) > 128 || len(spec.Bodies) == 0 || len(spec.Bodies) > s.cfg.MaxBodies || spec.LifetimeSeconds <= 0 || spec.LifetimeSeconds > int64(s.cfg.MaxLifetime/time.Second) {
		return Ref{}, ErrRequestInvalid
	}
	if err := gamephysics.ValidateMechanisms(spec.Bodies, spec.Motions, spec.Joints); err != nil {
		return Ref{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return Ref{}, ErrStopping
	}
	if r := s.rooms[spec.ID]; r != nil {
		if !reflect.DeepEqual(r.spec, spec) {
			return Ref{}, ErrRequestInvalid
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopped {
			return Ref{}, ErrStopping
		}
		s.metrics.Replayed("create")
		return r.ref, nil
	}
	now := time.Now()
	for id, until := range s.retired {
		if now.After(until.until) {
			delete(s.retired, id)
		}
	}
	if _, exists := s.retired[spec.ID]; exists {
		return Ref{}, ErrMissing
	}
	if len(s.retired) >= s.cfg.MaxRooms*16 {
		return Ref{}, ErrCapacity
	}
	if len(s.rooms) >= s.cfg.MaxRooms {
		s.metrics.Refused("create", "capacity")
		return Ref{}, ErrCapacity
	}
	w, err := physx.New(-9.81, 1)
	if err != nil {
		return Ref{}, err
	}
	for _, b := range spec.Bodies {
		if b.ID >= 1<<63 {
			w.Close()
			return Ref{}, ErrRequestInvalid
		}
		if err = w.Add(b); err != nil {
			w.Close()
			return Ref{}, err
		}
	}
	for _, j := range spec.Joints {
		if err = w.AddJoint(j); err != nil {
			w.Close()
			return Ref{}, err
		}
	}
	dynamic := 0
	for _, b := range spec.Bodies {
		if b.MotionType() != physx.Static {
			dynamic++
		}
	}
	if gamephysics.RoomHeaderBytes+36+dynamic*(22+1+gamephysics.QuantizedBodyBytes) > s.cfg.MaxFrameBytes {
		w.Close()
		return Ref{}, ErrCapacity
	}
	ref := Ref{ID: spec.ID, Epoch: epoch()}
	initial, err := gamephysics.NewQuantizedSpatialFrame(gamephysics.InitialFrame(ref.Epoch, spec.Bodies), 10)
	if err != nil {
		w.Close()
		return Ref{}, err
	}
	runCtx, cancel := context.WithTimeout(context.Background(), time.Duration(spec.LifetimeSeconds)*time.Second)
	spec.Bodies = slices.Clone(spec.Bodies)
	spec.Motions = slices.Clone(spec.Motions)
	spec.Joints = slices.Clone(spec.Joints)
	r := &room{ref: ref, spec: spec, world: w, cancel: cancel, done: make(chan struct{}), members: make(map[int64]*memberState), latest: initial, dynamic: make(map[uint64]bool), grabs: make(map[uint64]uint64)}
	for _, b := range spec.Bodies {
		if b.MotionType() == physx.Dynamic {
			r.dynamic[b.ID] = true
		}
	}
	r.replication = gamephysics.NewReplicationScene(spec.Bodies, spec.Motions)
	r.viewFrame = r.replication.Frame(gamephysics.InitialFrame(ref.Epoch, spec.Bodies).States)
	s.rooms[ref.ID] = r
	s.metrics.Accepted("create")
	s.metrics.Depth("rooms", int64(len(s.rooms)))
	go s.run(runCtx, r)
	return ref, nil
}
func (s *Service) lookup(ref Ref) (*room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, ErrStopping
	}
	r := s.rooms[ref.ID]
	if r == nil || r.ref != ref {
		return nil, ErrMissing
	}
	return r, nil
}
func (s *Service) run(ctx context.Context, r *room) {
	defer func() {
		r.mu.Lock()
		r.stopped = true
		r.members = nil
		r.mu.Unlock()
		r.world.Close()
		if s.cfg.Closed != nil {
			s.cfg.Closed(r.ref)
		}
		s.mu.Lock()
		if s.rooms[r.ref.ID] == r {
			delete(s.rooms, r.ref.ID)
			s.retired[r.ref.ID] = retiredRoom{ref: r.ref, until: time.Now().Add(s.cfg.MaxLifetime)}
		}
		s.metrics.Depth("rooms", int64(len(s.rooms)))
		s.mu.Unlock()
		close(r.done)
	}()
	defer r.cancel()
	sampled := make(map[uint64]uint64)
	generations := make(map[uint64]uint64)
	active := make(map[uint64]bool)
	err := gamephysics.Run(ctx, r.world, gamephysics.RunConfig{Epoch: r.ref.Epoch, Step: 20 * time.Millisecond, BeforeStep: func(uint64) error { return s.updateGrabs(r) }, Input: func(tick uint64) []physx.Command {
		r.mu.Lock()
		defer r.mu.Unlock()
		now := time.Now()
		clear(active)
		out := gamephysics.AppendMotionCommands(make([]physx.Command, 0, len(r.spec.Motions)+len(r.members)), r.spec.Bodies, r.spec.Motions, tick)
		for player, m := range r.members {
			if now.Sub(m.seenAt) > s.cfg.MemberTTL {
				delete(r.members, player)
				s.metrics.Dropped("member_expired", 1)
				continue
			}
			if !m.inputAt.IsZero() && now.Sub(m.inputAt) <= s.cfg.InputTTL {
				out = append(out, physx.Command{ID: m.body, Acceleration: m.input.Acceleration, Jump: m.jumpPending && now.Sub(m.jumpAt) <= s.cfg.InputTTL})
				m.jumpPending = false
				sampled[m.body] = m.input.Sequence
				generations[m.body] = m.member.InputGeneration
				active[m.body] = true
			}
		}
		// 稳定输入次序，不能让 map 遍历顺序成为物理重复运行的随机源。
		slices.SortFunc(out, func(a, b physx.Command) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		return out
	}}, func(ctx context.Context, f gamephysics.Frame) error {
		for i := range f.States {
			f.States[i].InputSequence = sampled[f.States[i].ID]
			f.States[i].InputGeneration = generations[f.States[i].ID]
			f.States[i].InputActive = active[f.States[i].ID]
		}
		if err := s.cfg.Commit(ctx, r.ref, f); err != nil {
			return err
		}
		frame, err := gamephysics.NewQuantizedSpatialFrame(f, 10)
		if err != nil {
			return err
		}
		// f.States 是本 Tick World.Step 新建的切片：Commit 已结束（DAO 复制了自己的一份），量化快照与
		// AOI 索引也已复制所需值，之后没有人再读写它，所以直接交给本帧的只读索引，不再复制。
		viewFrame := r.replication.AdoptFrame(f.States)
		r.mu.Lock()
		r.latest = frame
		r.viewFrame = viewFrame
		r.mu.Unlock()
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.metrics.Dropped("room_failed", 1)
		slog.Error("physicsroom: simulation or commit failed", "room", r.ref.ID, "epoch", r.ref.Epoch, "error", err)
	}
}
func (s *Service) Join(ctx context.Context, ref Ref, binding gateway.Binding, bodyID uint64) (Member, error) {
	if err := ctx.Err(); err != nil {
		return Member{}, err
	}
	if binding.Validate() != nil {
		return Member{}, ErrRequestInvalid
	}
	r, err := s.lookup(ref)
	if err != nil {
		return Member{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return Member{}, ErrStopping
	}
	if old := r.members[binding.PlayerID]; old != nil {
		if old.binding != binding || old.body != bodyID {
			return Member{}, ErrMember
		}
		old.seenAt = time.Now()
		return old.member, nil
	}
	if !r.dynamic[bodyID] {
		return Member{}, ErrRequestInvalid
	}
	for _, m := range r.members {
		if m.body == bodyID {
			return Member{}, ErrMember
		}
	}
	if len(r.members) >= s.cfg.MaxPlayers {
		return Member{}, ErrCapacity
	}
	m := Member{InputGeneration: epoch(), Room: ref, PlayerID: binding.PlayerID, Token: token()}
	r.members[m.PlayerID] = &memberState{member: m, binding: binding, body: bodyID, seenAt: time.Now()}
	s.metrics.Accepted("join")
	return m, nil
}
func (r *room) member(m Member) (*memberState, error) {
	if r.stopped {
		return nil, ErrStopping
	}
	v := r.members[m.PlayerID]
	if v == nil || v.member != m {
		return nil, ErrMember
	}
	return v, nil
}
func (s *Service) Submit(ctx context.Context, m Member, in Input) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	norm := float64(0)
	for _, v := range in.Acceleration {
		norm += float64(v) * float64(v)
	}
	if in.Acceleration[1] != 0 || in.Sequence == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) || norm > 400 {
		return ErrRequestInvalid
	}
	r, err := s.lookup(m.Room)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.member(m)
	if err != nil {
		return err
	}
	if in.Sequence == v.input.Sequence {
		if in != v.input {
			return ErrSequence
		}
		return nil
	}
	if in.Sequence < v.input.Sequence {
		return ErrSequence
	}
	if time.Since(v.jumpAt) > s.cfg.InputTTL {
		v.jumpPending = false
	}
	v.input = in
	if in.Jump {
		v.jumpPending = true
		v.jumpAt = time.Now()
	}
	v.inputAt = time.Now()
	v.seenAt = v.inputAt
	s.metrics.Accepted("input")
	return nil
}

// Poll 仅以 ack 推进游标；响应丢失会重发同一 pending。每人最多一帧、最多 20Hz 新帧。
func (s *Service) Poll(ctx context.Context, m Member, ack uint64) (Delivery, error) {
	if err := ctx.Err(); err != nil {
		return Delivery{}, err
	}
	r, err := s.lookup(m.Room)
	if err != nil {
		return Delivery{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.member(m)
	if err != nil {
		return Delivery{}, err
	}
	if v.pending != nil && ack == v.sequence+1 {
		if err = v.cursor.Commit(v.pending); err != nil {
			return Delivery{}, err
		}
		metrics.ObserveHistogram("physics.room.ack_wait", nil, time.Since(v.sentAt))
		metrics.IncCounter("physics.room.ack.total", nil, 1)
		v.spareView, v.view = v.view, v.nextView
		v.nextView = nil
		v.pending = nil
		v.pendingPayload = nil
		v.sequence = ack
		v.spareVisible, v.visible = v.visible, v.nextVisible
		v.nextVisible = nil
	} else if ack != v.sequence {
		return Delivery{}, ErrSequence
	}
	v.seenAt = time.Now()
	replay := v.pending != nil
	if v.pending == nil {
		if time.Since(v.sentAt) < 50*time.Millisecond {
			return Delivery{Room: r.ref, Sequence: v.sequence}, nil
		}
		snapshot, visible, err := r.latest.VisibleInto(r.visibleObjects, v.spareVisible, v.body, s.cfg.Radius, s.cfg.Margin, v.visible)
		if err != nil {
			return Delivery{}, err
		}
		// 备用 map 交给规划后即归本次结果所有；失败时随结果丢弃，下次重新分配。
		v.spareVisible = nil
		view, err := r.viewFrame.PlanInto(r.viewObjects, v.spareView, snapshot, v.body, r.grabs, v.view, s.cfg.Replication)
		v.spareView = nil
		if err != nil {
			return Delivery{}, err
		}
		p, err := v.cursor.PlanFields(view.Snapshot, gamephysics.QuantizedLayout())
		clear(snapshot.Objects) // 释放对载荷的引用，容量留给下一次 Poll。
		clear(view.Snapshot.Objects)
		r.visibleObjects, r.viewObjects = snapshot.Objects[:0], view.Snapshot.Objects[:0]
		if err != nil {
			return Delivery{}, err
		}
		v.pending = p
		v.nextView = view.Entries
		for class, count := range view.Samples {
			metrics.IncCounter("physics.room.view_changes.total", metrics.Labels{"class": class}, int64(count))
		}
		for class, count := range view.Deferred {
			metrics.IncCounter("physics.room.view_deferred.total", metrics.Labels{"class": class}, int64(count))
		}
		recordFrame(p.Data)
		var own gamephysics.InputAck
		if state, ok := r.viewFrame.State(v.body); ok && state.InputGeneration == v.member.InputGeneration {
			own = gamephysics.InputAck{Sequence: state.InputSequence, Generation: state.InputGeneration, Active: state.InputActive}
		}
		v.pendingPayload = gamephysics.RoomPayload(p.Data, own)
		v.nextVisible = visible
		v.sentAt = time.Now()
	}
	kind := "new"
	if replay {
		kind = "retry"
	}
	metrics.IncCounter("physics.room.payload_bytes.total", metrics.Labels{"kind": kind}, int64(len(v.pendingPayload)))
	return Delivery{Room: r.ref, Sequence: v.sequence + 1, Payload: slices.Clone(v.pendingPayload)}, nil
}
func (s *Service) Leave(ctx context.Context, m Member) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := s.lookup(m.Room)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.members[m.PlayerID] == nil {
		return nil
	}
	if _, err = r.member(m); err != nil {
		return err
	}
	delete(r.members, m.PlayerID)
	return nil
}
func (s *Service) Destroy(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fctx.BlockingError("physicsroom.Destroy"); err != nil {
		return err
	}
	r, err := s.lookup(ref)
	if err != nil {
		if errors.Is(err, ErrMissing) {
			s.mu.Lock()
			old, ok := s.retired[ref.ID]
			s.mu.Unlock()
			if ok && old.ref == ref {
				return nil
			}
		}
		return err
	}
	r.mu.Lock()
	r.stopped = true
	r.cancel()
	r.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown 超时只返回错误，后台仍等待真实提交退出后释放资源，不提前 Close。
func (s *Service) Shutdown(ctx context.Context) error {
	if err := fctx.BlockingError("physicsroom.Shutdown"); err != nil {
		return err
	}
	s.mu.Lock()
	s.stopping = true
	rooms := make([]*room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		r.stopped = true
		r.cancel()
		r.mu.Unlock()
	}
	for _, r := range rooms {
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
