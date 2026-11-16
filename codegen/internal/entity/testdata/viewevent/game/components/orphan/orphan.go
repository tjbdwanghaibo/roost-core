package orphan

import (
	"example.com/g/game/event"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2009
const CompTypeOrphan entity.ComponentType = 2009

// Component 没有实体挂载：它的监听只提示，不登记。
type Component struct{ entity.ComponentBase }

//roost:listen
func (c *Component) OnProfileLevelUp(ev event.ProfileLevelUp) error { return nil }
