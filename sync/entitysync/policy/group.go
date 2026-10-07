package policy

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// GroupConfig shapes a Group.
type GroupConfig struct {
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// Profile every member subscribes with. Zero is the default profile.
	Profile entity.SyncProfile
	// Session names a member's session. Nil means the session id IS the
	// member id.
	Session func(member int64) entitysync.SessionID
	// MaxSubjects and MaxMembers bound the group. Zero means unbounded (the
	// manager's own limits still apply).
	MaxSubjects int
	MaxMembers  int
}

var (
	ErrGroupClosed         = errors.New("policy: group is closed")
	ErrGroupSubjectLimit   = errors.New("policy: group subject limit reached")
	ErrGroupMemberLimit    = errors.New("policy: group member limit reached")
	ErrGroupSubjectPresent = errors.New("policy: subject is already in the group")
	ErrGroupMemberPresent  = errors.New("policy: member is already in the group")
	ErrGroupNotPresent     = errors.New("policy: not in the group")
)

// Group 组织成员与实体的全互见关系：每个成员订阅集合中的每个实体。
// 两者是独立集合；观战者只加入成员，场景 NPC 只加入实体，玩家通常两者都加入。
// 会话由 Manager 管理，Group 只维护成员关系并调用订阅接口。
type Group struct {
	mu            sync.Mutex
	config        GroupConfig
	subscriptions *entitysync.SubscriptionSource
	session       func(int64) entitysync.SessionID
	subjects      map[int64]struct{}
	members       map[int64]struct{}
	closed        bool
}

func NewGroup(config GroupConfig) (*Group, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	session := config.Session
	if session == nil {
		session = func(member int64) entitysync.SessionID { return entitysync.SessionID(member) }
	}
	g := &Group{config: config, session: session, subjects: make(map[int64]struct{}), members: make(map[int64]struct{})}
	g.subscriptions = config.Manager.NewSubscriptionSourceWithResubmit(g.resubmit)
	return g, nil
}

// resubmit 是 Group 的重新提交入口（RR-20260926-70）：框架撤销（RR-59 退回 remove）的本组订阅在同 ID 实体重新
// 登记后交还这里。实体仍在组里、会话仍属于某个成员时以本组视图重新订阅；缺席期间 RemoveSubject / Leave 过的不恢复。
// 没有调用方可以报告拒绝（会话已关闭、容量不足、实体又被注销），记一条 Warn 后放弃，与 AddSubject 失败需调用方重做同一口径。
func (g *Group) resubmit(retracted []entitysync.RetractedSubscription) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	sessions := make(map[entitysync.SessionID]struct{}, len(g.members))
	for member := range g.members {
		sessions[g.session(member)] = struct{}{}
	}
	for _, subscription := range retracted {
		if _, inGroup := g.subjects[subscription.Subject]; !inGroup {
			continue
		}
		if _, member := sessions[subscription.Session]; !member {
			continue
		}
		if err := g.subscriptions.Subscribe(subscription.Session, subscription.Subject, g.config.Profile); err != nil {
			flog.Warn("policy: group could not resubmit a subscription the framework retracted", "session", subscription.Session, "subject", subscription.Subject, "err", err)
		}
	}
}

// AddSubject registers a subject with the manager and subscribes every
// current member to it.
func (g *Group) AddSubject(state *entity.SubjectSyncState) error {
	if state == nil {
		return entitysync.ErrSubjectInvalid
	}
	id := state.SubjectID()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrGroupClosed
	}
	if _, present := g.subjects[id]; present {
		return ErrGroupSubjectPresent
	}
	if g.config.MaxSubjects > 0 && len(g.subjects) >= g.config.MaxSubjects {
		return ErrGroupSubjectLimit
	}
	if err := g.config.Manager.Register(state); err != nil && !errors.Is(err, entitysync.ErrSubjectRegistered) {
		return err
	}
	var errs []error
	for member := range g.members {
		if err := g.subscriptions.Subscribe(g.session(member), id, g.config.Profile); err != nil {
			errs = append(errs, fmt.Errorf("member %d: %w", member, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		for member := range g.members {
			_ = g.subscriptions.Unsubscribe(g.session(member), id)
		}
		return err
	}
	g.subjects[id] = struct{}{}
	return nil
}

// RemoveSubject 释放本组对实体的订阅。实体真正销毁时由调用方 Manager.Unregister。
func (g *Group) RemoveSubject(id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, present := g.subjects[id]; !present {
		return ErrGroupNotPresent
	}
	delete(g.subjects, id)
	return g.releaseSubject(id)
}

// releaseSubject 由持有 g.mu 的调用方使用；不触碰其他政策的来源。
func (g *Group) releaseSubject(id int64) error {
	var errs []error
	for member := range g.members {
		if err := g.subscriptions.Unsubscribe(g.session(member), id); err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			errs = append(errs, fmt.Errorf("member %d: %w", member, err))
		}
	}
	return errors.Join(errs...)
}

// Join makes a session a member: it receives every subject in the group. The
// session must already be open with the manager.
// 重复 Join 保留成员关系并重新提交订阅，用于 Manager 丢会话后重开。
func (g *Group) Join(member int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrGroupClosed
	}
	_, present := g.members[member]
	if !present && g.config.MaxMembers > 0 && len(g.members) >= g.config.MaxMembers {
		return ErrGroupMemberLimit
	}
	session := g.session(member)
	var errs []error
	for id := range g.subjects {
		if err := g.subscriptions.Subscribe(session, id, g.config.Profile); err != nil {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		if !present {
			for id := range g.subjects {
				_ = g.subscriptions.Unsubscribe(session, id)
			}
		}
		return err
	}
	g.members[member] = struct{}{}
	return nil
}

// Leave takes a member out: it stops receiving the group's subjects.
func (g *Group) Leave(member int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, present := g.members[member]; !present {
		return ErrGroupNotPresent
	}
	delete(g.members, member)
	session := g.session(member)
	var errs []error
	for id := range g.subjects {
		if err := g.subscriptions.Unsubscribe(session, id); err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Close 释放本组的订阅与成员。会话及实体注册仍由 Manager 和业务生命周期管理。
func (g *Group) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	var errs []error
	for id := range g.subjects {
		if err := g.releaseSubject(id); err != nil {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	clear(g.subjects)
	clear(g.members)
	return errors.Join(errs...)
}

// Members and Subjects report the group's size.
func (g *Group) Members() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.members)
}

func (g *Group) Subjects() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.subjects)
}
