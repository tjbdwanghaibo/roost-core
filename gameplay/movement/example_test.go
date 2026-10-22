package movement_test

import (
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/movement"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 业务的动作种类与组：两个移动动作放同一组，新的移动取代旧的。
const (
	ActMoveTo actionflow.ActionKind = 1
	ActFollow actionflow.ActionKind = 2

	GroupMove actionflow.ActionGroup = 1
)

// Monster 是一个会移动的实体：一个心跳宿主，挂 MapBasic、Move（被动）与心跳驱动的 runner（移动由 MoveTo 动作在
// runner 的心跳里推进）。位置的权威是业务 DAO 的
// nopersist + sync 字段（这里用 MemFields 代替），组件不另存一份。
type Monster struct {
	*entity.EntityBase
	host *heartbeat.Host
	mb   *movement.MapBasic
	move *movement.Move
	flow *actionflow.BeatRunner
}

func (m *Monster) Base() *entity.EntityBase             { return m.EntityBase }
func (m *Monster) MapBasic() *movement.MapBasic         { return m.mb }   // movement.MapOwner：被跟随时读位置
func (m *Monster) MoveComponent() *movement.Move        { return m.move } // movement.MoveOwner：移动动作找组件
func (m *Monster) Runner() *actionflow.ActionRunner     { return m.flow.Runner() }
func (m *Monster) OnDestroy(entity.EntityDestroyReason) { m.mb.OnDestroy(); m.host.OnDestroy() }

// 进程里一个心跳驱动（wiring/heartbeat 的 Mod 装上它）、一个封存的动作注册表、一个只读实体查找。
var (
	driver   = heartbeat.NewDriver(heartbeat.Config{Name: "game"})
	registry = actionflow.NewRegistry()
	lookup   func(id int64) entity.IThreadSafeEntity
)

func init() {
	if err := movement.RegisterActions(registry, ActMoveTo, ActFollow); err != nil {
		panic(err)
	}
	registry.Seal()
}

// newMonster 是实体工厂（实体锁内或发布之前）：构造组件；runner 在组第一次有动作时登记那个组的心跳。
func newMonster(e *entity.EntityBase, fields movement.Fields) (*Monster, error) {
	m := &Monster{EntityBase: e}
	m.host = driver.NewHost(m)
	var err error
	if m.mb, err = movement.NewMapBasic(m, m.host, movement.MapConfig{
		Fields:      fields,
		SaveOnLeave: true,
		Save: func(sceneID int64, at spatial.Point, facing movement.Facing) error {
			return nil // 写业务 DAO 的持久字段（上次所在场景、位置与朝向）
		},
	}); err != nil {
		return nil, err
	}
	if m.move, err = movement.NewMove(m.mb, movement.MoveConfig{Locate: movement.LocateIn(lookup)}); err != nil {
		return nil, err
	}
	m.flow, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(kind actionflow.ActionKind) (actionflow.ActionGroup, bool) {
			return GroupMove, kind == ActMoveTo || kind == ActFollow
		},
	}, actionflow.BeatConfig{Host: m.host})
	return m, err
}

// handleSpawn 和 handleMoveTo 是快池 handler 的业务部分（实体锁与事务内）：进场景、发起移动都只改内存与入队。
// 发起移动是 MoveTo 动作：runner 给移动组挂心跳，每拍动作调 Move.Advance，到达时动作收尾。runner 不随事务回滚，
// 所以动作接口放在 handler 的最后一步：前面会失败的校验、修改都做完了再 Start。
func handleSpawn(m *Monster, interest *policy.Interest, at spatial.Point) error {
	return m.mb.Enter(1, movement.InterestSpace{Interest: interest}, at)
}

func handleMoveTo(m *Monster, goal spatial.Point) error {
	if !m.mb.InScene() {
		return movement.ErrNotInScene // 校验在前
	}
	// 动作在最后。
	_, err := m.Runner().Start(ActMoveTo, movement.MoveToParam{Path: []spatial.Point{goal}, Speed: 6}, 0, time.Time{})
	return err
}

// Example 展示一个实体怎样接上 MapBasic、Move 与心跳驱动的 runner：实体工厂 newMonster 构造组件，handler 里
// handleSpawn 进场景、handleMoveTo 发起移动（handler 照常经 nest.MustRegisterHandlerWithMeta 注册，rollback=undo）。
// 这里只构造实体、不运行：运行需要 Nest 引擎、心跳驱动与同步 Manager，完整的运行见本包 nest_test.go 与 smoke_test.go。
func Example() {
	m, err := newMonster(entity.NewEntityBase(1<<20, entity.EntityCategoryOther, true), &movement.MemFields{})
	fmt.Println(err == nil, m.mb.InScene(), m.move.Moving())
	_, _ = handleSpawn, handleMoveTo
	// Output: true false false
}
