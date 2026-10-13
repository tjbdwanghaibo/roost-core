package skill

// StateSlot 是持久状态在 Program 里的槽位。
type StateSlot uint16

// StateHandle 标识一个持久状态：所属 Program 的 digest 与槽位，或共享状态句柄。
type StateHandle struct {
	GameplayDigest string
	Slot           StateSlot
	Shared         SharedStateHandle
}

// IsShared 报告句柄是否指向共享状态。
func (handle StateHandle) IsShared() bool { return handle.Shared != 0 }

// StateScope 是持久状态的作用域。
type StateScope string

const (
	StateScopeOwner       StateScope = "owner"
	StateScopeOwnerTarget StateScope = "owner_target"
	StateScopeTeam        StateScope = "team"
	StateScopeMatch       StateScope = "match"
)

// StateScopeBinding 是作用域绑定到的拥有者、主体与队伍。
type StateScopeBinding struct {
	Owner   EntityID
	Subject EntityID
	Team    uint64
}

// StateReadRequest 读取一个持久状态；Default 是状态不存在时的值。
type StateReadRequest struct {
	Meta    QueryMeta
	Handle  StateHandle
	Binding StateScopeBinding
	Default RuntimeValue
}

// StateReadResult 是读取结果；Present 表示状态确实存在。
type StateReadResult struct {
	Meta    QueryResultMeta
	Value   RuntimeValue
	Present bool
}

// StateMutationCommand 是一次状态写入。Default 是状态不存在时的 before：Runtime 总是按 state 声明的类型给出
// （null 默认值给的是声明类型的缺省值，不是 null 类型），Host 的 set 可以直接比较
// before 与写入值的类型。
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

// StateMutationResult 是一次状态写入的结果与前后值。
type StateMutationResult struct {
	ResultOutcome
	Commit CommitReceipt
	Before RuntimeValue
	After  RuntimeValue
}

// StateChangeEvent 描述一次持久状态变化。
type StateChangeEvent struct {
	Handle  StateHandle
	Binding StateScopeBinding
	Before  RuntimeValue
	After   RuntimeValue
	Reason  string
}

// StateStore 是 Host 的持久状态读写接口。
type StateStore interface {
	ReadState(request StateReadRequest) (StateReadResult, error)
	ModifyState(command StateMutationCommand) (StateMutationResult, error)
}
