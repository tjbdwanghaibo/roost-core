// Package attribute is the framework's half of the attribute system: the
// identifiers, the metadata, the profile contract and the container that
// holds a subject's layers. The other half is generated per project by
// roost-codegen's attribute generator from a `//roost:attribute` marker —
// it produces one profile's ids, masks, metadata, typed setters, derived
// formulas and typed accessors, and it implements Profile.
//
// The split is deliberate. What every game needs the same way (how an
// attribute is named and addressed, what a snapshot means, how layers are
// selected) lives here and can be relied on across projects; what is specific
// to a game (which attributes exist, how power is derived from attack) is
// generated from the game's own declaration. Before this package existed the
// generated code referenced these types by name and no package provided
// them, so the feature could not compile at all (RR-20260917-06).
package attribute

// AttrID identifies one attribute inside a profile. Ids are assigned by the
// generator from the profile's declared index range, so they are stable for
// as long as the declaration is: persist them, put them on the wire, key
// maps by them.
type AttrID uint16

// AttrValue is the common numeric form every attribute travels in. A profile
// may store an attribute as a narrower type; the generated setters convert.
// One type for the wire and for bulk transfer is what lets the framework move
// attributes it knows nothing about.
type AttrValue int64

// Meta describes one attribute: what it is called in the declaration
// (`attr:"hp"`) and in Go (`HP`), which dirty bit belongs to it, and whether
// it is derived — a derived attribute is computed by a formula and must not
// be set directly, which is why the generated SetDirectAttr refuses it.
type Meta struct {
	ID      AttrID
	Name    string
	Field   string
	Mask    uint64
	Derived bool
}

// Profile is what a generated attribute profile implements. A game holds
// profiles; the framework moves them.
//
// The dirty mask is the contract's core: every setter ORs its attribute's bit
// into it, Update recomputes the derived attributes whose inputs are set, and
// ClearDirty resets it once the change has been published. The generated code
// maintains an unexported `dirtyMask uint64` field on the profile struct —
// the one field the declaration must carry itself.
type Profile interface {
	// Reset returns the profile to its zero value, dirty mask included.
	Reset()
	// CloneProfile is a deep copy: a snapshot hands one out so a reader can
	// never mutate the live profile.
	CloneProfile() Profile
	// DirtyMask is the OR of every attribute changed since ClearDirty.
	DirtyMask() uint64
	ClearDirty()
	// SetAttr sets any attribute, derived ones included; the generated code
	// uses it for restores. Returns whether the value changed.
	SetAttr(id AttrID, value AttrValue) bool
	// SetDirectAttr sets only attributes that are not derived. This is the
	// entry point for gameplay: a derived attribute is the formula's to own.
	SetDirectAttr(id AttrID, value AttrValue) bool
	GetAttr(id AttrID) (AttrValue, bool)
	// LoadValues applies a bulk map and runs Update, and returns the bits
	// this call turned on (the mask before the load is excluded, so a caller
	// learns what IT changed).
	LoadValues(values map[AttrID]AttrValue) uint64
	// ExportValues writes the profile into dst, deleting the entries whose
	// attribute is zero so the map stays the sparse form the wire wants.
	ExportValues(dst map[AttrID]AttrValue)
	// Update recomputes every derived attribute whose inputs are dirty and
	// reports whether any of them moved.
	Update() bool
	MetaByID(id AttrID) (Meta, bool)
	MetaByName(name string) (Meta, bool)
}
