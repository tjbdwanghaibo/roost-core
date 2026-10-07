//go:build protocoldef

package protocoldef

// RankTopRequest reads the top of a board. The demo has one board, so the
// request carries no board id: what a client may rank by is a server
// decision, and accepting a board id from the wire would make every board the
// service holds readable by anyone who guesses its name.
type RankTopRequest struct {
	Limit int32 `pb:"1"`
}

type RankTopEntry struct {
	Rank     int64 `pb:"1"`
	PlayerID int64 `pb:"2"`
	Value    int64 `pb:"3"`
}

type RankTopResponse struct {
	Code    int32          `pb:"1"`
	Reason  string         `pb:"2"`
	Entries []RankTopEntry `pb:"3"`
	MyRank  int64          `pb:"4"`
	Total   int64          `pb:"5"`
}

//roost:protocol group=game handler=player
type RankTopProtocol interface {
	//roost:msg id=10016
	RankTop(RankTopRequest) RankTopResponse
}
