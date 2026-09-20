package world

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

//roost:component type=2002
const CompTypeStats entity.ComponentType = 2002

// StatsComponent owns World gameplay logic. Persistent or replicated state should
// be changed through generated DAO methods so dirty tracking remains correct.
type StatsComponent struct {
	entity.ComponentBase
	owner *World
}

// IStatsEntity is the narrow lock-safe view for Nest handlers using this
// component. The generated Entity wire implements the getter.
type IStatsEntity interface {
	entity.IThreadSafeEntity
	StatsComp() *StatsComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeStats, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*World)
		if !ok {
			return nil, fmt.Errorf("stats component: owner %T is not *World", owner)
		}
		return &StatsComponent{owner: typed}, nil
	})
}

func (component *StatsComponent) Name() string { return "stats" }

// Owner is available only while the Entity is alive and locked by Nest.
func (component *StatsComponent) Owner() *World { return component.owner }

// Add business methods below. Do not add mutexes here: Nest enters business
// handlers only after locking the owning Entity.

// Stats is the World's counters as a handler result. It crosses the Nest
// boundary as a value, so the endpoint that reads it never touches the World
// outside its lock.
type Stats struct {
	PlayersEntered int64
	MatchesFormed  int64
	ExpGranted     int64
}

// RecordActivitySettled records that this server has paid out one activity's
// settlement, and reports whether this call is the one that recorded it.
//
// It is on the World because settlement is server-wide: the coordinator says
// "the phase is collected", and what that PAYS is this game's, decided once
// per activity however many times the result dispatch is redelivered. The
// mails themselves are idempotent per (activity, player) too — this record is
// what stops the whole board being read and mailed again, not what makes the
// payout exactly-once.
func (component *StatsComponent) RecordActivitySettled(activityID string, nowUnix int64) bool {
	if activityID == "" {
		return false
	}
	dao := component.Owner().Dao()
	if _, settled := dao.GetSettledActivities(activityID); settled {
		return false
	}
	dao.SetSettledActivities(activityID, nowUnix)
	return true
}

// ActivitySettled reports whether an activity's settlement has been paid.
func (component *StatsComponent) ActivitySettled(activityID string) bool {
	_, settled := component.Owner().Dao().GetSettledActivities(activityID)
	return settled
}

// RecordEnter counts one login.
func (component *StatsComponent) RecordEnter() {
	dao := component.Owner().Dao()
	dao.SetPlayersEntered(dao.GetPlayersEntered() + 1)
}

// RecordMatch counts one formed match.
func (component *StatsComponent) RecordMatch() {
	dao := component.Owner().Dao()
	dao.SetMatchesFormed(dao.GetMatchesFormed() + 1)
}

// RecordExp adds to the server-wide experience total. It is called from
// AddExp, which holds the Player and the World at once — the reason the two
// kinds have distinct lock ranks.
func (component *StatsComponent) RecordExp(amount int64) {
	dao := component.Owner().Dao()
	dao.SetExpGranted(dao.GetExpGranted() + amount)
}

// Snapshot reads the counters under the lock.
func (component *StatsComponent) Snapshot() Stats {
	dao := component.Owner().Dao()
	return Stats{PlayersEntered: dao.GetPlayersEntered(), MatchesFormed: dao.GetMatchesFormed(), ExpGranted: dao.GetExpGranted()}
}
