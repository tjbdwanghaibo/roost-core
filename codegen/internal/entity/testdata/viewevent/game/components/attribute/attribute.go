package attribute

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2003
const CompTypeAttribute entity.ComponentType = 2003

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.AttributeHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeAttribute, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

// OnProfileLevelUp 先刷新属性，task 再按刷新后的属性算进度。
//
//roost:listen priority=100
func (c *Component) OnProfileLevelUp(ev event.ProfileLevelUp) error { return nil }

func (c *Component) Refresh() error { return nil }
