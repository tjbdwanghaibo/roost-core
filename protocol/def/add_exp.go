//go:build protocoldef

package protocoldef

// AddExpRequest's field name must match the Nest handler's parameter (amount).
type AddExpRequest struct {
	Amount int64 `pb:"1"`
}

// AddExpResponse: Code 0 and LevelsGained on success; a coded failure
// otherwise. The reward mail for a level-up arrives asynchronously, through
// the mail service — this response says nothing about it on purpose.
type AddExpResponse struct {
	Code         int32  `pb:"1"`
	Reason       string `pb:"2"`
	LevelsGained int32  `pb:"3"`
}

//roost:protocol group=game handler=player
type AddExpProtocol interface {
	//roost:msg id=10001
	AddExp(AddExpRequest) AddExpResponse
}
