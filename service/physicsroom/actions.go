package physicsroom

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// 关卡流程在物理房间里的执行器。流程本身（LevelComponent）挂在房间 Entity 上，由 Commit 的实现在 Nest Guard
// 里推进；这里只做 Guard 之外的三件事，全部在房间执行 goroutine 上：
//
//   - 把已提交的物理帧变成事实（谁进出了哪个区域、谁加入或离开），随下一次提交交给流程；
//   - 提交成功后取回流程的待执行动作：传送、检查点、重生在下一个 Tick 的 Step 之前执行，
//     回执随那个 Tick 的帧一起提交；
//   - 业务动作交给 Config.Actions，在独立 goroutine 上执行，结果随之后某个 Tick 提交。
//
// 流程只在 Running 阶段接收事实：准备阶段（PrepareDelay）里的事实攒着，开始后按序补投；结束之后的丢弃。

// FactSource 是房间投给流程的全部事实的来源名。
const FactSource = "physics"

const (
	// 每次提交最多交给流程 64 条事实，超出的按序留到之后的 Tick。攒到 4096 条仍然交不完时房间停止：
	// 那说明事实产生得持续比流程处理得快，继续攒只会让流程越来越落后于物理世界。
	maxFactsPerTick = 64
	maxFactBacklog  = 4096

	defaultActionTimeout     = 5 * time.Second
	defaultActionConcurrency = 16
	actionRetryMin           = 100 * time.Millisecond
	actionRetryMax           = 5 * time.Second
)

// LevelInstance 是房间流程的实例名：房间 ID 不一定符合流程标识符的规则，所以取它的 SHA-256 前 8 字节。
// 流程的 Epoch 就是 Ref.Epoch，动作 ID（实例名:Epoch:序号）因此在不同房间、同一房间的不同代际之间都不重复。
func LevelInstance(ref Ref) string {
	sum := sha256.Sum256([]byte(ref.ID))
	return "room-" + hex.EncodeToString(sum[:8])
}

type memberFact struct {
	body uint64
	join bool
}

// flow 是一个房间的流程执行状态。没有标注的字段只由房间执行 goroutine 读写。
type flow struct {
	program      *level.Program
	instance     string
	tracker      *gamephysics.RegionTracker
	spawns       map[string]gamephysics.Spawn
	defaultSpawn string
	slots        map[uint64]int  // 玩家刚体在 Spec.Players 里的下标
	jointed      map[uint64]bool // 被 Spec.Joints 连着的刚体，不能传送

	phase     level.Phase
	occupancy gamephysics.RegionOccupancy
	nextFact  uint64
	backlog   []level.Event

	// known 是已经接手、回执还没提交的动作 ID：流程每次交回的是全部待执行动作，靠它保证每个动作只接手一次。
	known       map[string]struct{}
	queue       []level.Effect    // 下一个 Tick 的 Step 之前要执行的物理动作
	receipts    []level.Receipt   // 已执行的物理动作的回执，随本 Tick 的帧提交
	waiting     []level.Effect    // 还没有派发的业务动作
	checkpoints map[uint64]string // 玩家刚体最近一次记下的出生点；只由已提交的动作写
	teleports   map[uint64]uint32 // 刚体累计被传送的次数，写进 State.Teleports
	teleported  map[uint64]bool   // 本 Tick 被传送过的刚体

	mu       sync.Mutex // 保护 results 与 inflight：业务动作的 goroutine 也会写
	results  []level.Receipt
	inflight int
	workers  sync.WaitGroup
}

func finished(p level.Phase) bool {
	return p == level.Finishing || p == level.Closing || p == level.Closed
}

// newFlow 校验并编译 Spec 里与流程有关的字段；没有 Flow 时返回 nil。错误一律是 ErrRequestInvalid。
func newFlow(spec Spec, actions ActionHandler) (*flow, error) {
	invalid := func(err error) error { return errcode.Wrap(ErrRequestInvalid, err) }
	dynamic := make(map[uint64]bool, len(spec.Bodies))
	for _, b := range spec.Bodies {
		dynamic[b.ID] = b.MotionType() == physx.Dynamic
	}
	slots := make(map[uint64]int, len(spec.Players))
	for i, id := range spec.Players {
		if _, repeated := slots[id]; repeated || !dynamic[id] {
			return nil, invalid(fmt.Errorf("physicsroom: player body %d is missing, repeated or not dynamic", id))
		}
		slots[id] = i
	}
	if err := gamephysics.ValidateRegions(spec.Bodies, spec.Players, spec.Regions, spec.Spawns, spec.DefaultSpawn); err != nil {
		return nil, invalid(err)
	}
	if len(spec.Flow) == 0 {
		return nil, nil
	}
	var business level.Registry
	if actions != nil {
		business = actions.Registry()
	}
	program, err := gamephysics.CompileFlow(spec.Flow, spec.Regions, spec.Spawns, business)
	if err != nil {
		return nil, invalid(err)
	}
	f := &flow{program: program, tracker: gamephysics.NewRegionTracker(spec.Regions, spec.Players), spawns: make(map[string]gamephysics.Spawn, len(spec.Spawns)),
		defaultSpawn: spec.DefaultSpawn, slots: slots, jointed: make(map[uint64]bool), known: make(map[string]struct{}),
		checkpoints: make(map[uint64]string), teleports: make(map[uint64]uint32), teleported: make(map[uint64]bool)}
	for _, s := range spec.Spawns {
		f.spawns[s.ID] = s
	}
	for _, j := range spec.Joints {
		f.jointed[j.A], f.jointed[j.B] = true, true
	}
	return f, nil
}

// runActions 在 Step 之前执行上一次提交交回的物理动作，按动作序号的次序。每个动作恰好执行一次并得到一条回执；
// 目标不合法是动作的失败（回执 Success=false，流程自己决定怎么办），原生调用出错则返回错误、房间停止。
func (s *Service) runActions(r *room) error {
	f := r.flow
	clear(f.teleported)
	if len(f.queue) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range f.queue {
		reason, err := s.runAction(r, e)
		if err != nil {
			return err
		}
		if reason == "" {
			s.metrics.Accepted("action")
		} else {
			s.metrics.Refused("action", reason)
		}
		f.receipts = append(f.receipts, level.Receipt{Instance: e.Instance, Epoch: e.Epoch, ID: e.ID, Success: reason == ""})
	}
	clear(f.queue)
	f.queue = f.queue[:0]
	return nil
}

// runAction 执行一个物理动作；调用方持有 r.mu。返回的 reason 为空表示成功，否则是失败原因（用作指标标签）。
func (s *Service) runAction(r *room, e level.Effect) (reason string, err error) {
	f := r.flow
	actor := e.Args["actor"].Int
	if actor < 0 {
		return "target", nil
	}
	body := uint64(actor)
	_, player := f.slots[body]
	spawn := e.Args["spawn"].Text
	switch e.Name {
	case gamephysics.ActionCheckpoint:
		if !player {
			return "not_player", nil
		}
		if _, ok := f.spawns[spawn]; !ok {
			return "spawn", nil
		}
		f.checkpoints[body] = spawn
		return "", nil
	case gamephysics.ActionRespawn:
		if !player {
			return "not_player", nil
		}
		if spawn = f.checkpoints[body]; spawn == "" {
			spawn = f.defaultSpawn
		}
	}
	target, ok := f.spawns[spawn]
	switch {
	case !ok:
		return "spawn", nil
	case !r.dynamic[body]:
		return "target", nil
	case f.jointed[body]:
		return "jointed", nil
	}
	// 抓取弹簧不在这里拆：紧接着的 updateGrabs（同一个 BeforeStep，Step 之前）不会把本 Tick 被传送的刚体
	// 算作想抓的一对，于是它作为抓取者或被抓者的弹簧都在那里释放，也不会按传送前的位置重新抓上。
	for _, m := range r.members {
		if m.body == body {
			m.jumpPending = false // 传送前按下的跳跃不带到新位置
		}
	}
	rotation := target.Rotation
	if rotation == (physx.Quat{}) {
		rotation = physx.Quat{0, 0, 0, 1}
	}
	// 多个玩家传到同一个出生点时按各自在 Players 里的下标错开位置；不是玩家的刚体用第一个位置。
	position := target.Positions[f.slots[body]%len(target.Positions)]
	if err = r.world.SetPose(body, position, rotation, physx.Vec3{}, physx.Vec3{}); err != nil {
		return "", err
	}
	f.teleports[body]++
	f.teleported[body] = true
	return "", nil
}

// flowTick 组出这一帧要提交的 Tick：首帧带上 Program，然后是事实与回执。occupancy 是这一帧之后的区域占用，
// 提交成功后才由 flowSettled 采纳。current 是这一帧的只读索引，r.viewFrame 此时还是上一个已提交帧。
func (s *Service) flowTick(r *room, frame gamephysics.Frame, current *gamephysics.ReplicationFrame) (Tick, gamephysics.RegionOccupancy, error) {
	f := r.flow
	tick := Tick{Frame: frame}
	if frame.Tick == 1 {
		tick.Program = f.program
	}
	r.mu.Lock()
	members := r.memberFacts
	r.memberFacts = nil
	r.mu.Unlock()
	occupancy := f.occupancy
	if finished(f.phase) {
		f.backlog = nil
	} else {
		// 次序固定：先成员事实（按刚体 ID，同一刚体保持发生的先后），再区域事实（区域按关卡顺序、刚体按 ID）。
		slices.SortStableFunc(members, func(a, b memberFact) int { return cmp.Compare(a.body, b.body) })
		for _, m := range members {
			kind := gamephysics.EventMemberLeave
			if m.join {
				kind = gamephysics.EventMemberJoin
			}
			f.fact(r.ref, kind, map[string]level.Value{"actor": level.Int(int64(m.body))})
		}
		var regions []gamephysics.RegionFact
		regions, occupancy = f.tracker.Step(f.occupancy, r.viewFrame, current, f.teleported)
		for _, fact := range regions {
			kind := gamephysics.EventRegionLeave
			if fact.Enter {
				kind = gamephysics.EventRegionEnter
			}
			f.fact(r.ref, kind, map[string]level.Value{"actor": level.Int(int64(fact.Actor)), "region": level.String(fact.Region)})
		}
		if len(f.backlog) > maxFactBacklog {
			return Tick{}, nil, errcode.Wrap(ErrLevel, errors.New("physicsroom: level facts outpace the flow"))
		}
		if f.phase == level.Running {
			tick.Facts = slices.Clone(f.backlog[:min(len(f.backlog), maxFactsPerTick)])
		}
	}
	f.mu.Lock()
	f.receipts = append(f.receipts, f.results...)
	f.results = f.results[:0]
	f.mu.Unlock()
	tick.Receipts = slices.Clone(f.receipts)
	return tick, occupancy, nil
}

func (f *flow) fact(ref Ref, kind string, fields map[string]level.Value) {
	f.nextFact++
	f.backlog = append(f.backlog, level.Event{Instance: f.instance, Epoch: ref.Epoch, Source: FactSource, Sequence: f.nextFact, Kind: kind, Fields: fields})
}

// flowSettled 在 Commit 成功返回之后调用：这时才把这一帧的事实与回执记为已交付，并接手流程交回的动作。
// 返回流程公开状态的新编码（没有变化时为 nil），由调用方在 r.mu 下换上。
func (s *Service) flowSettled(ctx context.Context, r *room, tick Tick, occupancy gamephysics.RegionOccupancy, settled Settled) (view []byte, revision uint64, err error) {
	f := r.flow
	f.backlog = f.backlog[len(tick.Facts):]
	if len(f.backlog) == 0 {
		f.backlog = nil // 不让已经交付的事实留在底层数组里
	}
	f.receipts = f.receipts[:0]
	f.occupancy = occupancy
	s.metrics.Dropped("receipt_stale", settled.StaleReceipts)
	if settled.View != nil {
		f.phase = settled.View.Phase
		if view, err = EncodeLevelView(*settled.View); err != nil {
			return nil, 0, errcode.Wrap(ErrLevel, err)
		}
		revision = settled.View.Revision
	}
	for _, e := range settled.Actions {
		if _, taken := f.known[e.ID]; taken {
			continue
		}
		f.known[e.ID] = struct{}{}
		switch e.Name {
		case gamephysics.ActionTeleport, gamephysics.ActionCheckpoint, gamephysics.ActionRespawn:
			f.queue = append(f.queue, e)
		default:
			f.waiting = append(f.waiting, e)
		}
	}
	// 回执已经随这一帧提交：这些动作从此不再待执行，忘掉它们的 ID。放在接手之后做，
	// 这样即使这次交回的列表里还带着刚提交回执的动作，也不会被当成新动作再执行一遍。
	for _, receipt := range tick.Receipts {
		delete(f.known, receipt.ID)
	}
	s.dispatch(ctx, r)
	return view, revision, nil
}

// dispatch 把等待中的业务动作派发出去，同时在途的不超过上限。只在房间执行 goroutine 上调用：
// 一个动作结束后空出来的名额在下一个 Tick 补上。
func (s *Service) dispatch(ctx context.Context, r *room) {
	f := r.flow
	if len(f.waiting) == 0 {
		return
	}
	limit := s.cfg.ActionConcurrency
	if limit <= 0 {
		limit = defaultActionConcurrency
	}
	f.mu.Lock()
	free := limit - f.inflight
	started := min(free, len(f.waiting))
	if started < 0 {
		started = 0
	}
	f.inflight += started
	f.mu.Unlock()
	for _, e := range f.waiting[:started] {
		f.workers.Add(1)
		go s.execute(ctx, r, e)
	}
	f.waiting = slices.Delete(f.waiting, 0, started)
	s.actionsInflight(started)
}

func (s *Service) actionsInflight(delta int) {
	s.inflightMu.Lock()
	s.inflight += int64(delta)
	s.metrics.Depth("actions_inflight", s.inflight)
	s.inflightMu.Unlock()
}

// execute 在自己的 goroutine 上执行一个业务动作，直到拿到明确结果或房间结束。
// 超时、返回错误、panic 都表示结果未知：不给失败回执，带着同一个动作 ID 退避后再试。
func (s *Service) execute(ctx context.Context, r *room, e level.Effect) {
	f := r.flow
	defer f.workers.Done()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
		s.actionsInflight(-1)
	}()
	timeout := s.cfg.ActionTimeout
	if timeout <= 0 {
		timeout = defaultActionTimeout
	}
	for wait := actionRetryMin; ; wait = min(wait*2, actionRetryMax) {
		success, err := s.executeOnce(ctx, r.ref, e, timeout)
		if err == nil {
			f.mu.Lock()
			f.results = append(f.results, level.Receipt{Instance: e.Instance, Epoch: e.Epoch, ID: e.ID, Success: success})
			f.mu.Unlock()
			s.metrics.Accepted("action")
			return
		}
		retry := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			retry.Stop()
			// 房间结束时结果仍然未知：业务账本按动作 ID 了结，这里只留下记录。
			s.metrics.Dropped("action_unsettled", 1)
			slog.Warn("physicsroom: business action left unsettled by room end", "room", r.ref.ID, "epoch", r.ref.Epoch, "action", e.Name, "id", e.ID, "error", err)
			return
		case <-retry.C:
			s.metrics.Refused("action", "retry")
		}
	}
}

func (s *Service) executeOnce(ctx context.Context, ref Ref, e level.Effect, timeout time.Duration) (success bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			success, err = false, fmt.Errorf("physicsroom: action handler panicked: %v", p)
		}
	}()
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	e.Args = maps.Clone(e.Args) // 每次调用给一份自己的参数表
	return s.cfg.Actions.Execute(call, ref, e)
}
