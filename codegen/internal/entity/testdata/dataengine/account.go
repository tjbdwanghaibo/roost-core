package persistflow

import "github.com/tjbdwanghaibo/roost-core/entity"

const EntityKindAccount entity.EntityKind = 192

func init() { entity.MustRegisterEntityKindCategory(EntityKindAccount, entity.EntityCategory(4)) }

//roost:entity entityKind=EntityKindAccount
type Account struct {
	*entity.EntityBase
	entity.DaoManager
	state *StateDao `dao:"states"`
}
