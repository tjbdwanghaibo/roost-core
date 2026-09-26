//go:build protocoldef

package protocoldef

// BattleInputRequest is one client's contribution to one lockstep frame. It
// carries three things at once because all three are per-frame chatter and a
// lockstep client sends them on the same cadence:
//
//   - Frame + Payload: the input this client wants folded into that frame.
//     Frame must be inside the room's submit window (next..next+window);
//     outside it the input is refused rather than buffered.
//   - HashFrame + Hash: the simulation hash this client sampled at a
//     keyframe. Zero HashFrame means "no report this time". The room judges
//     a keyframe once a quorum agrees, and the seats that disagree are the
//     desync verdict.
//   - CatchupFrom: ask the room to page history from this frame because the
//     client's assembler found a gap it could not heal. Zero means no
//     request.
type BattleInputRequest struct {
	BattleID    string `pb:"1"`
	Frame       uint32 `pb:"2"`
	Payload     []byte `pb:"3"`
	HashFrame   uint32 `pb:"4"`
	Hash        uint64 `pb:"5"`
	CatchupFrom uint32 `pb:"6"`
}

// BattleInputResponse is the room's verdict on this message. A refused input
// is a coded error, not a dropped connection: a client that is late, that
// addressed a battle which has ended, or that holds no seat has to learn
// which of those it was.
type BattleInputResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
}

//roost:protocol group=game handler=player
type BattleInputProtocol interface {
	//roost:msg id=10015
	BattleInput(BattleInputRequest) BattleInputResponse
}
