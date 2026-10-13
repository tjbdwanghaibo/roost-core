package entity

// EntityIDMeta 是 Nest、实体查找、Remote 实体加锁与持久化共用的规范化身份视图。
type EntityIDMeta struct {
	InputID       int64
	UniqueID      int64
	FullID        int64
	Category      EntityCategory
	Kind          EntityKind
	RemoteCapable bool
}

// ResolveEntityID 解析完整 EntityID（规范输入）。kind 注册为 Remote 可用时 FullID 补上 Remote 位。
func ResolveEntityID(id int64) EntityIDMeta {
	v := uint64(id) & EntityIDValueMask
	uniqueID := int64((v >> UniqueIDShift) & UniqueIDMask)
	kind := GetEntityKindFromID(id)
	remote := GetEntityRemoteFromID(id)

	// kind 的分类以注册表为准。ID 的低两位只能表示三个值，所以已注册 kind 的分类从注册表读取，只有本进程不认识的
	// kind（无法在本地查到分类的跨服 ID）才退回读取这两位。
	category, known := EntityCategoryOfKind(kind)
	if !known {
		category = EntityCategory(v & EntityCategoryMask)
	}

	fullID := id
	if kind != EntityKindNone && IsEntityKindRemoteCapable(kind) {
		remote = true
		fullID = setRemoteCapableBit(fullID)
	}

	return EntityIDMeta{
		InputID:       id,
		UniqueID:      uniqueID,
		FullID:        fullID,
		Category:      category,
		Kind:          kind,
		RemoteCapable: remote,
	}
}
