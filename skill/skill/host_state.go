package skill

type StateSlot uint16

type StateHandle struct {
	GameplayDigest string
	Slot           StateSlot
	Shared         SharedStateHandle
}

func (handle StateHandle) IsShared() bool { return handle.Shared != 0 }

type StateScope string

const (
	StateScopeOwner       StateScope = "owner"
	StateScopeOwnerTarget StateScope = "owner_target"
	StateScopeTeam        StateScope = "team"
	StateScopeMatch       StateScope = "match"
)

type StateScopeBinding struct {
	Owner   EntityID
	Subject EntityID
	Team    uint64
}

type StateReadRequest struct {
	Meta    QueryMeta
	Handle  StateHandle
	Binding StateScopeBinding
	Default RuntimeValue
}

type StateReadResult struct {
	Meta    QueryResultMeta
	Value   RuntimeValue
	Present bool
}

type StateMutationCommand struct {
	Meta                 CommandMeta
	Handle               StateHandle
	Binding              StateScopeBinding
	Scope                StateScope
	Operation            string
	Value                RuntimeValue
	Default              RuntimeValue
	Minimum              int64
	Maximum              int64
	DurationTicks        Tick
	MaximumDurationTicks Tick
	ExpiryPolicy         string
	ClearOn              []string
	Event                EventContext
}

type StateMutationResult struct {
	ResultOutcome
	Commit CommitReceipt
	Before RuntimeValue
	After  RuntimeValue
}

type StateChangeEvent struct {
	Handle  StateHandle
	Binding StateScopeBinding
	Before  RuntimeValue
	After   RuntimeValue
	Reason  string
}

type StateStore interface {
	ReadState(request StateReadRequest) (StateReadResult, error)
	ModifyState(command StateMutationCommand) (StateMutationResult, error)
}
