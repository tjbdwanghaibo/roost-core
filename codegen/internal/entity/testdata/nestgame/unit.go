package nestgame

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

// 独立生成模块使用的 kind，不写入其他压测的进程级注册表。
const kindUnit entity.EntityKind = 194
const componentHeartbeat entity.ComponentType = 1

func init() {
	entity.MustRegisterEntityKindCategory(kindUnit, entity.EntityCategory(4))
	entity.RegisterComponentFactory(componentHeartbeat, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &HeartbeatComponent{unit: owner.(*Unit)}, nil
	})
}

//roost:entity entityKind=kindUnit
type Unit struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
	state     *StateDao           `dao:"states"`
	heartbeat *HeartbeatComponent `comp:"componentHeartbeat"`
}

// HeartbeatComponent 没有可回滚业务状态；所有修改经正式生成 DAO setter。
// 调用方必须是持有该 Entity Guard 的 Nest handler，不能在 ticker 回调直接调用。
type HeartbeatComponent struct {
	entity.ComponentBase
	unit *Unit
}

func (c *HeartbeatComponent) Name() string { return "heartbeat" }

func (c *HeartbeatComponent) Tick(change bool) {
	state := c.unit.state
	if hp := state.GetHP(); hp < 0 || hp >= 100 {
		panic("invalid heartbeat state")
	}
	if change {
		state.SetHeartbeats(state.GetHeartbeats() + 1)
		state.SetHP((state.GetHP() + 1) % 100)
	}
}

func (e *Unit) HandleMessage(change bool) {
	if e.state.GetX() != e.state.GetMessages() {
		panic("invalid message state")
	}
	if change {
		e.state.SetMessages(e.state.GetMessages() + 1)
		e.state.SetX(e.state.GetX() + 1)
	}
}
