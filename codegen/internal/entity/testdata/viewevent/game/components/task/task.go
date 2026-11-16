package task

import (
	"example.com/g/game/event"
	"example.com/g/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=2004
const CompTypeTask entity.ComponentType = 2004

type Component struct {
	entity.ComponentBase
	host interface {
		entity.IThreadSafeEntity
		view.TaskHolder
	}
}

func init() {
	entity.RegisterComponentFactory(CompTypeTask, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &Component{}, nil
	})
}

var _ = event.ProfileLevelUp{}

//roost:listen
func (c *Component) OnProfileLevelUp(ev event.ProfileLevelUp) error { return c.complete(int64(ev.To)) }

// complete 经同一接收者的方法再发布：静态扫描跟进直接调用。
func (c *Component) complete(id int64) error {
	return c.host.Publish(event.TaskCompleted{ID: id})
}

func (c *Component) Done(id int64) bool { return false }
