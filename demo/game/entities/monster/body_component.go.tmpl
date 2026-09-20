package monster

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
)

//roost:component type=2100
const CompTypeBody entity.ComponentType = 2100

// BodyComponent is where a monster is and how much of it is left. It is the
// Player's MapComponent with fewer questions: the same rule applies, which is
// that the position is read and written here and nowhere else.
type BodyComponent struct {
	entity.ComponentBase
	owner *Monster
}

// IBodyEntity is the narrow lock-safe view for Nest handlers.
type IBodyEntity interface {
	entity.IThreadSafeEntity
	BodyComp() *BodyComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeBody, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*Monster)
		if !ok {
			return nil, fmt.Errorf("body component: owner %T is not *Monster", owner)
		}
		return &BodyComponent{owner: typed}, nil
	})
}

func (component *BodyComponent) Name() string { return "body" }

func (component *BodyComponent) Owner() *Monster { return component.owner }

func (component *BodyComponent) Pos() spatial.Point {
	dao := component.owner.Dao()
	return spatial.Point{X: dao.GetPosX(), Y: dao.GetPosY()}
}

func (component *BodyComponent) Template() int32 { return component.owner.Dao().GetTemplate() }

func (component *BodyComponent) HP() int64 { return component.owner.Dao().GetHP() }

func (component *BodyComponent) Alive() bool { return component.HP() > 0 }

// Place sets the monster up where the spawner decided. It is the one write
// that happens before anybody can see the monster, so it is also the only one
// that sets the template.
func (component *BodyComponent) Place(template int32, hp int64, at spatial.Point) {
	dao := component.owner.Dao()
	dao.SetTemplate(template)
	dao.SetHP(hp)
	dao.SetPosX(at.X)
	dao.SetPosY(at.Y)
	component.owner.PublishSyncDirty()
}

// Damage takes hp off and reports whether this blow killed it. The caller
// decides what a death means — the monster does not remove itself, because
// what has to happen (tell the AOI, tell the room, queue a respawn) is the
// scene's business and not a component's.
func (component *BodyComponent) Damage(amount int64) bool {
	if amount <= 0 || !component.Alive() {
		return false
	}
	dao := component.owner.Dao()
	remaining := dao.GetHP() - amount
	if remaining < 0 {
		remaining = 0
	}
	dao.SetHP(remaining)
	component.owner.PublishSyncDirty()
	return remaining == 0
}
