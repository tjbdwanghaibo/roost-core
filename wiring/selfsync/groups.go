package selfsync

import (
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
)

// GroupsConfig 是 NewGroups 的输入：一类“按组推送”的实体。
type GroupsConfig struct {
	// Kind 是组实体的 kind（房间、队伍、副本……）：组 ID 就是这个实体的 ID。必填，不能是玩家实体的 kind。
	Kind entity.EntityKind
	// Profile 是成员看组实体的视图；组实体的 packer 必须认识它（例如派对房间的 Profile{Key: "room"} 只放出记分板）。
	// Key 必填：组视图的字段权限由 packer 按 Key 决定，空 Key 就是默认视图，容易把不该给成员看的字段放出去。
	Profile entity.SyncProfile
	// MaxMembers 是每组成员上限，0 表示不限。业务自己的座位规则仍是第一道检查，这里是兜底。
	MaxMembers int
}

// Groups 是 self_sync 之上的“按组同步”：某类实体（如 PartyRoom）的同步视图推给组里的成员（房间里的玩家），
// 与自身视图共用同一个 Manager、同一条玩家连接、同一个推送编号。客户端在同一条 Sync 流里按每个更新自带的
// 命名空间（实体标记的 syncNamespace）与视图键区分两路：自身视图是 Player 的命名空间 + 视图 "self"，组视图是
// 组实体的命名空间 + Profile.Key。
//
// 生命周期在 framework/sync/entitysync/policy.GroupView；这里只做两件事：
//
//   - 装配：Config.Groups 交给 New，Nest Mod Init 建 Manager 时与自身视图一起装上（政策阶段排在自身视图之后，
//     会话打开的同一个阶段成员就能订阅）；组实体从权威加载时转交 EntityLoaded。
//   - 提交边界：Join / Leave / Disband 在 Nest 事务里调用时推迟到事务提交之后（nest.AfterCommit），事务回滚时
//     什么都不发生；事务外调用立即记下。三者都只记事实、不阻塞，可以在 handler、心跳、定时器回调里直接调用。
//
// 框架保证：成员关系在提交后生效；成员断线（会话关闭）后保留成员关系、不再推送，重连的那个同步周期收到组实体的
// 全量；顶号（第二条连接）随新 epoch 全量重发；组实体卸载后重新加载、被回滚撤回后重新出现都会重新登记；Disband
// 后成员收到 remove。业务负责：谁是成员（座位规则）、什么时候 Leave（宽限期满、离开房间）、什么时候 Disband
// （房间关闭、销毁实体之前）；成员关系不持久，进程重启后在玩家重新连接时再 Join。
type Groups struct {
	config GroupsConfig
	view   atomic.Pointer[policy.GroupView]
}

// ErrGroupsNotAttached：Groups 还没有装到自身视图的 Manager 上（没交给 selfsync.Config.Groups，或 Nest Mod 还没 Init）。
var ErrGroupsNotAttached = errors.New("selfsync: groups are not attached to a self_sync entity sync manager")

// NewGroups 校验配置。返回的 Groups 交给 selfsync.Config.Groups。
func NewGroups(config GroupsConfig) (*Groups, error) {
	if config.Kind == 0 {
		return nil, errors.New("selfsync: group entity kind is required")
	}
	if config.Profile.Key == "" {
		return nil, errors.New("selfsync: group view key is required (the packer decides which fields members see by it)")
	}
	if config.MaxMembers < 0 {
		return nil, fmt.Errorf("selfsync: group member limit %d is negative", config.MaxMembers)
	}
	return &Groups{config: config}, nil
}

// MustNewGroups 是 NewGroups，配置错误时 panic；给包级变量用。
func MustNewGroups(config GroupsConfig) *Groups {
	created, err := NewGroups(config)
	if err != nil {
		panic(err)
	}
	return created
}

// Kind 是组实体的 kind。
func (g *Groups) Kind() entity.EntityKind { return g.config.Kind }

// Join 让玩家（玩家 ID，即自身视图的会话 ID、gateway Principal.PlayerID；玩家实体 ID 用
// entity.GetUniqueIDFromEntityID 换）成为组 group（组实体的 ID）的成员。在 Nest 事务里调用时，组 ID、玩家 ID、
// 组上限在调用时检查并返回错误，成员关系在事务提交之后才记下；提交时组已满（并发的另一次 Join 先提交）只记 Warn。
// 事务外调用立即记下。重复 Join 是无变化的成功。
func (g *Groups) Join(group, player int64) error {
	if player == 0 {
		return errors.New("selfsync: player id 0 is invalid")
	}
	view, err := g.target(group, player)
	if err != nil {
		return err
	}
	if g.config.MaxMembers > 0 && !view.IsMember(group, player) && view.Members(group) >= g.config.MaxMembers {
		return fmt.Errorf("selfsync: group %d: %w", group, policy.ErrGroupMemberLimit)
	}
	join := func() error { return view.Join(group, player) }
	if corenest.AfterCommit(func() {
		if err := join(); err != nil {
			slog.Warn("selfsync: group join after commit refused", "group", group, "player_id", player, "err", err)
		}
	}) {
		return nil
	}
	return join()
}

// Leave 让玩家离开组 group：之后的同步周期里它收到组实体的 remove。Nest 事务里调用时推迟到提交之后。
func (g *Groups) Leave(group, player int64) error {
	if player == 0 {
		return errors.New("selfsync: player id 0 is invalid")
	}
	view, err := g.target(group, player)
	if err != nil {
		return err
	}
	leave := func() { view.Leave(group, player) }
	if !corenest.AfterCommit(leave) {
		leave()
	}
	return nil
}

// Disband 解散组 group：全部成员收到 remove，组实体的登记被注销。Nest 事务里调用时推迟到提交之后。组实体随后
// 可以销毁（先销毁也安全）；之后同 ID 再 Join 是一个新组。
func (g *Groups) Disband(group int64) error {
	view, err := g.target(group, 0)
	if err != nil {
		return err
	}
	disband := func() { view.Disband(group) }
	if !corenest.AfterCommit(disband) {
		disband()
	}
	return nil
}

// Members 报告组 group 已记下的成员数（不含尚未提交的 Join）；未装配时为 0。
func (g *Groups) Members(group int64) int { return g.view.Load().Members(group) }

// IsMember 报告玩家是否已是组 group 的成员（不含尚未提交的 Join）；未装配时为 false。
func (g *Groups) IsMember(group, player int64) bool { return g.view.Load().IsMember(group, player) }

// target 校验 ID 并取出已装配的组视图；player 为 0 表示不涉及玩家（Disband）。
func (g *Groups) target(group, player int64) (*policy.GroupView, error) {
	if group == 0 || entity.GetEntityKindFromID(group) != g.config.Kind {
		return nil, fmt.Errorf("selfsync: %d is not an entity of the group kind %d: %w", group, g.config.Kind, entitysync.ErrSubjectInvalid)
	}
	if player < 0 {
		return nil, fmt.Errorf("selfsync: player id %d is invalid", player)
	}
	view := g.view.Load()
	if view == nil {
		return nil, ErrGroupsNotAttached
	}
	return view, nil
}

// attach 在自身视图之后把组视图装到同一个 Manager 上；同一个 Groups 只能装到一个 Manager。
func (g *Groups) attach(manager *entitysync.Manager, lookup func(int64) *entity.SubjectSyncState) error {
	view, err := policy.NewGroupView(policy.GroupViewConfig{Manager: manager, Profile: g.config.Profile, Lookup: lookup, MaxMembers: g.config.MaxMembers})
	if err != nil {
		return err
	}
	if !g.view.CompareAndSwap(nil, view) {
		view.Close()
		return fmt.Errorf("selfsync: groups of kind %d are already attached to an entity sync manager", g.config.Kind)
	}
	return nil
}

func (g *Groups) detach() {
	if view := g.view.Swap(nil); view != nil {
		view.Close()
	}
}
