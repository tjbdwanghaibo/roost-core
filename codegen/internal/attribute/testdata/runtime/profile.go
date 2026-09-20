package combat

// The hand-written half of an attribute profile: the fields, the derived
// formula, and the dirty mask the generated code maintains. Everything else
// (ids, masks, metadata, typed setters, Update, snapshot accessors) is
// generated from the marker.

//roost:attribute index=1 max=64
type Combat struct {
	HP     int64 `attr:"hp"`
	Attack int64 `attr:"attack"`
	Power  int64 `attr:"power"`

	// dirtyMask is the convention the generator writes through: every
	// setter ORs its attribute's bit in here, Update recomputes the derived
	// attributes whose inputs are set, and ClearDirty resets it.
	dirtyMask uint64
}

// _Power is a derived attribute: the leading underscore marks the formula and
// the parameter names name the inputs.
func (p *Combat) _Power(Attack int64, HP int64) int64 {
	return Attack*2 + HP/10
}
