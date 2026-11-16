package player

import (
	"example.com/g/game/event"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 实体包里的组件（roost add component 的写法）：类型号 2000 比组件包里的都小，同优先级时排在它们前面。
//
//roost:component type=2000
const CompTypeQuest entity.ComponentType = 2000

type QuestComponent struct {
	entity.ComponentBase
	owner *Player
}

//roost:listen
func (c *QuestComponent) OnProfileLevelUp(ev event.ProfileLevelUp) error { return nil }
