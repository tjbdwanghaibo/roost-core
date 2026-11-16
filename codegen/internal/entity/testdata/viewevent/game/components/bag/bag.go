package bag

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2001
const CompTypeBag entity.ComponentType = 2001

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.BagHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeBag, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

func (c *Component) Count(itemID int64) int64 { return 0 }
