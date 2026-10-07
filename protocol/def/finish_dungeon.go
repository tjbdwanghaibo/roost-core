//go:build protocoldef

package protocoldef

// FinishDungeonRequest resolves the player's run. Success is the game's
// verdict; the session service records it and releases what the run held.
type FinishDungeonRequest struct {
	RunID   string `pb:"1"`
	Success bool   `pb:"2"`
}

// FinishDungeonResponse reports the terminal state and the exp a clear was
// worth (granted through the same AddExp transaction the AddExp endpoint
// uses, so a clear can level the player up and send the reward mail).
type FinishDungeonResponse struct {
	Code         int32  `pb:"1"`
	Reason       string `pb:"2"`
	State        string `pb:"3"`
	Outcome      string `pb:"4"`
	ExpGranted   int64  `pb:"5"`
	LevelsGained int32  `pb:"6"`
	// ActivityID is the server-wide window this clear's point went into, or
	// empty when it did not contribute (no runner, or the coordinator refused).
	//
	// A window lasts 5 minutes and the standing endpoint reports whichever one
	// is current when it is ASKED, so a clear at 12:14:59 and a standing at
	// 12:15:01 are about different windows. Without this field nothing can
	// tell that apart from "the point did not count" (RR-20260921-01).
	ActivityID string `pb:"7"`
}

//roost:protocol group=game handler=player
type FinishDungeonProtocol interface {
	//roost:msg id=10011
	FinishDungeon(FinishDungeonRequest) FinishDungeonResponse
}
