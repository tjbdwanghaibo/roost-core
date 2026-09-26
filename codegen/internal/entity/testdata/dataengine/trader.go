package persistflow

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const EntityKindTrader entity.EntityKind = 193

func init() { entity.MustRegisterEntityKindCategory(EntityKindTrader, entity.EntityCategory(4)) }

//roost:entity entityKind=EntityKindTrader sync=true syncNamespace="trader" subjectPacker=TraderPacker
type Trader struct {
	*entity.EntityBase
	entity.DaoManager
	wallet    *WalletDao    `dao:"trade_wallets"`
	inventory *InventoryDao `dao:"trade_inventories"`
}

// 性能工程使用正式生成 DAO 的同步脏标记，业务快照按 Entity 原子打包。
type traderSnapshot struct {
	ID        int64 `bson:"id"`
	Coins     int64 `bson:"coins"`
	Transfers int64 `bson:"transfers"`
	Items     int64 `bson:"items"`
	X         int64 `bson:"x"`
	Y         int64 `bson:"y"`
	Changed   int64 `bson:"changed"`
}

func TraderPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	e := value.(*Trader)
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		raw, err := bson.Marshal(traderSnapshot{e.ID(), e.wallet.GetCoins(), e.wallet.GetTransfers(), e.inventory.GetItems(), e.wallet.GetX(), e.wallet.GetY(), e.wallet.GetChanged()})
		return entity.TakeFrozenSyncPayload(1, raw), err
	}
	return entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}
}
