package activity

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2005
const CompTypeActivity entity.ComponentType = 2005

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.ActivityHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeActivity, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

//roost:listen priority=-1
func (c *Component) OnProfileLevelUp(ev event.ProfileLevelUp) error { return nil }

func (c *Component) Rearm() error { return nil }
