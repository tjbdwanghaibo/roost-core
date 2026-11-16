package profile

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2002
const CompTypeProfile entity.ComponentType = 2002

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.ProfileHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeProfile, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

// OnTaskCompleted 完成任务给经验，可能再升级：ProfileLevelUp → TaskCompleted → ProfileLevelUp 是一条环（静态提示）。
//
//roost:listen
func (c *Component) OnTaskCompleted(ev event.TaskCompleted) error {
	return c.host.Publish(event.ProfileLevelUp{From: 1, To: 2})
}

func (c *Component) Level() int32 { return 0 }
