// Package movement 是实体在地图上的位置与移动（DESIGN-TODO D20 第 2 期）：
//
//   - MapBasic：所在场景、位置、朝向，以及与 AOI 的接线——进场景 Enter，位置变化 QueueMove（事务内入队、走提交门，
//     回滚的移动不生效），离开 Leave；落库由业务选（周期 / 离开场景时，经 MapConfig.Save）。
//   - Move：速度、路径或跟随目标、状态；被动组件，只提供设定与推进接口（MoveTo / Follow / Advance / Stop），
//     自己不挂心跳。
//   - MoveTo / Follow 两个 actionflow 动作：移动由动作驱动（参考 ssr：action 挂心跳、在心跳里调 move）。动作所在组
//     由实体的 actionflow.BeatRunner 挂心跳（缺省 100ms），每拍 Tick 调 Move.Advance(Beat.Elapsed)（不追帧），
//     到达、被打断、被阻挡、目标消失由 actionflow 收尾（RegisterActions）。所有控制实体行为的逻辑都经动作，不绕过
//     动作直接驱动组件（维护者 2026-10-21）。
//
// 三者都只在实体锁内调用（Nest handler、心跳回调，或加载 / 新建的初始化阶段），只做内存计算与入队，不阻塞、不做
// I/O。状态的权威在业务实体的 DAO（Fields）：组件不另存一份位置；Move 自己的推进状态（亚格精度的位置、路径进度）
// 随事务回滚恢复。ActionRunner 不随事务回滚：handler 最后一步才调用动作接口（见 actionflow 包说明）。
package movement

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

var (
	// ErrNotInScene 表示实体不在任何场景里（没有 Enter，或已 Leave）。
	ErrNotInScene = errors.New("movement: entity is not in a scene")
	// ErrAlreadyInScene 表示实体已在场景里：换场景先 Leave 再 Enter。
	ErrAlreadyInScene = errors.New("movement: entity is already in a scene")
)

// Facing 是朝向，单位 1/100 度，取值 [0, 36000)：0 指向 +X，按 atan2(dy, dx) 方向递增（+Y 为 9000）。
type Facing int32

// FacingOf 返回从 (0, 0) 指向 (dx, dy) 的朝向；(0, 0) 返回 0。
func FacingOf(dx, dy float64) Facing {
	if dx == 0 && dy == 0 {
		return 0
	}
	deg := math.Atan2(dy, dx) * 18000 / math.Pi
	f := Facing(math.Round(deg))
	if f < 0 {
		f += 36000
	}
	if f >= 36000 {
		f -= 36000
	}
	return f
}

// Fields 是 MapBasic 的状态所在：业务实体 DAO 的字段。场景 ID 是否持久由业务定；位置与朝向应声明为 nopersist、
// 参与同步（D20 决定 ⑮：位置不每拍落库，按周期或离开场景时经 MapConfig.Save 落库一次；朝向同一规则）。
//
// 实现写 DAO 并标同步脏；生成的 setter 在事务里登记逆操作，回滚随 DAO 恢复。只在实体锁内调用。
type Fields interface {
	SceneID() int64
	SetSceneID(int64)
	Pos() spatial.Point
	SetPos(spatial.Point)
	Facing() Facing
	SetFacing(Facing)
}

// Space 是 MapBasic 与 AOI 的接线（场景的兴趣政策）。policy.Interest 经 InterestSpace 适配；业务场景也可以自己实现
// （例如同时登记同步 subject、处理玩家会话）。
//
// QueueMove 在实体锁与事务内调用：只记事实、走提交门，提交确认后才生效、回滚丢弃（policy.Interest.QueueMove）。
// Enter / Leave 在事务提交、实体锁释放之后调用（nest.AfterCommit），回滚的进出场景不发生；它们在 worker 上执行，
// 不能阻塞、不能做 I/O。不在事务里（加载 / 新建的初始化阶段）三者都立即调用。
type Space interface {
	Enter(e entity.IThreadSafeEntity, at spatial.Point) error
	QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error
	Leave(e entity.IThreadSafeEntity) error
}

// InterestSpace 把 policy.Interest 适配成 Space：Player 认出的实体按玩家进出（Interest.Enter / Leave：既看又被看），
// 其余实体只被看（Interest.Show / Hide）。Interest 的这些入口只登记（短锁、不做 AOI），由 Manager 的政策阶段或
// Interest 的应用线程按调用顺序应用并说给 Manager；被拒的订阅由 Interest 自己重说（要记日志用 InterestConfig.OnRefusal）。
// 实体的同步 subject 与玩家会话由业务在进场景之前登记 / 打开。
type InterestSpace struct {
	Interest *policy.Interest
	// Player 返回实体对应的玩家（观察者）ID 与主视野；ok 为 false 时实体只被看。nil 表示全部只被看。
	Player func(e entity.IThreadSafeEntity) (playerID int64, view policy.ObserverView, ok bool)
}

func (s InterestSpace) Enter(e entity.IThreadSafeEntity, at spatial.Point) error {
	var err error
	if playerID, view, ok := s.player(e); ok {
		err = s.Interest.Enter(playerID, e.ID(), at, view)
	} else {
		err = s.Interest.Show(e.ID(), at)
	}
	return err
}

func (s InterestSpace) QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error {
	return s.Interest.QueueMove(e, at)
}

func (s InterestSpace) Leave(e entity.IThreadSafeEntity) error {
	var err error
	if playerID, _, ok := s.player(e); ok {
		err = s.Interest.Leave(playerID, e.ID())
	} else {
		err = s.Interest.Hide(e.ID())
	}
	return err
}

func (s InterestSpace) player(e entity.IThreadSafeEntity) (int64, policy.ObserverView, bool) {
	if s.Player == nil {
		return 0, policy.ObserverView{}, false
	}
	return s.Player(e)
}

// Snapshot 是 MapBasic 最近一次提交的状态，供其他实体（Follow 的跟随者）在任意 goroutine 无锁读取。
type Snapshot struct {
	// Present 为 false 表示实例已销毁（卸载、删档）或从未发布。
	Present bool
	SceneID int64
	Pos     spatial.Point
	Facing  Facing
}

// MapConfig 是 NewMapBasic 的输入。
type MapConfig struct {
	// Fields 是状态所在（业务 DAO 的字段）。必填。
	Fields Fields
	// Save 把当前场景、位置与朝向写进业务 DAO 的持久字段，在实体锁与事务内调用（离开场景时、周期落库时）。
	// nil 表示不落库（位置只同步）。
	Save func(sceneID int64, at spatial.Point, facing Facing) error
	// SaveOnLeave 为 true 时 Leave 先 Save 一次（场景、位置或朝向变过才写）。
	SaveOnLeave bool
	// SaveEvery 非 0 时在场景里每隔这么久 Save 一次（场景、位置或朝向变过才写），必须是 heartbeat.Periods 之一。周期落库是心跳
	// 登记 "map.save"，持久级别按 SaveOptions（缺省 async）。
	SaveEvery   time.Duration
	SaveOptions []heartbeat.RegOption
	// OnSpaceError 收到提交后 Enter / Leave 的失败（事务已提交、场景字段已改，AOI 没有跟上）；nil 打 Warn。
	OnSpaceError func(op string, err error)
}

// MapBasic 是实体在地图上的位置组件。它的方法只能在实体锁内调用，Published 除外。
type MapBasic struct {
	owner  entity.IThreadSafeEntity
	fields Fields
	config MapConfig

	saveReg *heartbeat.Reg

	// 以下只在实体锁内读写，随事务回滚恢复（mapState 整体快照）。
	st mapState
	// pending 是一个事务里攒下的提交后工作（AOI 进出与发布），按事务 ID 识别；之前事务留下的记录不再改动。
	pending *mapCommit
	// rbTx 是最近一次登记了回滚恢复的事务。
	rbTx nest.TransactionID
	seq  uint64

	published atomic.Pointer[publishedSnapshot]
}

type mapState struct {
	// space 非 nil 表示业务视角在场景里。
	space Space
	// dirty 表示场景、位置或朝向自上次 Save 以来变过。
	dirty bool
}

type mapCommit struct {
	tx   nest.TransactionID
	ops  []spaceOp
	snap Snapshot
	seq  uint64
}

type spaceOp struct {
	enter bool
	space Space
	at    spatial.Point
}

type publishedSnapshot struct {
	seq  uint64
	snap Snapshot
}

const finalSeq = math.MaxUint64

// MapOwner 由带 MapBasic 的实体实现，LocateIn 据此找到跟随目标的位置。
type MapOwner interface {
	MapBasic() *MapBasic
}

// NewMapBasic 构造组件；在实体工厂或 OnInitFinish 里调用（实体锁内或发布之前）。host 是实体的心跳宿主，只在
// SaveEvery 非 0 时用来登记周期落库。实体当前的状态（例如加载时的场景字段）原样作为第一份发布；加载后要回到场景，
// 由业务按自己的场景重新 Enter（组件不持有场景对象，也不持久化“在场景里”）。
func NewMapBasic(owner entity.IThreadSafeEntity, host *heartbeat.Host, config MapConfig) (*MapBasic, error) {
	if owner == nil || config.Fields == nil {
		return nil, errors.New("movement: map basic needs an owner and fields")
	}
	m := &MapBasic{owner: owner, fields: config.Fields, config: config}
	if config.SaveEvery != 0 {
		if host == nil {
			return nil, errors.New("movement: periodic save needs a heartbeat host")
		}
		reg, err := host.Register("map.save", config.SaveEvery, m.saveBeat, config.SaveOptions...)
		if err != nil {
			return nil, err
		}
		m.saveReg = reg
	}
	m.seq++
	m.publish(m.seq, Snapshot{Present: true, SceneID: config.Fields.SceneID(), Pos: config.Fields.Pos(), Facing: config.Fields.Facing()})
	return m, nil
}

// Owner 返回组件所属的实体。
func (m *MapBasic) Owner() entity.IThreadSafeEntity { return m.owner }

// SceneID 是所在场景，0 表示不在场景里（读 Fields）。
func (m *MapBasic) SceneID() int64 { return m.fields.SceneID() }

// Pos 是当前位置（读 Fields）。
func (m *MapBasic) Pos() spatial.Point { return m.fields.Pos() }

// Facing 是当前朝向（读 Fields）。
func (m *MapBasic) Facing() Facing { return m.fields.Facing() }

// InScene 报告业务视角是否在场景里（Enter 之后、Leave 之前，含本事务里尚未提交的进出）。
func (m *MapBasic) InScene() bool { return m.st.space != nil }

// Space 返回所在场景的 AOI 接线；不在场景里时为 nil。
func (m *MapBasic) Space() Space { return m.st.space }

// Enter 进入场景：写场景 ID 与位置，提交后 space.Enter（位置取本事务结束时的位置），配置了周期落库时开始周期落库。
// 已在场景里时返回 ErrAlreadyInScene；配置了周期落库而 Remote 托管实体处于共享模式时返回
// heartbeat.ErrHeartbeatRemoteShared（共享模式不允许挂心跳），两者都什么都不改。
func (m *MapBasic) Enter(sceneID int64, space Space, at spatial.Point) error {
	if sceneID == 0 || space == nil {
		return fmt.Errorf("movement: enter needs a scene id and a space (scene %d)", sceneID)
	}
	if m.st.space != nil {
		return ErrAlreadyInScene
	}
	if m.saveReg != nil {
		if err := m.saveReg.Start(); err != nil {
			return err
		}
	}
	m.guard()
	m.st.space, m.st.dirty = space, true
	m.fields.SetSceneID(sceneID)
	m.fields.SetPos(at)
	m.addOp(spaceOp{enter: true, space: space, at: at})
	return nil
}

// Leave 离开场景：SaveOnLeave 时先落库，清场景 ID（位置保留，是回来时的位置），提交后 space.Leave，停周期落库。
// 不在场景里时返回 ErrNotInScene。正在进行的移动在下一次 Advance 以 LeftScene 收尾。
func (m *MapBasic) Leave() error {
	if m.st.space == nil {
		return ErrNotInScene
	}
	if m.config.SaveOnLeave {
		if err := m.Save(); err != nil {
			return err
		}
	}
	m.guard()
	space := m.st.space
	m.st.space = nil
	m.fields.SetSceneID(0)
	m.addOp(spaceOp{space: space})
	if m.saveReg != nil {
		m.saveReg.Stop()
	}
	return nil
}

// SetPos 移动到 at：在场景里时先 QueueMove（事务内入队、走提交门；失败时什么都不写、返回错误），再写位置。同一事务里
// 刚 Enter、还没提交时不入队，提交后的 Enter 直接用最新位置。不在场景里只写位置。
func (m *MapBasic) SetPos(at spatial.Point) error {
	if at == m.fields.Pos() {
		return nil
	}
	commit := m.current()
	entering := commit != nil && len(commit.ops) > 0 && commit.ops[len(commit.ops)-1].enter
	if m.st.space != nil && !entering {
		if err := m.st.space.QueueMove(m.owner, at); err != nil {
			return err
		}
	}
	m.guard()
	m.st.dirty = true
	m.fields.SetPos(at)
	if entering {
		commit.ops[len(commit.ops)-1].at = at
	}
	m.stage()
	return nil
}

// SetFacing 写朝向。朝向和位置一样是 MapBasic 的数据：写 Fields（同步规则同位置，回滚随 DAO 字段的逆操作）、
// 算作变过（Save 与周期落库会写它，“变过”随事务回滚），提交后发布。AOI 不关心朝向，不入队。
func (m *MapBasic) SetFacing(f Facing) {
	if f == m.fields.Facing() {
		return
	}
	m.guard()
	m.st.dirty = true
	m.fields.SetFacing(f)
	m.stage()
}

// Save 场景、位置或朝向变过时调 MapConfig.Save 落库一次；没配 Save 或没变过时什么也不做。在实体锁与事务内调用，
// 持久级别是调用它的 handler 的级别。
func (m *MapBasic) Save() error {
	if m.config.Save == nil || !m.st.dirty {
		return nil
	}
	if err := m.config.Save(m.fields.SceneID(), m.fields.Pos(), m.fields.Facing()); err != nil {
		return err
	}
	m.guard()
	m.st.dirty = false
	return nil
}

// Published 返回最近一次提交的状态。任意 goroutine 可调用、不加锁：给跟随者读另一个实体的位置用（它不持有那个
// 实体的锁）。回滚的改动从不发布。
func (m *MapBasic) Published() Snapshot {
	if p := m.published.Load(); p != nil {
		return p.snap
	}
	return Snapshot{}
}

// OnDestroy 由实体的 OnDestroy 调用（卸载、删档、撤销新建）：发布“已不在”，跟随它的实体在下一拍以 TargetLost 收尾。
// 不调用 space.Leave——只从内存卸载不等于离开场景（重新加载后业务重新 Enter）；真正离开场景由业务先 Leave。
func (m *MapBasic) OnDestroy() {
	m.publish(finalSeq, Snapshot{})
}

// saveBeat 是周期落库的心跳回调。
func (m *MapBasic) saveBeat(heartbeat.Beat) error {
	if m.st.space == nil {
		m.saveReg.Stop()
		return nil
	}
	return m.Save()
}

// --- 事务内的提交后工作与回滚 ---

// guard 在当前事务里第一次改 mapState 前登记回滚恢复（每个事务一次）。不在事务里时什么也不做。
func (m *MapBasic) guard() {
	tx := nest.CurrentRollbackTx()
	if tx == nil || tx.ID() == m.rbTx {
		return
	}
	m.rbTx = tx.ID()
	prev := m.st
	tx.DeferRollback(func() error {
		m.st = prev
		return nil
	})
}

// current 返回本事务的提交后记录；不在事务里、或本事务还没有时返回 nil。
func (m *MapBasic) current() *mapCommit {
	tx := nest.CurrentRollbackTx()
	if tx == nil || m.pending == nil || m.pending.tx != tx.ID() {
		return nil
	}
	return m.pending
}

// addOp 记一次进出场景。同一事务里先进后出抵消（AOI 什么都不做）。不在事务里立即执行。
func (m *MapBasic) addOp(op spaceOp) {
	if nest.CurrentRollbackTx() == nil {
		m.runOp(op)
		m.stage()
		return
	}
	commit := m.stagedCommit()
	if n := len(commit.ops); n > 0 && commit.ops[n-1].enter && !op.enter {
		commit.ops = commit.ops[:n-1]
	} else {
		commit.ops = append(commit.ops, op)
	}
}

// stage 把当前状态记为提交后要发布的快照；不在事务里立即发布。
func (m *MapBasic) stage() {
	if nest.CurrentRollbackTx() == nil {
		m.seq++
		m.publish(m.seq, m.snapshot())
		return
	}
	m.stagedCommit()
}

// stagedCommit 取（必要时新建并登记 AfterCommit）本事务的提交后记录，并把快照更新为当前状态。
func (m *MapBasic) stagedCommit() *mapCommit {
	commit := m.current()
	if commit == nil {
		tx := nest.CurrentRollbackTx()
		m.seq++
		commit = &mapCommit{tx: tx.ID(), seq: m.seq}
		m.pending = commit
		// 提交后在实体锁外执行：只读这条记录（之后的事务各有自己的记录），不读组件的其他状态。
		tx.AfterCommit(func() { m.afterCommit(commit) })
	}
	commit.snap = m.snapshot()
	return commit
}

func (m *MapBasic) snapshot() Snapshot {
	return Snapshot{Present: true, SceneID: m.fields.SceneID(), Pos: m.fields.Pos(), Facing: m.fields.Facing()}
}

func (m *MapBasic) afterCommit(commit *mapCommit) {
	for _, op := range commit.ops {
		m.runOp(op)
	}
	m.publish(commit.seq, commit.snap)
}

func (m *MapBasic) runOp(op spaceOp) {
	var err error
	name := "leave"
	if op.enter {
		name = "enter"
		err = op.space.Enter(m.owner, op.at)
	} else {
		err = op.space.Leave(m.owner)
	}
	if err == nil {
		return
	}
	if m.config.OnSpaceError != nil {
		m.config.OnSpaceError(name, err)
		return
	}
	slog.Warn("movement: scene "+name+" did not reach the interest policy", "entity_id", m.owner.ID(), "err", err)
}

// publish 只接受更新的序号：同一实体的提交后回调按提交顺序执行，序号守住即使它们与下一个事务交错也不倒退；
// OnDestroy 用最大序号封住。
func (m *MapBasic) publish(seq uint64, snap Snapshot) {
	next := &publishedSnapshot{seq: seq, snap: snap}
	for {
		old := m.published.Load()
		if old != nil && old.seq >= seq {
			return
		}
		if m.published.CompareAndSwap(old, next) {
			return
		}
	}
}

// LocateIn 用只读实体查找（例如 EntityManager.Get：不加载、不等待）构造 Follow 用的 Locator：目标实体实现 MapOwner
// 时读它的 Published。
func LocateIn(get func(id int64) entity.IThreadSafeEntity) Locator {
	return func(id int64) (Snapshot, bool) {
		e := get(id)
		if e == nil {
			return Snapshot{}, false
		}
		owner, ok := e.(MapOwner)
		if !ok || owner.MapBasic() == nil {
			return Snapshot{}, false
		}
		snap := owner.MapBasic().Published()
		return snap, snap.Present
	}
}

// MemFields 是只在内存里的 Fields：没有 DAO 的实体（测试、示例、不持久的 NPC）用。事务（rollback=undo）里的写
// 登记逆操作，回滚恢复；它不标同步脏，需要同步的实体用 DAO 字段实现 Fields。
type MemFields struct {
	sceneID int64
	pos     spatial.Point
	facing  Facing
	// OnChange 非 nil 时每次写之后调用（例如标同步脏）。
	OnChange func()
}

const (
	memFieldScene uint64 = 1 << iota
	memFieldPos
	memFieldFacing
)

func (f *MemFields) SceneID() int64     { return f.sceneID }
func (f *MemFields) Pos() spatial.Point { return f.pos }
func (f *MemFields) Facing() Facing     { return f.facing }

func (f *MemFields) SetSceneID(v int64) {
	old := f.sceneID
	nest.RecordUndo(f, memFieldScene, func() error { f.sceneID = old; return nil })
	f.sceneID = v
	f.changed()
}

func (f *MemFields) SetPos(v spatial.Point) {
	old := f.pos
	nest.RecordUndo(f, memFieldPos, func() error { f.pos = old; return nil })
	f.pos = v
	f.changed()
}

func (f *MemFields) SetFacing(v Facing) {
	old := f.facing
	nest.RecordUndo(f, memFieldFacing, func() error { f.facing = old; return nil })
	f.facing = v
	f.changed()
}

func (f *MemFields) changed() {
	if f.OnChange != nil {
		f.OnChange()
	}
}
