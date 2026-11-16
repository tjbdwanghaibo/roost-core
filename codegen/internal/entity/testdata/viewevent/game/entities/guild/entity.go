package guild

import (
	db "example.com/g/db"
	"example.com/g/game/components/activity"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const EntityKindGuild entity.EntityKind = 2

// Guild 也挂 activity 组件（同一个类型号）：activity 的监听在 Guild 上再登记一次。
//
//roost:entity id=2 entityKind=EntityKindGuild category=entity.EntityCategoryOther
type Guild struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
	dao      *db.GuildDao        `dao:"guild"`
	activity *activity.Component `comp:"activity.CompTypeActivity"`
}
