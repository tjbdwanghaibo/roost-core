package remoteflow

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

// EntityKindClerk 是本地、不持久的实体，只给 RR-20260926-27 用例当“无 RollbackTx 的 memory 消息”的目标：
// 带 Remote 目标的消息总有 RollbackTx，走不到快 worker 上 admitRemoteEntityDelete 那条路径。取 237，不与 235 / 236 撞号。
const EntityKindClerk entity.EntityKind = 237

func init() { entity.MustRegisterEntityKindCategory(EntityKindClerk, entity.EntityCategory(2)) }

//roost:entity entityKind=EntityKindClerk noPersist=true
type Clerk struct {
	*entity.EntityBase
	entity.DaoManager
}
