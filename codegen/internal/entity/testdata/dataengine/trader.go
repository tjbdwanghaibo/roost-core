package persistflow

import "github.com/tjbdwanghaibo/roost-core/entity"

const EntityKindTrader entity.EntityKind = 193

func init() { entity.MustRegisterEntityKindCategory(EntityKindTrader, entity.EntityCategory(4)) }

//roost:entity entityKind=EntityKindTrader
type Trader struct {
	*entity.EntityBase
	entity.DaoManager
	wallet    *WalletDao    `dao:"trade_wallets"`
	inventory *InventoryDao `dao:"trade_inventories"`
}
