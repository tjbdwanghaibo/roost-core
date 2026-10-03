package schema

// FeatureFlag is the demo's kill-switch table.
//
// A flag is config data rather than a constant because the thing it answers —
// "should this be on right now" — is an operational question with a live
// answer. The table is the SOURCE; `roost-core/featureflag` is the in-memory
// store the game reads, rebuilt from this on every reload
// (internal/service/<game>/flags.go).
//
// What belongs here is a switch at an ENTRY POINT: the store, the spawner, a
// new endpoint. What does not is a branch inside a transaction — a flag that
// flips between the two halves of one operation leaves state that neither
// branch would have produced.
//
//roost:table name=featureflag key=Name
type FeatureFlag struct {
	Name    string `csv:"name" json:"name" title:"Name" required:"true" unique:"true"`
	Enabled bool   `csv:"enabled" json:"enabled" title:"Enabled"`
	// Note is for the operator who finds this flag off at 3am and has to
	// decide whether turning it on is safe. An empty note is a flag nobody
	// can act on.
	Note string `csv:"note" json:"note" title:"Note"`
}
