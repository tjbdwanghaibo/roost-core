package ai

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
)

// DecisionHost is per entity. The owner serializes all calls, including Init,
// under its normal guard. Hooks must not do external I/O or reenter the strategy.
// Load returns (0,nil,nil) only for an explicitly absent record. Existing records
// must have strictly positive versions. Init only reads; it never resets a record.
// Input maps trusted host state and (optionally) one immediate game event to
// explicit logic time/fields. An event must never turn into an implicit tick.
// Commit must compare expected, advance the version and stage ALL intents together
// with next in one transaction, or reject with no changes. Do not execute effects
// and then try to save next. Nest's outer commit can still roll back a staged write.
// A host with retried messages supplies its own stable input sequence/idempotency key.
// OnError is required: Controller's void callbacks cannot propagate a rejection.
// For transactional hosts report it to the enclosing handler, which returns the error.
// None of the callbacks should retain or mutate the Program's inputs concurrently.
type DecisionHost struct {
	Load    func(*Context) (version int64, state *decision.State, err error)
	Input   func(*Context, time.Time, *Event) (decision.Input, error)
	Commit  func(ctx *Context, expected int64, next decision.State, intents []decision.Intent) error
	OnError func(error)
}

// DecisionStrategy is a selector/intent producer, not an action runner. It has no
// timer, effect dispatcher, mutable interpreter state or locally committed cursor.
// The authority is always Load (including after an outer transaction rolls back).
type DecisionStrategy struct {
	name    string
	program *decision.Program
	host    DecisionHost
}

var _ Strategy = (*DecisionStrategy)(nil)
var _ EventStrategy = (*DecisionStrategy)(nil)

func NewDecisionStrategy(name string, program *decision.Program, host DecisionHost) (*DecisionStrategy, error) {
	if name == "" || program == nil || host.Load == nil || host.Input == nil || host.Commit == nil || host.OnError == nil {
		return nil, errors.New("ai: decision strategy requires name, program, load, input, commit and error sink")
	}
	if err := program.ValidateState(program.NewState()); err != nil {
		return nil, err
	}
	return &DecisionStrategy{name: name, program: program, host: host}, nil
}
func (s *DecisionStrategy) Name() string { return s.name }

func (s *DecisionStrategy) load(ctx *Context) (int64, decision.State, error) {
	version, state, err := s.host.Load(ctx)
	if err != nil {
		return 0, decision.State{}, err
	}
	if version == 0 && state == nil {
		return 0, s.program.NewState(), nil
	}
	if version <= 0 || state == nil {
		return 0, decision.State{}, fmt.Errorf("ai: invalid decision snapshot (version %d, present %t)", version, state != nil)
	}
	if err := s.program.ValidateState(*state); err != nil {
		return 0, decision.State{}, err
	}
	return version, *state, nil
}
func (s *DecisionStrategy) Init(ctx *Context) error { _, _, err := s.load(ctx); return err }
func (s *DecisionStrategy) step(ctx *Context, now time.Time, event *Event) error {
	version, current, err := s.load(ctx)
	if err != nil {
		return err
	}
	if version == math.MaxInt64 {
		return errors.New("ai: decision version exhausted")
	}
	in, err := s.host.Input(ctx, now, event)
	if err != nil {
		return err
	}
	if (event == nil) != (in.Event == "") {
		return errors.New("ai: host changed tick/event dispatch kind")
	}
	next, intents, err := s.program.Step(current, in)
	if err != nil {
		return err
	}
	return s.host.Commit(ctx, version, next, intents)
}
func (s *DecisionStrategy) Tick(ctx *Context, now time.Time) {
	if err := s.step(ctx, now, nil); err != nil {
		s.host.OnError(err)
	}
}
func (s *DecisionStrategy) OnEvent(ctx *Context, ev Event) {
	var now time.Time
	if ctx != nil {
		now = ctx.Now
	}
	if err := s.step(ctx, now, &ev); err != nil {
		s.host.OnError(err)
	}
}

// Raw action and mission completions belong to the executing strategy. If a game
// needs a decision event for one, validate its instance/token in the host first.
func (*DecisionStrategy) OnActionEnd(*Context, int64, actionflow.ActionKind, actionflow.ActionReason) {
}
func (*DecisionStrategy) OnMissionEnd(*Context, actionflow.Mission, actionflow.ActionReason) {}
func (*DecisionStrategy) CanStopByNext(Strategy) bool                                        { return true }
