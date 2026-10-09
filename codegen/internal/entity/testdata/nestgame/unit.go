package nestgame

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 此夹具的客户端、Gate、Game 在同一进程；测试包中的测量字段保存同一单调时钟
// 原点的偏移，解码时还原单调时间。UnixNano 序列化会丢失单调分量，长测中
// 系统墙钟调整会让按计划生成的时间与接收时间失配。跨进程不能复用此口径。
var loadClockOrigin = time.Now()

func loadTimestamp(t time.Time) int64 { return int64(t.Sub(loadClockOrigin)) }
func loadTime(ns int64) time.Time     { return loadClockOrigin.Add(time.Duration(ns)) }

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

// 手写同步取脏入口只用于这个正式生成负载夹具，数据仍由生成 DAO setter 管理。
func (e *Unit) TakeEntitySyncChanges() uint64 {
	mask := e.state.DirtyTracker().TakeEntitySyncDirty()
	if mask != 0 && e.Sync() != nil {
		e.state.SetCommittedNS(loadTimestamp(time.Now()))
		mask |= e.state.DirtyTracker().TakeEntitySyncDirty()
	}
	return mask
}
