package dbdef

// GuildDao is a guild's persistent state.
//
// It is the demo's first REMOTE-MANAGED entity's state: a guild belongs to
// whichever game process is holding its distributed lock at the time, not to
// the process that created it. That is the whole point — a player on any
// process can join any guild, and the transaction that does it holds the
// guild wherever it currently lives.
//
// Nothing here is `sync`: a guild is not a replicated subject. What a client
// sees of it comes back as an endpoint's answer, not as a state frame.
//
//roost:dao coll=guild db=game
type GuildDao struct {
	Name string `bson:"name" dao:"persist"`
	// Members is player unique id -> when they joined. A map rather than a
	// count because the roster is what the game asks about, and a count that
	// can disagree with the roster is a count nobody trusts.
	Members map[int64]int64 `bson:"members" dao:"persist,map=fast"`
	// CreatedAtUnix and FounderID are for the operator answering "whose guild
	// is this and since when", which is the first question about any shared
	// object with a dispute attached.
	CreatedAtUnix int64 `bson:"created_at_unix" dao:"persist"`
	FounderID     int64 `bson:"founder_id" dao:"persist"`
}
