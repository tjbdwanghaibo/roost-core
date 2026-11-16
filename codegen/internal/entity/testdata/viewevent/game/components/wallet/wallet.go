package wallet

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2006
const CompTypeWallet entity.ComponentType = 2006

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.WalletHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeWallet, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

func (c *Component) Gold() int64 { return 0 }
