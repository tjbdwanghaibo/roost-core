package syncruntime

// The minimal shape a project gets from `roost add entity` plus `sync=true`:
// an EntityBase, the two managers, and a packer factory. No business DAO and
// no component — the point is the generated Sync wiring, not the payload.

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
)

const EntityKindAvatar entity.EntityKind = 141

const SyncTopicAvatar = "avatar"

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindAvatar, Category: 4})
}

// AvatarPacker is the project-supplied factory. Its signature is Core's
// current one: one packer per entity.
func AvatarPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	return avatarPacker{}
}

type avatarPacker struct{}

func (avatarPacker) PackSubjectSnapshot(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return entity.FrozenSyncPayload{}, nil
}

func (avatarPacker) PackSubjectDelta(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
	return entity.FrozenSyncPayload{}, nil
}

//roost:entity entityKind=EntityKindAvatar sync=true syncTopic="avatar" subjectPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}

// Plain is the sync=false control: the same shape with no sync wiring.
const EntityKindPlain entity.EntityKind = 142

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindPlain, Category: 4})
}

//roost:entity entityKind=EntityKindPlain
type Plain struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
