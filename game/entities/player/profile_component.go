package player

import (
	"fmt"
	"math"
	"strings"

	"example.com/planet/game/dungeon"
	"example.com/planet/game/effects"
	apperrors "example.com/planet/internal/errors"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

//roost:component type=2000
const CompTypeProfile entity.ComponentType = 2000

// ProfileComponent owns Player gameplay logic. Persistent or replicated state should
// be changed through generated DAO methods so dirty tracking remains correct.
type ProfileComponent struct {
	entity.ComponentBase
	owner *Player
}

// IProfileEntity is the narrow lock-safe view for Nest handlers using this
// component. The generated Entity wire implements the getter.
type IProfileEntity interface {
	entity.IThreadSafeEntity
	ProfileComp() *ProfileComponent
}

// GuildID is the guild this player belongs to, or 0.
//
// It is the player's own copy of one fact that lives on the guild's roster.
// Two places for one fact is normally what to avoid; it is right here because
// the alternative is taking the guild's DISTRIBUTED lock every time the game
// wants to know whether a player has a guild. What keeps them from drifting
// is that both are written in the same transaction — possible because Nest
// can hold a remote entity and a local one together (game/handler/join_guild.go).
func (component *ProfileComponent) GuildID() int64 {
	return component.Owner().Dao().GetGuildID()
}

// SetGuild records the player's side of a join. It is called only from the
// transaction that also writes the roster.
func (component *ProfileComponent) SetGuild(guildID int64) {
	component.Owner().Dao().SetGuildID(guildID)
	component.Owner().PublishSyncDirty()
}

func init() {
	entity.RegisterComponentFactory(CompTypeProfile, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*Player)
		if !ok {
			return nil, fmt.Errorf("profile component: owner %T is not *Player", owner)
		}
		return &ProfileComponent{owner: typed}, nil
	})
}

func (component *ProfileComponent) Name() string { return "profile" }

// Owner is available only while the Entity is alive and locked by Nest.
func (component *ProfileComponent) Owner() *Player { return component.owner }

// Add business methods below. Do not add mutexes here: Nest enters business
// handlers only after locking the owning Entity.

// ExpPerLevel is the demo's level curve: flat, so the example stays about the
// framework rather than about balancing.
const ExpPerLevel int64 = 100

// Rename sets the display name through the generated mutator.
func (component *ProfileComponent) Rename(name string) error {
	if strings.TrimSpace(name) == "" {
		return errcode.Wrap(apperrors.ErrNameEmpty, nil)
	}
	component.Owner().Dao().SetName(name)
	component.Owner().PublishSyncDirty()
	return nil
}

// CheckAddExp reports, without writing anything, whether AddExp(amount)
// would be accepted. Handlers that write other fields or other entities in
// the same transaction call it first, so an out-of-range grant is refused
// before any DAO write instead of being written and then undone.
func (component *ProfileComponent) CheckAddExp(amount int64) error {
	_, _, err := component.planExp(amount)
	return err
}

// planExp 只读地算出发放后的剩余经验与升级数，不写任何字段。
// 经验是 int64、等级是 int32：先检查加法不溢出、升级后等级仍在 int32 内，再按平坦曲线除余；
// 不按升级数逐级循环——大额奖励若逐级减，会按奖励大小占住业务 worker（修前 MaxInt64 一直卡在循环里）。
// 等级受 int32 约束后，属性按等级线性增长（每级 HP+10、Attack+2，int64）也不会溢出。
// 已存的经验或等级为负说明存档已坏，一并拒绝，不在坏值上继续累加。
func (component *ProfileComponent) planExp(amount int64) (exp int64, gained int32, err error) {
	dao := component.Owner().Dao()
	currentExp, currentLevel := dao.GetExp(), dao.GetLevel()
	if amount <= 0 || currentExp < 0 || currentLevel < 0 || amount > math.MaxInt64-currentExp {
		return 0, 0, errcode.Wrap(apperrors.ErrExpAmount, nil, "amount", amount, "exp", currentExp, "level", currentLevel)
	}
	total := currentExp + amount
	levels := total / ExpPerLevel
	if levels > int64(math.MaxInt32)-int64(currentLevel) {
		return 0, 0, errcode.Wrap(apperrors.ErrExpAmount, nil, "amount", amount, "exp", currentExp, "level", currentLevel)
	}
	return total % ExpPerLevel, int32(levels), nil
}

// AddExp grants experience, levels the player up while the curve allows it,
// and returns the number of levels gained. A grant outside the storage range
// is refused with exp_amount before either field is written.
//
// Both fields move through generated mutators, so a handler that fails later
// has both rolled back together — and so does the effect: EmitPlayerLevelUp
// rides on the same Nest transaction, so the reward mail it leads to is sent
// exactly when the new level is durable, never for a level-up that rolled
// back. The fact "a level-up happened" is emitted here, next to the state
// change that is that fact; who reacts to it lives elsewhere.
func (component *ProfileComponent) AddExp(amount int64) (int32, error) {
	exp, gained, err := component.planExp(amount)
	if err != nil {
		return 0, err
	}
	dao := component.Owner().Dao()
	currentLevel := dao.GetLevel()
	dao.SetExp(exp)
	if gained == 0 {
		// One call per mutation, at every exit that changed something: the
		// subject carries the accumulated mask, so the client sees one delta
		// for "exp moved" rather than one per setter.
		component.Owner().PublishSyncDirty()
		return 0, nil
	}
	level := currentLevel + gained
	dao.SetLevel(level)
	// The level is a DAO field; what a level is WORTH is the attribute
	// system's. LevelUp raises the base layer, recomposes Final over it and
	// writes both back — all inside this transaction, so a rollback takes
	// the attributes with the level.
	component.Owner().AttributeComp().LevelUp(gained)
	if err := effects.EmitPlayerLevelUp(component.Owner().UniqueID(), level); err != nil {
		return 0, fmt.Errorf("profile: %w", err)
	}
	component.Owner().PublishSyncDirty()
	return gained, nil
}

// ClaimDungeonRun records runID as paid and reports whether this call is the
// one that recorded it. A second call for the same run returns false and
// changes nothing, which is what makes the clear reward exactly-once: the
// record lands in the same Nest transaction as the reward itself.
//
// The value stored is the run's **resolution time**, not the time of the
// claim: that is the number the reward's deadline is computed from, so the
// ledger and the admission check read the same fact (game/dungeon explains
// why one number for both is the whole argument).
//
// Pruning happens here, on the write path, so the ledger is bounded without
// a sweeper. It is safe because a record can only be dropped a grace after
// the run it names is past its claim window (dungeon.LedgerMayForget), and a
// run past its claim window is refused before it ever reaches this function
// — with the time the claim executes, not the time its caller read. Everything moves through generated
// mutators, so a handler that fails later takes the whole ledger change back
// with the reward.
//
// A record that holds the claim time instead is never earlier than the
// resolution time — it simply expires a little later, never sooner, so no
// such record can start paying again.
func (component *ProfileComponent) ClaimDungeonRun(runID string, resolvedAtUnix, nowUnix int64) bool {
	dao := component.Owner().Dao()
	if _, claimed := dao.GetDungeonClaims(runID); claimed {
		return false
	}
	var stale []string
	dao.RangeDungeonClaims(func(key string, resolvedAt int64) bool {
		if dungeon.LedgerMayForget(resolvedAt, nowUnix) {
			stale = append(stale, key)
		}
		return true
	})
	for _, key := range stale {
		dao.DelDungeonClaims(key)
	}
	dao.SetDungeonClaims(runID, resolvedAtUnix)
	return true
}

// CountDungeonClear adds one paid clear to season's count and returns the new
// count. It is called only from the claim transaction, right after
// ClaimDungeonRun recorded the run, so the count moves exactly when a run is
// paid and rolls back with the payment.
//
// The count is the clears board's source (game/ranking): the board is brought
// up to it with rank UpdateMax rather than incremented, which is what makes a
// replay of any run — however many clears later — harmless there. Unlike the
// claim ledger it is never pruned: one entry per season the player cleared in.
func (component *ProfileComponent) CountDungeonClear(season int64) int64 {
	dao := component.Owner().Dao()
	current, _ := dao.GetDungeonClears(season)
	dao.SetDungeonClears(season, current+1)
	return current + 1
}

// DungeonClears is the player's count of paid clears in season. A read for
// callers holding the lock.
func (component *ProfileComponent) DungeonClears(season int64) int64 {
	current, _ := component.Owner().Dao().GetDungeonClears(season)
	return current
}

// DungeonRunClaimed reports whether a run has already been paid. It is a
// read for callers holding the lock; the claim itself must go through
// ClaimDungeonRun, inside the transaction that pays.
func (component *ProfileComponent) DungeonRunClaimed(runID string) bool {
	_, claimed := component.Owner().Dao().GetDungeonClaims(runID)
	return claimed
}

// DisplayName and Level read current state for callers holding the Entity
// lock. The plain Name() above is the framework's component name, so the
// player-facing one needs its own.
func (component *ProfileComponent) DisplayName() string { return component.Owner().Dao().GetName() }
func (component *ProfileComponent) Level() int32        { return component.Owner().Dao().GetLevel() }
