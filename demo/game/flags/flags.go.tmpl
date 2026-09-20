// Package flags is this game's kill switches: which ones exist, and what each
// one is allowed to gate.
//
// The names are constants here so a typo is a compile error rather than a
// switch that silently never matches — a flag read by a name nobody sets
// reads as "off" forever, and nothing reports that.
package flags

import "github.com/tjbdwanghaibo/roost-core/featureflag"

// The flags this game has. Each one is also a row in
// configs/table/featureflag.csv with a note for the operator; the table is the
// source and the store is rebuilt from it on every config reload.
const (
	// Purchase gates the store's entry point. Off refuses new purchases with
	// a coded answer; orders the platform service already recorded still
	// settle, because a paid order is not something a switch may drop.
	Purchase = "purchase"
	// MonsterSpawn gates respawns. Off lets the population drain as monsters
	// are killed rather than removing anything that is already alive.
	MonsterSpawn = "monster_spawn"
	// Activity gates opening new activity windows. A window already open
	// still closes and settles — the same rule as Purchase, for the same
	// reason: work already accepted is finished.
	Activity = "activity"
)

// Enabled reports whether a flag is on. An unknown name is OFF, deliberately:
// a switch nobody configured must not read as "go ahead".
//
// Where a flag may be read is the part worth stating. It belongs at an ENTRY
// POINT — before a transaction opens, before an endpoint does its work —
// never inside one. A flag that flips between the two halves of an operation
// leaves state that neither the on-path nor the off-path would have produced,
// and the flag is then the only explanation for a shape that should not exist.
func Enabled(name string) bool { return featureflag.Enabled(name) }

// All is the set this game knows about, for the operator surface: a flag in
// the store that is not here is stale configuration, and one here that is not
// in the store has never been set.
func All() []string { return []string{Purchase, MonsterSpawn, Activity} }
