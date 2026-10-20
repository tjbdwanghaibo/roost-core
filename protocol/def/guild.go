//go:build protocoldef

package protocoldef

// FoundGuildRequest creates a guild with this player as its founder.
type FoundGuildRequest struct {
	Name string `pb:"1"`
}

// JoinGuildRequest joins an existing guild by id. The id comes from somewhere
// outside this demo — a list endpoint, a friend, a recruitment board — which
// is deliberately not modelled: the point here is the cross-process join, not
// guild discovery.
type JoinGuildRequest struct {
	GuildID int64 `pb:"1"`
}

// GuildInfoRequest reads a guild. Empty means "the one I am in".
type GuildInfoRequest struct {
	GuildID int64 `pb:"1"`
}

// GuildResponse is what all three answer with.
type GuildResponse struct {
	Code    int32  `pb:"1"`
	Reason  string `pb:"2"`
	GuildID int64  `pb:"3"`
	Name    string `pb:"4"`
	Members int32  `pb:"5"`
	// ServedBySID is the game process that ran this call.
	//
	// It is in the answer because it is the one observable difference a
	// remote-managed entity makes, and it is worth being precise about what
	// it means: the guild has no permanent home. Whichever process takes its
	// distributed lock owns it for that transaction, so two players on two
	// processes joining the same guild produce two different values here —
	// and the roster is still one document that both writes went through.
	ServedBySID int32 `pb:"6"`
}

//roost:protocol group=game handler=player
type GuildProtocol interface {
	//roost:msg id=10021
	FoundGuild(FoundGuildRequest) GuildResponse
	//roost:msg id=10022
	JoinGuild(JoinGuildRequest) GuildResponse
	//roost:msg id=10023
	GuildInfo(GuildInfoRequest) GuildResponse
}
