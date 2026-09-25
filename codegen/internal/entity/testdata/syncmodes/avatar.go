package syncmodes

import (
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

const EntityKindAvatar entity.EntityKind = 191

func init() { entity.MustRegisterEntityKindCategory(EntityKindAvatar, entity.EntityCategory(4)) }

//roost:entity entityKind=EntityKindAvatar sync=true syncNamespace="avatar" subjectPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.DaoManager
	state *StateDao `dao:"states"`
}

func AvatarPacker(e entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	avatar := e.(*Avatar)
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, binary.LittleEndian.AppendUint64(nil, uint64(avatar.state.GetValue()))), nil
	}
	return entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}
}
