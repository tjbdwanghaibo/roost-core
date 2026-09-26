package monster

import (
	db "example.com/planet/db"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// MonsterSyncCodec identifies the payload format on the wire.
const MonsterSyncCodec uint16 = 2

// NewMonsterSyncPacker is the factory the entity's subjectPacker marker names.
// It is the Player's packer with a different DAO — which is the point: the
// replication pipeline does not have a special case for "things that are not
// players".
func NewMonsterSyncPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	owner, _ := value.(*Monster)
	return monsterSyncPacker{monster: owner}
}

type monsterSyncPacker struct{ monster *Monster }

var monsterViews, monsterViewsErr = dataengine.NewSyncViewSet(db.MonsterDaoSyncFields(),
	entity.NamedSyncView{Profile: entity.SyncProfile{}, Fields: []string{"*"}, Priority: -1},
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "near"}, Fields: []string{"Template", "PosX", "PosY", "HP"}, Priority: 0},
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "far", LOD: 1}, Fields: []string{"Template", "PosX", "PosY"}, Priority: 10},
)

func (packer monsterSyncPacker) PackSubjectSnapshot(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, dataengine.AllFields)
}

func (packer monsterSyncPacker) PackSubjectDelta(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, mask)
}

func (packer monsterSyncPacker) pack(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	if packer.monster == nil {
		return entity.FrozenSyncPayload{}, fmt.Errorf("monster sync packer: no entity")
	}
	if monsterViewsErr != nil {
		return entity.FrozenSyncPayload{}, monsterViewsErr
	}
	view, ok := monsterViews.Lookup(profile)
	if !ok {
		return entity.FrozenSyncPayload{}, fmt.Errorf("%w: %+v", entity.ErrSyncViewUnknown, profile)
	}
	mask &= view.Fields
	if mask == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	raw := packer.monster.Dao().MarshalSync(mask)
	if len(raw) == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	return entity.TakeFrozenSyncPayload(MonsterSyncCodec, raw), nil
}
