package player

import (
	db "example.com/g/db"
	"example.com/g/game/components/activity"
	"example.com/g/game/components/attribute"
	"example.com/g/game/components/bag"
	"example.com/g/game/components/profile"
	"example.com/g/game/components/task"
	"example.com/g/game/components/wallet"
	"example.com/g/game/event"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const EntityKindPlayer entity.EntityKind = 1

//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryPlayer
type Player struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
	dao       *db.PlayerDao        `dao:"player"`
	bag       *bag.Component       `comp:"bag.CompTypeBag"`
	wallet    *wallet.Component    `comp:"wallet.CompTypeWallet"`
	profile   *profile.Component   `comp:"profile.CompTypeProfile"`
	attribute *attribute.Component `comp:"attribute.CompTypeAttribute"`
	task      *task.Component      `comp:"task.CompTypeTask"`
	activity  *activity.Component  `comp:"activity.CompTypeActivity"`
	quest     *QuestComponent      `comp:"CompTypeQuest"`
}

// OnProfileRenaming 是实体级的拦截监听。
//
//roost:listen
func (e *Player) OnProfileRenaming(ev event.ProfileRenaming) error { return nil }

// OnProfileLevelUp：同优先级时实体自己的方法排在组件前面。
//
//roost:listen
func (e *Player) OnProfileLevelUp(ev event.ProfileLevelUp) error { return nil }
