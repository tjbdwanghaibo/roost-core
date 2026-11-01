package entity

// RemoteDAOViewDefinition 描述一个字段投影的身份和解码器。Scope 区分同 DAO 的不同视图；
// Schema 同时绑定 kind 和字段形状，字段变更后不能把旧载荷当成新视图读取。
type RemoteDAOViewDefinition struct {
	Scope  uint32
	Schema uint32
	Decode RemoteSnapshotDecodeFunc
}

// RemoteDAOViewProvider 由带 remoteview 字段的生成 DAO 实现。
// BuildRemoteViews 在 Entity 锁内读取事务最终状态，只构建独立载荷，不发布、不修改 DAO。
// 持久确认后的发布及失败恢复仍归 Remote commit / outbox；删除按全部声明的 scope 撤销。
type RemoteDAOViewProvider interface {
	RemoteViewDefinitions(EntityKind) []RemoteDAOViewDefinition
	BuildRemoteViews(entityID int64, kind EntityKind, mask uint64) ([]RemoteSnapshotRecord, error)
}

// RegisterRemoteDAOViews 在 Entity 注册阶段登记投影解码器，无投影的 DAO 不需要适配。
func RegisterRemoteDAOViews(dao any, kind EntityKind) {
	if provider, ok := dao.(RemoteDAOViewProvider); ok {
		for _, def := range provider.RemoteViewDefinitions(kind) {
			MustRegisterRemoteSnapshotDecoder(def.Schema, def.Decode)
		}
	}
}

// BuildRemoteDAOViews 冻结本次事务涉及的字段投影，不读取 DAO 的全局脏状态。
func BuildRemoteDAOViews(dao any, entityID int64, kind EntityKind, mask uint64) ([]RemoteSnapshotRecord, error) {
	if provider, ok := dao.(RemoteDAOViewProvider); ok {
		return provider.BuildRemoteViews(entityID, kind, mask)
	}
	return nil, nil
}

// RemoteDAOViewKeys 返回删除实体时需要失效的全部投影，不受本次字段 mask 限制。
func RemoteDAOViewKeys(dao any, entityID int64, kind EntityKind) []RemoteSnapshotKey {
	provider, ok := dao.(RemoteDAOViewProvider)
	if !ok {
		return nil
	}
	defs := provider.RemoteViewDefinitions(kind)
	keys := make([]RemoteSnapshotKey, 0, len(defs))
	for _, def := range defs {
		keys = append(keys, RemoteSnapshotKey{EntityID: entityID, Kind: kind, Scope: def.Scope})
	}
	return keys
}
