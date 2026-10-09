package entity

// EntityIDMeta is the normalized identity view shared by Nest, entity
// lookup, remote entity locking, and persistence.
type EntityIDMeta struct {
	InputID       int64
	UniqueID      int64
	FullID        int64
	Category      EntityCategory
	Kind          EntityKind
	RemoteCapable bool
}

// ResolveEntityID treats full EntityID as the canonical input.
func ResolveEntityID(id int64) EntityIDMeta {
	v := uint64(id) & EntityIDValueMask
	uniqueID := int64((v >> UniqueIDShift) & UniqueIDMask)
	kind := GetEntityKindFromID(id)
	remote := GetEntityRemoteFromID(id)

	// The registry is the authority on a kind's category. The ID's low two bits
	// are legacy padding that can only express three values, so a registered
	// kind's category is read from the registry and the field is consulted only
	// for a kind this process does not know — a cross-server ID whose category
	// nothing here can look up (M-02).
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
