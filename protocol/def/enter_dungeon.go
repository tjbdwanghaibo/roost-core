//go:build protocoldef

package protocoldef

// EnterDungeonRequest opens a bounded run for the player with the session
// service. The frame sequence is the idempotency key: a retried frame gets
// the same run, and a player already in a run is refused rather than given a
// second one.
type EnterDungeonRequest struct {
	Kind string `pb:"1"`
}

// EnterDungeonResponse carries the run the session service opened and when
// it expires if never finished.
type EnterDungeonResponse struct {
	Code         int32  `pb:"1"`
	Reason       string `pb:"2"`
	RunID        string `pb:"3"`
	State        string `pb:"4"`
	DeadlineUnix int64  `pb:"5"`
}

//roost:protocol group=game handler=player
type EnterDungeonProtocol interface {
	//roost:msg id=10010
	EnterDungeon(EnterDungeonRequest) EnterDungeonResponse
}
