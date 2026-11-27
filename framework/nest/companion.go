package nest

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// ErrCompanionChangeUnsupported 表示本次事务不能在持原锁时取得明确的本地提交结论。
var ErrCompanionChangeUnsupported = errors.New("nest: companion transition requires a definite in-lock commit without Remote data writes")

func hasCompanionTarget(es []entity.IThreadSafeEntity) bool {
	for _, e := range es {
		if e != nil && entity.IsCompanionParticipant(e.GetEntityKind()) {
			return true
		}
	}
	return false
}

// 每次按不可变的关系视图构造一个锁计划。Touch 固定实体实例的生存期，不代替取锁和取得全体锁后的代际复核。
// roots 只含物理域代表；members 还含完整逻辑参与者（分别进 Guard、事务与 Sync）。
type companionPlan struct {
	snapshots               []entity.CompanionDomainSnapshot
	roots, members, touched []entity.IThreadSafeEntity
}

func (p *companionPlan) prepare(explicit []entity.IThreadSafeEntity) error {
	seen := make(map[entity.IThreadSafeEntity]bool)
	roots := make(map[entity.IThreadSafeEntity]bool)
	add := func(e entity.IThreadSafeEntity) bool {
		if e == nil {
			return false
		}
		if seen[e] {
			return true
		}
		if !e.Touch() {
			return false
		}
		p.touched = append(p.touched, e)
		seen[e] = true
		p.members = append(p.members, e)
		return true
	}
	for _, e := range explicit {
		if e == nil {
			continue
		}
		s := entity.SnapshotCompanionDomain(e)
		p.snapshots = append(p.snapshots, s)
		if !add(e) || !add(s.Root) {
			return ErrEntityLockGroupChanged
		}
		if !roots[s.Root] {
			roots[s.Root] = true
			p.roots = append(p.roots, s.Root)
		}
		for _, m := range s.Members {
			if !add(m) {
				return ErrEntityLockGroupChanged
			}
		}
	}
	SortEntity(p.roots)
	return nil
}
func (p *companionPlan) untouch() {
	for _, e := range p.touched {
		defer e.UnTouch()
	}
}

func (mgr *NestMgr) lockCompanionPlan(p *companionPlan, guard *entity.EntityGuard) (release func(), err error) {
	groups := captureDispatchGroupSnapshots(p.members, nil)
	if err := rejectPendingEntityGroupTransition(p.members); err != nil {
		return nil, err
	}
	groupID, err := resolveDispatchGroupIDFromSnapshots(groups)
	if err != nil {
		return nil, err
	}
	var releaseGroup func()
	if groupID != 0 {
		locks := mgr.groupLockManager()
		ge, ok := locks.acquire(groupID)
		if !ok {
			return nil, ErrLockTimeout
		}
		restore := pushEntityLockGroupScope(groupID, groupStoreOf(mgr.getter))
		releaseGroup = func() { defer locks.release(ge); restore() }
	}
	var acquired []entity.IThreadSafeEntity
	cleanup := func() {
		if releaseGroup != nil {
			defer releaseGroup()
		}
		for _, e := range acquired {
			defer guard.ReleaseEntityInstance(entitypermit.System(), e)
		}
	}
	handed := false
	defer func() {
		if !handed {
			cleanup()
		}
	}()
	lock := func(e entity.IThreadSafeEntity) bool {
		if guard.GuardedEntity(e) {
			return true
		}
		if !guard.TryRequireEntity(entitypermit.System(), e) {
			return false
		}
		acquired = append(acquired, e)
		return true
	}
	// 全部代表先按全局锁序 Try；登记逻辑别名时不再取第二把物理锁。任何过时计划都不能阻塞等待。
	for _, e := range p.roots {
		if !lock(e) {
			return nil, ErrLockTimeout
		}
	}
	for _, e := range p.members {
		if !lock(e) {
			return nil, ErrLockTimeout
		}
	}
	for _, s := range p.snapshots {
		if !s.Valid() {
			return nil, ErrEntityLockGroupChanged
		}
	}
	if err := validateDispatchGroupSnapshots(groups); err != nil {
		return nil, err
	}
	handed = true
	return cleanup, nil
}

// companionCall 只活在这一段业务函数中，既不是 goroutine 上可继承的授权，也不延伸到提交回调。
type companionCall struct {
	members []entity.IThreadSafeEntity
	scope   *entity.GuardScope
	tx      *RollbackTx
}

func (c *companionCall) permits(e entity.IThreadSafeEntity) bool {
	return c != nil && c.scope == entity.CurrentGuardScope() && c.tx == CurrentRollbackTx() && slices.Contains(c.members, e) && c.scope.Guard().GuardedEntity(e)
}

func companionSubjectView(es, members []entity.IThreadSafeEntity) []entity.IThreadSafeEntity {
	var subject entity.IThreadSafeEntity
	for _, e := range es {
		if e == nil {
			continue
		}
		if subject != nil && e != subject {
			return nil
		}
		subject = e
	}
	if subject == nil {
		return nil
	}
	out := []entity.IThreadSafeEntity{subject}
	for _, e := range members {
		if e != subject {
			out = append(out, e)
		}
	}
	return out
}

func (mgr *NestMgr) dispatchCompanions(entry handlerEntry, name string, es []entity.IThreadSafeEntity, params []any, opts ...HandlerOption) (ret any, err error) {
	guard, err := dispatchScopeGuard()
	if err != nil {
		return nil, err
	}
	var plan companionPlan
	defer plan.untouch()
	if err = plan.prepare(es); err != nil {
		return nil, err
	}
	unlock, err := mgr.lockCompanionPlan(&plan, guard)
	if err != nil {
		return nil, err
	}
	release := onceRelease(unlock)
	defer func() {
		if r := recoverRelease(release); r != nil {
			err = joinRecoveredError(err, r)
		}
	}()
	msg := currentNestDispatchMsg()
	if err = validateCompanionRemote(msg, &plan); err != nil {
		return nil, err
	}
	view := companionSubjectView(es, plan.members)
	var changes []*entity.CompanionChange
	defer func() {
		for _, change := range changes {
			defer change.Release()
		}
	}()
	var memoryChanges []*entity.CompanionChange
	stage := func(change *entity.CompanionChange) error {
		if entry.meta.Durability == DurabilityPipelined {
			return ErrCompanionChangeUnsupported
		}
		// 传统 LockGroup 的迁移与关系迁移不合并成同一个状态机；遇到分组的实体拒绝迁移，不用其组锁代替事务授权。
		for _, e := range []entity.IThreadSafeEntity{change.Principal, change.Companion} {
			if e.Base().GroupLockID() != 0 || e.Base().GroupTransitionPending() {
				return ErrCompanionChangeUnsupported
			}
		}
		if companionRemote(change.Principal) || companionRemote(change.Companion) || msg != nil && msg.RemoteWriteBatch != nil {
			if msg == nil || msg.RemoteWriteBatch == nil || entry.meta.Rollback == RollbackNone || CurrentRollbackTx() == nil {
				return ErrCompanionChangeUnsupported
			}
			if _, ok := msg.RemoteWriteBatch.(emptyCompanionConfirmer); !ok {
				return ErrCompanionChangeUnsupported
			}
			for _, e := range []entity.IThreadSafeEntity{change.Principal, change.Companion} {
				if companionRemote(e) && !exclusiveCompanionGrant(msg, e) {
					return fmt.Errorf("%w: companion %d", entity.ErrRemoteFenced, e.GUId())
				}
			}
			msg.companionChangePending = true
		}
		changes = append(changes, change)
		if tx := CurrentRollbackTx(); tx != nil {
			tx.companionChanges = append(tx.companionChanges, change)
		} else {
			memoryChanges = append(memoryChanges, change)
		}
		return nil
	}
	earlyRelease := release
	if msg != nil && msg.Type == MsgTypeBroadcast {
		earlyRelease = nil
	}
	ret, err = mgr.invokeHandlerTransaction(entry, plan.members, name, earlyRelease, func() (any, error) {
		if msg != nil {
			prev := msg.companionCall
			msg.companionCall = &companionCall{members: plan.members, scope: entity.CurrentGuardScope(), tx: CurrentRollbackTx()}
			msg.handlerEntities = es
			defer func() { msg.companionCall = prev; msg.handlerEntities = nil }()
		}
		defer entity.PushCompanionScope(entitypermit.System(), view, stage)()
		v, err := entry.handler(es, params, opts...)
		if err == nil && len(changes) > 0 {
			err = mgr.FenceError()
		}
		return v, err
	})
	if err == nil {
		for _, c := range memoryChanges {
			c.Apply()
		}
	}
	return ret, err
}

// 与 sync.Once.Do 一样，release panic 后不能重试（尤其是提前交出锁的 pipelined 分支）。只在同一 worker 使用。
func onceRelease(release func()) func() {
	done := false
	return func() {
		if !done {
			done = true
			release()
		}
	}
}
func companionRemote(e entity.IThreadSafeEntity) bool {
	if e == nil {
		return false
	}
	_, remote := e.(entity.IThreadSafeRemoteEntity)
	return remote || entity.ResolveEntityID(e.GUId()).RemoteCapable || entity.IsEntityKindRemoteCapable(e.GetEntityKind())
}

func validateCompanionRemote(msg *Msg, p *companionPlan) error {
	for _, s := range p.snapshots {
		for _, e := range s.Members {
			if !companionRemote(e) {
				continue
			}
			if !shouldPrepareRemoteID(entity.ResolveEntityID(e.GUId())) {
				return fmt.Errorf("%w: companion %d not locally managed", entity.ErrRemoteWriteCapabilityDisabled, e.GUId())
			}
			if entity.RemoteKindShared(e.GetEntityKind()) {
				return fmt.Errorf("%w: shared companion %d", entity.ErrRemoteFenced, e.GUId())
			}
			if !remoteBatchCovers(msg, e.GUId()) {
				if msg != nil && msg.Type != MsgTypeBroadcast && msg.prepared == nil && msg.RemoteWriteBatch == nil && msg.engine != nil && msg.engine.dispatcher != nil && msg.engine.dispatcher.remoteHandler != nil {
					msg.HasRemote = true
					return declaredTargetCold(entity.ErrRemoteWriteCapabilityDisabled)
				}
				return fmt.Errorf("%w: companion %d not in fenced batch", entity.ErrRemoteWriteCapabilityDisabled, e.GUId())
			}
			if !exclusiveCompanionGrant(msg, e) {
				return fmt.Errorf("%w: companion %d lacks a current exclusive grant", entity.ErrRemoteFenced, e.GUId())
			}
		}
	}
	for _, g := range msgGrants(msg) {
		if !g.snapshot.Valid() || !slices.Contains(p.members, g.instance) {
			return ErrEntityLockGroupChanged
		}
	}
	return nil
}

type companionGrant struct {
	instance entity.IThreadSafeEntity
	snapshot entity.CompanionDomainSnapshot
}

// 由取得真实写门的批次作证（实例、owner、写模式、fence 与代际）；接口缺失即拒绝。
type exclusiveCompanionChecker interface {
	AllowsExclusiveCompanionLocked(entity.IThreadSafeRemoteEntity) bool
}
type emptyCompanionConfirmer interface{ ConfirmEmptyCompanionLocked() bool }

func exclusiveCompanionGrant(m *Msg, e entity.IThreadSafeEntity) bool {
	if !remoteBatchCovers(m, e.GUId()) {
		return false
	}
	remote, ok := e.(entity.IThreadSafeRemoteEntity)
	checker, capable := m.RemoteWriteBatch.(exclusiveCompanionChecker)
	return ok && capable && !entity.RemoteKindShared(e.GetEntityKind()) && shouldPrepareRemoteID(entity.ResolveEntityID(e.GUId())) && checker.AllowsExclusiveCompanionLocked(remote)
}
func msgGrants(m *Msg) map[int64]companionGrant {
	if m == nil {
		return nil
	}
	return m.companionGrants
}

// I/O 阶段只用已加载实例发现候选；ID 计算、候选快照都不是授权。PrepareRemoteWriteBatch 一次取得全部写门，
// 最后业务锁内由该批次核对实例、状态和写租约。缺少任何一项均不进入 handler。
func declareCompanionRemote(msg *Msg) error {
	for _, id := range dispatchIDs(msg) {
		meta := entity.ResolveEntityID(id)
		if !entity.IsCompanionParticipant(meta.Kind) {
			continue
		}
		e, err := msg.getter.Get(entity.WithLoadedEntitiesOnly(nestBaseContext()), meta.FullID, meta.Category)
		if errors.Is(err, entity.ErrColdLoadInLogic) {
			continue
		}
		if err != nil {
			return err
		}
		if e == nil {
			continue
		}
		if !e.Touch() {
			return ErrEntityLockGroupChanged
		}
		msg.companionRefs.touched = append(msg.companionRefs.touched, e)
		s := entity.SnapshotCompanionDomain(e)
		for _, m := range s.Members {
			if !m.Touch() {
				return ErrEntityLockGroupChanged
			}
			msg.companionRefs.touched = append(msg.companionRefs.touched, m)
			if !companionRemote(m) {
				continue
			}
			if msg.Type == MsgTypeBroadcast {
				return ErrRemoteBroadcastUnsupported
			}
			mid := m.GUId()
			if !shouldPrepareRemoteID(entity.ResolveEntityID(mid)) || entity.RemoteKindShared(m.GetEntityKind()) {
				return fmt.Errorf("%w: remote companion %d", entity.ErrRemoteFenced, mid)
			}
			if msg.companionGrants == nil {
				msg.companionGrants = make(map[int64]companionGrant)
			}
			msg.companionGrants[mid] = companionGrant{instance: m, snapshot: s}
			msg.companionRemoteIDs = append(msg.companionRemoteIDs, mid)
			msg.HasRemote = true
		}
	}
	return nil
}
