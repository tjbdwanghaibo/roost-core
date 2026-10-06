package remoteflow

import "github.com/tjbdwanghaibo/roost-core/entity"

// EntityKindGuild 是 Mirror 第 5 步公会摘要样例的 owner（mirror_test.go）。本包已用 235（Vault）、236（SyncedVault）、
// 237（Clerk），这里取 238。
const EntityKindGuild entity.EntityKind = 238

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindGuild, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
}

//roost:entity entityKind=EntityKindGuild remote=managed
type Guild struct {
	*entity.RemoteEntityBase
	entity.DaoManager
	dao *GuildDao `dao:"remote_guilds"`
}
