package attribute

// The demo's attribute profile. This file is the whole hand-written half:
// the fields, their wire names, the derived formula and the dirty mask.
// Everything else — ids, masks, metadata, typed setters, Update, and the
// accessors that pull this profile out of a snapshot or a container — is
// generated from the marker into gen_combat_attribute.go, and the framework
// half (AttrID, AttributeProfile, Snapshot, Container, Selector) is
// re-exported by runtime.go from roost-core/attribute.
//
// Why attributes and not just DAO fields: a DAO field is one number the
// player owns. An attribute is a number several things have an opinion
// about — the base value, a buff, a piece of equipment — and the framework's
// container keeps those layers apart so the game can ask for "what is this
// player's attack, with everything applied" without every system knowing
// every other system.

//roost:attribute index=1 max=64
type Combat struct {
	// HP and Attack are set by gameplay; a level-up raises both.
	HP     int64 `attr:"hp"`
	Attack int64 `attr:"attack"`
	// Power is derived: _Power below owns it, and SetDirectAttr refuses it
	// so no endpoint can set a rating the formula did not produce.
	Power int64 `attr:"power"`

	// dirtyMask is the convention every generated setter writes through.
	dirtyMask uint64
}

// _Power is the derived formula: the leading underscore marks it, and the
// parameter names name the inputs. The generator turns it into a recompute
// that runs exactly when one of those inputs is dirty.
func (c *Combat) _Power(Attack int64, HP int64) int64 {
	return Attack*2 + HP/10
}

// LevelUp is the demo's one piece of attribute gameplay: a level raises the
// base numbers, and Update lets the formula catch up. It returns the bits
// that moved, which is what a caller publishes.
func (c *Combat) LevelUp(levels int32) uint64 {
	if c == nil || levels <= 0 {
		return 0
	}
	before := c.DirtyMask()
	c.SetHP(c.HP + int64(levels)*10)
	c.SetAttack(c.Attack + int64(levels)*2)
	c.Update()
	return c.DirtyMask() &^ before
}

// ForLevel builds the profile a player of this level starts from. It is a
// plain constructor: the container decides where it lives.
func ForLevel(level int32) *Combat {
	profile := &Combat{}
	profile.SetHP(100 + int64(level)*10)
	profile.SetAttack(10 + int64(level)*2)
	profile.Update()
	return profile
}
