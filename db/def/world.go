package dbdef

// WorldDao is the World's persistent state: two server-wide counters. They
// are the World's real job in this demo — one place that knows how many
// players ever entered this server and how many matches it formed — and they
// move only inside World's Nest lock, through the generated mutators.
//
//roost:dao coll=world db=game
type WorldDao struct {
	PlayersEntered int64 `bson:"players_entered" dao:"persist,sync"`
	MatchesFormed  int64 `bson:"matches_formed" dao:"persist,sync"`
	// ExpGranted moves in the same Nest transaction as a Player's Exp: one
	// WAL record, two entities, two lock ranks.
	ExpGranted int64 `bson:"exp_granted" dao:"persist,sync"`
	// Timers is the World's timer heap, persisted: node id -> node. The
	// scheduler in memory is rebuilt from this on load, which is what makes a
	// timer survive a restart — an in-process heap loses every pending
	// deadline when the process does, and the deadlines here are things like
	// "close the activity at 20:00" that nobody re-creates.
	//
	// It is written by the scheduler's change hook, inside the transaction
	// that scheduled or fired the timer, so the heap and the state change it
	// belongs to are one WAL record.
	Timers map[int64]*TimerNode `bson:"timers" dao:"persist,map=fast"`
	// TimerSeed is the highest node id ever minted. Persisted so a restart
	// does not hand out an id a stored node already has.
	TimerSeed int64 `bson:"timer_seed" dao:"persist"`
	// SettledActivities is activity id -> when this server acked its result
	// dispatch. It is what makes settlement once-per-activity across
	// restarts, and it is on the World rather than in the coordinator because
	// the coordinator says "the phase is collected" — what settlement PAYS is
	// the game's.
	SettledActivities map[string]int64 `bson:"settled_activities" dao:"persist,map=fast"`
}

// TimerNode is one stored timer. It is core/timer's Node minus the parts that
// only exist in memory (the heap index and the handler): a stored handler
// would be a function pointer, and the type is what finds it again.
type TimerNode struct {
	Type         int32  `bson:"type"`
	Param1       int64  `bson:"param1"`
	Param2       int64  `bson:"param2"`
	Payload      string `bson:"payload,omitempty"`
	EndUnixMilli int64  `bson:"end_unix_milli"`
	DelayMillis  int64  `bson:"delay_millis"`
}
