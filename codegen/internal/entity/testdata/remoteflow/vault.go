package remoteflow

import "github.com/tjbdwanghaibo/roost-core/entity"

const EntityKindVault entity.EntityKind = 235

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindVault, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
}

//roost:entity entityKind=EntityKindVault remote=managed
type Vault struct {
	*entity.RemoteEntityBase
	entity.DaoManager
	balance *BalanceDao `dao:"remote_balances"`
	items   *ItemsDao   `dao:"remote_items"`
}
