// The World's persistent timers, written by hand on the framework's entity
// timer runtime (framework/nest/entitytimer).
//
// This file is the reference for wiring that runtime without the generator.
// A project gets the same shape from `roost add timer --entity <Entity>`; the
// demo is a hand-written application and keeps every line of its own, so it
// shows the four pieces a host supplies and nothing else:
//
//   - the DAO fields (WorldDao.Timers / TimerSeed, db/def/world.go) and the
//     Store adapter below that maps them onto entitytimer.Store;
//   - the component, holding one entitytimer.Host per World, with the
//     operations this game needs (arm a window's close, find it, tick);
//   - the tick handler (game/handler/tick_world_timers.go), whose Nest name is
//     TimerTickHandler;
//   - the driver, TimerDriver, which the game service binds to Nest and the
//     business clock and starts (startWorldTimers, internal/service/game/activity.go).
//
// Deadlines are stored in milliseconds (TimerNode). The host reads the
// business clock in milliseconds and the tick carries the business time it
// was delivered at, so a deadline orders the same before and after a restart.
package world

import (
	"fmt"
	"log/slog"
	"time"

	db "example.com/planet/db"
	"example.com/planet/game/effects"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/infra/base/timer"
)

//roost:component type=2006
const CompTypeTimer entity.ComponentType = 2006

// Timer types. The type is what finds the handler again after a restart: a
// stored node cannot carry a function, so it carries a number that this
// package maps back to one.
const (
	// TimerTypeActivityPhase: the activity window whose id is in Payload has
	// reached its close phase.
	TimerTypeActivityPhase int32 = 1
)

// activityPhaseRetry is how long a phase timer waits before trying again when
// it could not record its effect.
const activityPhaseRetry = 30 * time.Second

// TimerTickHandler is the Nest handler the driver delivers a due tick to
// (handlerTickWorldTimers, game/handler/tick_world_timers.go). The Nest
// generator derives the registered name from the function name; a test in the
// handler package compares the two, so renaming either side fails a test
// instead of delivering ticks to a handler that does not exist.
const TimerTickHandler = "handlerTickWorldTimers"

// TimerDriver is the World's timer driver: an in-process index of the earliest
// deadline of each loaded World that delivers one tick to that World when it
// is due — no once-a-second loop. It is a framework object; the game service
// binds it to Nest and the business clock and starts it in Init, before the
// World is loaded, and stops it in Shutdown (startWorldTimers, internal/service/game/activity.go).
var TimerDriver = entitytimer.NewDriver(entitytimer.Config{Name: "world.timer", Handler: nest.NewHandlerName(TimerTickHandler)})

// TimerComponent is the World's scheduler: deadlines that belong to the
// server rather than to a player.
//
// Three things make it different from a `time.AfterFunc` in the process:
//
//   - **It survives a restart.** The deadlines are the DAO's Timers map and
//     nothing else, so "close the activity at 20:00" is still due after a
//     deploy at 19:58. An in-process timer loses every pending deadline with
//     the process, and loses it silently. A deadline that passed while the
//     process was down fires once after the World loads (Fire.Overdue); the
//     framework does not replay missed periods.
//   - **It fires inside the transaction.** Tick runs under the World's Nest
//     lock, so what a callback changes and the removal of the node it fired
//     are one WAL record. A timer that fired and a state change that did not
//     is the shape that pays a reward twice after a crash.
//   - **A callback does no I/O.** It records an EFFECT and returns; the effect
//     rides the same transaction to the outbox and a consumer makes the bus
//     call. Calling another service from inside an entity lock is how one
//     slow service stops a whole process.
//
// It keeps no schedule of its own: the host builds a timer.Scheduler from the
// DAO for each call and its change hook writes every add, fire and removal
// back, so a rolled-back arm or fire is the DAO's rollback and nothing else.
// The earliest deadline is reported to the driver after the transaction
// commits; a rolled-back transaction leaves the driver's index as it was.
//
// Order: deadlines due at the same moment fire by priority (lower first,
// default 0), then in the order they were armed (node id). The activity phase
// arms at 0.
//
// A stored node whose type has no callback in this build (a type that was
// removed) is reported once per type when the World loads, and dropped with a
// warning and timer.unhandled_dropped_total{kind} when it comes due: keeping
// it would leave it at the top of the heap forever.
//
// Methods may only be called under the World's lock (inside a Nest handler).
type TimerComponent struct {
	entity.ComponentBase
	owner *World
	host  *entitytimer.Host
}

// ITimerEntity is the narrow lock-safe view for Nest handlers using this
// component.
type ITimerEntity interface {
	entity.IThreadSafeEntity
	TimerComp() *TimerComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeTimer, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*World)
		if !ok {
			return nil, fmt.Errorf("timer component: owner %T is not *World", owner)
		}
		component := &TimerComponent{owner: typed}
		component.host = TimerDriver.NewHost(typed, worldTimerStore{owner: typed}, component.registerTimerHandlers)
		return component, nil
	})
}

func (component *TimerComponent) Name() string { return "timer" }

func (component *TimerComponent) Owner() *World { return component.owner }

// registerTimerHandlers maps each stored timer type to its callback.
func (component *TimerComponent) registerTimerHandlers(handlers *entitytimer.Handlers) {
	handlers.Register(TimerTypeActivityPhase, component.onActivityPhase)
}

// OnInitFinish records when this World was loaded and reports its earliest
// stored deadline to the driver, so a deadline that passed while the process
// was down is delivered once after the load. It runs after the DAO is
// attached and before the World is published, writes nothing (a load does
// not produce a save), and fails the build when a callback registration is
// wrong (a non-positive or duplicate type).
func (component *TimerComponent) OnInitFinish(_ *entity.EntityCreateParam, isCreate bool) error {
	return component.host.OnInitFinish(isCreate)
}

// OnDestroy takes the World out of the driver's index for good (unload,
// delete, an aborted create).
func (component *TimerComponent) OnDestroy(entity.EntityDestroyReason) { component.host.OnDestroy() }

// ScheduleActivityPhase arms the close deadline for one activity window, and
// reports whether this call is the one that armed it.
//
// It is idempotent per activity id: every game process that loads this World
// tries to arm the current window, and a second node for the same window
// would fire the phase twice. (Firing twice is survivable — NotifyPhase is
// idempotent per game sid — but a schedule that grows by one node per restart
// is not.)
//
// A deadline that is not after now (the caller's business time) is already
// due: it is armed for the next tick rather than refused, so a process that
// starts after the deadline still closes the window.
func (component *TimerComponent) ScheduleActivityPhase(activityID string, at time.Time, now time.Time) (int64, bool) {
	if activityID == "" {
		return 0, false
	}
	if id, armed := component.PendingActivityPhase(activityID); armed {
		return id, false
	}
	if !at.After(now) {
		at = now
	}
	id := component.host.NewTimerAt(at, TimerTypeActivityPhase, 0, 0, []byte(activityID))
	return id, id != 0
}

// PendingActivityPhase reports the node armed for an activity, if any.
func (component *TimerComponent) PendingActivityPhase(activityID string) (int64, bool) {
	found := int64(0)
	component.owner.Dao().RangeTimers(func(id int64, stored *db.TimerNode) bool {
		if stored != nil && stored.GetType() == TimerTypeActivityPhase && stored.GetPayload() == activityID {
			found = id
			return false
		}
		return true
	})
	return found, found != 0
}

// Tick fires everything due at now and returns how many nodes remain. The
// tick handler calls it with the business time the driver delivered.
func (component *TimerComponent) Tick(now time.Time) int { return component.host.Tick(now) }

// onActivityPhase records the phase effect and retires the node.
//
// The return value is the retry: zero means "done, remove me", and a duration
// means "come back then". That is how a failed Emit is handled — the node
// stays and the window is closed late — rather than by logging and losing the
// only thing that would have closed it. A phase that came due while the
// process was down (fire.Overdue) is reported the same way: closing a window
// late is the whole point of firing it after a restart.
func (component *TimerComponent) onActivityPhase(fire entitytimer.Fire) time.Duration {
	activityID := string(fire.Payload)
	if activityID == "" {
		return 0
	}
	if err := effects.EmitActivityPhaseDue(activityID); err != nil {
		slog.Error("world timer: activity phase effect not recorded; retrying",
			"activity_id", activityID, "retry_in", activityPhaseRetry, "err", err)
		return activityPhaseRetry
	}
	return 0
}

// worldTimerStore maps WorldDao's two timer fields onto entitytimer.Store.
// Writes go through the generated mutators, so they roll back with the
// transaction that made them.
type worldTimerStore struct{ owner *World }

func (store worldTimerStore) SavedTimers() []timer.Node {
	dao := store.owner.Dao()
	saved := make([]timer.Node, 0, dao.TimersLen())
	dao.RangeTimers(func(id int64, stored *db.TimerNode) bool {
		if stored == nil {
			return true
		}
		saved = append(saved, timer.Node{
			ID: id, Type: stored.GetType(), Priority: stored.GetPriority(),
			Param1: stored.GetParam1(), Param2: stored.GetParam2(),
			Payload: []byte(stored.GetPayload()),
			End:     time.UnixMilli(stored.GetEndUnixMilli()),
			Delay:   time.Duration(stored.GetDelayMillis()) * time.Millisecond,
		})
		return true
	})
	return saved
}

// TimerSeed is the highest node id ever minted. The host advances it with
// every new node, so a restart cannot mint an id a stored node already holds
// — two nodes with one id is one deadline silently replacing another.
func (store worldTimerStore) TimerSeed() int64 { return store.owner.Dao().GetTimerSeed() }

func (store worldTimerStore) SetTimerSeed(seed int64) { store.owner.Dao().SetTimerSeed(seed) }

func (store worldTimerStore) DeleteTimer(id int64) { store.owner.Dao().DelTimers(id) }

func (store worldTimerStore) PutTimer(node timer.Node) {
	stored := &db.TimerNode{}
	stored.SetType(node.Type)
	stored.SetPriority(node.Priority)
	stored.SetParam1(node.Param1)
	stored.SetParam2(node.Param2)
	stored.SetPayload(string(node.Payload))
	stored.SetEndUnixMilli(node.End.UnixMilli())
	stored.SetDelayMillis(node.Delay.Milliseconds())
	store.owner.Dao().SetTimers(node.ID, stored)
}
