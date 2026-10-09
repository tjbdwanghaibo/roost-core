package remoteflow

import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// EntityKindSyncedVault 只给持久拒绝用例（reject_test.go）用：与 Vault 同样的两份 Remote DAO，另开 entitysync。
// 单独一个 kind 而不是给 Vault 加 sync=true：Vault 同时是 Remote 容量 / 长稳压测（scripts/perf/remote.sh）的实体，
// 打开同步会改变压测负载的口径。本包已用 235（Vault），这里取 236，两者不能撞号。
const EntityKindSyncedVault entity.EntityKind = 236

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindSyncedVault, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
}

//roost:entity entityKind=EntityKindSyncedVault remote=managed sync=true syncNamespace="synced_vault" subjectPacker=SyncedVaultPacker
type SyncedVault struct {
	*entity.RemoteEntityBase
	entity.DaoManager
	balance *BalanceDao `dao:"remote_balances"`
	items   *ItemsDao   `dao:"remote_items"`
}

type syncedVaultSnapshot struct {
	Balance int64 `bson:"balance"`
	Items   int64 `bson:"items"`
}

// SyncedVaultPacker 每次给整份内容，订阅者据此能分辨收到的是被拒绝的内存值还是权威值。
func SyncedVaultPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	e := value.(*SyncedVault)
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		raw, err := bson.Marshal(syncedVaultSnapshot{Balance: e.balance.GetValue(), Items: e.items.GetValue()})
		return entity.TakeFrozenSyncPayload(1, raw), err
	}
	return entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}
}
