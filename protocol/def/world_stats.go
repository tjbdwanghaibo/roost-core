//go:build protocoldef

package protocoldef

type WorldStatsRequest struct{}

// WorldStatsResponse is the World's two counters, read under its lock.
type WorldStatsResponse struct {
	Code           int32  `pb:"1"`
	Reason         string `pb:"2"`
	PlayersEntered int64  `pb:"3"`
	MatchesFormed  int64  `pb:"4"`
	ExpGranted     int64  `pb:"5"`
}

//roost:protocol group=game handler=player
type WorldStatsProtocol interface {
	//roost:msg id=10005
	WorldStats(WorldStatsRequest) WorldStatsResponse
}
