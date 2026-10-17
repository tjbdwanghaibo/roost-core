package guild

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	apperrors "example.com/planet/internal/errors"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

//roost:component type=2007
const CompTypeRoster entity.ComponentType = 2007

// MaxMembers bounds a guild. A roster is loaded, locked and written as one
// document, so "how big can it get" is a real limit rather than a policy —
// an unbounded one turns every join into a bigger write and a longer lock.
const MaxMembers = 50

// RosterComponent owns who is in the guild.
//
// Every method here runs under the guild's DISTRIBUTED lock, which is the
// same statement as "under the entity's lock" for any other entity — Nest
// prepares the remote entity before the handler runs, so a handler does not
// know or care that the lock crossed a network.
type RosterComponent struct {
	entity.ComponentBase
	owner *Guild
}

// IRosterEntity is the narrow lock-safe view for Nest handlers.
type IRosterEntity interface {
	entity.IThreadSafeEntity
	RosterComp() *RosterComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeRoster, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*Guild)
		if !ok {
			return nil, fmt.Errorf("roster component: owner %T is not *Guild", owner)
		}
		return &RosterComponent{owner: typed}, nil
	})
}

func (component *RosterComponent) Name() string { return "roster" }

func (component *RosterComponent) Owner() *Guild { return component.owner }

// ValidateName is the one rule for a guild name, and it reads no state, so the
// found_guild endpoint calls it before it reserves an id or creates the
// entity: a request that can be refused from its bytes alone must not reach
// anything durable. Found calls it too, for any other caller.
//
// 名字必须有可见内容：全空白、含控制字符或不是合法 UTF-8 的名字与空名同样是 guild_name。名字原样保存，不裁剪，
// 同一请求的重试按原名比对。
//
// 长度上限、敏感词、全服唯一是业务规则，框架不处理（维护者 2026-10-17 决定），demo 也不实现：两个公会可以同名。
// 长度上限与敏感词加在这里（不读状态，仍在保留 ID 之前）。全服唯一要读写共享状态，不属于这个函数：用框架的
// service/directory（wiring/directory 装配）在保留 ID 之前 Reserve 占名，创建成功后 Commit、失败 Cancel；
// 预约带过期，进程中途崩溃时名字到期释放，不会被永久占死。
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return errcode.Wrap(apperrors.ErrGuildName, nil, "name", name)
	}
	return nil
}

// Found writes the guild's identity. It is separate from Join because a guild
// that exists with no name is a row nobody can explain.
func (component *RosterComponent) Found(name string, founderID, nowUnix int64) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	dao := component.Owner().Dao()
	if dao.GetName() != "" {
		return errcode.Wrap(apperrors.ErrGuildExists, nil, "name", dao.GetName())
	}
	dao.SetName(name)
	dao.SetFounderID(founderID)
	dao.SetCreatedAtUnix(nowUnix)
	dao.SetMembers(founderID, nowUnix)
	return nil
}

// Join adds a member and reports the roster size afterwards.
//
// Already a member is NOT an error: the endpoint that calls this is retried
// by clients and by the transport, and a retry that lands after the first
// join must answer the same thing rather than a failure the player would read
// as "you are not in".
func (component *RosterComponent) Join(playerID, nowUnix int64) (int32, error) {
	dao := component.Owner().Dao()
	if dao.GetName() == "" {
		return 0, errcode.Wrap(apperrors.ErrGuildMissing, nil, "guild_id", component.Owner().ID())
	}
	if _, member := dao.GetMembers(playerID); member {
		return int32(dao.MembersLen()), nil
	}
	if dao.MembersLen() >= MaxMembers {
		return int32(dao.MembersLen()), errcode.Wrap(apperrors.ErrGuildFull, nil,
			"guild_id", component.Owner().ID(), "max", MaxMembers)
	}
	dao.SetMembers(playerID, nowUnix)
	return int32(dao.MembersLen()), nil
}

// Info is the guild as a handler result: a value that crosses the Nest
// boundary, so the endpoint never touches the entity outside its lock.
type Info struct {
	GuildID   int64
	Name      string
	Members   int32
	FounderID int64
	CreatedAt int64
}

// Snapshot reads the guild for a caller holding the lock.
func (component *RosterComponent) Snapshot() Info {
	dao := component.Owner().Dao()
	return Info{
		GuildID: component.Owner().ID(), Name: dao.GetName(),
		Members: int32(dao.MembersLen()), FounderID: dao.GetFounderID(),
		CreatedAt: dao.GetCreatedAtUnix(),
	}
}

// Member reports whether a player is in this guild.
func (component *RosterComponent) Member(playerID int64) bool {
	_, member := component.Owner().Dao().GetMembers(playerID)
	return member
}
