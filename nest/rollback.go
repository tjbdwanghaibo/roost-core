package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
)

type RollbackPolicy uint8

const (
	RollbackNone RollbackPolicy = iota
	RollbackState
	RollbackUndo
)

func ParseRollbackPolicy(value string) (RollbackPolicy, error) {
	switch value {
	case "", "none":
		return RollbackNone, nil
	case "state":
		return RollbackState, nil
	case "undo":
		return RollbackUndo, nil
	default:
		return RollbackNone, fmt.Errorf("nest: unsupported rollback policy %q", value)
	}
}

func (p RollbackPolicy) String() string {
	switch p {
	case RollbackNone:
		return "none"
	case RollbackState:
		return "state"
	case RollbackUndo:
		return "undo"
	default:
		return "invalid"
	}
}

type HandlerMeta struct {
	Rollback   RollbackPolicy
	Durability DurabilityPolicy
}

// RollbackParticipant can be implemented by an entity, component, or DAO that
// needs custom state rollback beyond the generated DAO snapshot fallback.
type RollbackParticipant interface {
	CaptureRollback(tx *RollbackTx) error
}

// RollbackTx 保存业务回滚与持久化参与者，执行时序由 execution.go 管理。
// 准入前归持锁的业务 goroutine；pipelined 转交完成池后，业务侧不再操作该对象。
type RollbackTx struct {
	id                  TransactionID
	policy              RollbackPolicy
	durability          DurabilityPolicy
	handler             string
	state               rollbackTxState
	rollbacks           []func() error
	afterAdmission      []func()
	admissionErr        error
	stageMetrics        bool
	admissionObserved   bool
	syncMutation        *entity.SyncMutation
	commits             []func()
	undoKeys            map[undoKey]struct{}
	participants        []CommitParticipant
	participantSet      map[CommitParticipant]struct{}
	participantChanges  map[MutationParticipant]*PersistChange
	participantOrder    []MutationParticipant
	remoteParticipants  map[MutationParticipant]struct{}
	preparedMutations   map[MutationParticipant]dataengine.Mutation
	persistencePrepared bool
	accepted            bool
	mutations           []EntityMutation
	mutationKeys        map[mutationKey]struct{}
	effects             []Effect
	effectIDs           map[string]struct{}
	receipts            []dataengine.Receipt
	receiptDigests      map[receiptKey][]byte
	remoteWrite         bool
	deleteIntents       map[int64]struct{}
	// created 是 handler 内 CreateInScope 新建的实体，准入前再次加入 SyncMutation（RR-20260926-35）。
	created []entity.IThreadSafeEntity
	// createLockBusy 是 handler 内新建实体时第一次锁冲突的错误（RR-20260926-48）。可回滚的事务据此在
	// handler 结束时整条回滚并以锁超时重新准入，即使业务吞掉了 Create 返回的错误。
	createLockBusy error
	// captureFailed 是 handler 内动态纳入事务的实体（CreateInScope 新建，RR-20260927-11；Cast 取得，RR-20260927-31）第一次
	// 事务捕获失败的错误。与 createLockBusy 同一种处理：handler 结束时即使业务吞掉了返回的错误也整条回滚——捕获失败前可能
	// 已登记了提交参与者，照常提交会让它们准备记录，本事务的其他修改带着一个未进入回滚 / 持久化参与的实体持久化。
	captureFailed error
	// dispatch 是以本事务为自身事务的派发消息（嵌套的 RunIsolatedTransaction / 无派发调用为 nil）；
	// 越过提交点时在它上面记录，dispatchNest 据此不再重新准入（RR-20260926-49）。
	dispatch *Msg
	// enclosing 是本事务开始时已在执行的外层事务（handler 内嵌套的 RunIsolatedTransaction 才有；消息自己的事务为 nil）。
	// snapshotted 是本事务以可回滚策略捕获（登记了快照 / tracker 恢复）的实体。嵌套事务提交前据此拒绝写外层会回滚的实体
	// （RR-20260926-74）。
	enclosing   *RollbackTx
	snapshotted []entity.IThreadSafeEntity
}

type rollbackTxState uint8

const (
	rollbackTxOpen rollbackTxState = iota
	rollbackTxCommitted
	rollbackTxRolledBack
)

type undoKey struct {
	owner any
	field uint64
	token any
}

type mutationKey struct {
	database string
	resource string
	entityID int64
}

type receiptKey struct {
	namespace string
	id        string
}

func NewRollbackTx(policy RollbackPolicy) *RollbackTx {
	return &RollbackTx{id: newTransactionID(), policy: policy}
}

func (tx *RollbackTx) ID() TransactionID {
	if tx == nil {
		return TransactionID{}
	}
	return tx.id
}

func (tx *RollbackTx) Policy() RollbackPolicy {
	if tx == nil {
		return RollbackNone
	}
	return tx.policy
}

func (tx *RollbackTx) DeferRollback(fn func() error) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.rollbacks = append(tx.rollbacks, fn)
	}
}

// RecordUndo records at most one inverse operation for owner/field in this
// transaction. owner must be comparable; generated code passes a DAO pointer.
func (tx *RollbackTx) RecordUndo(owner any, field uint64, fn func() error) error {
	return tx.RecordUndoToken(owner, field, nil, fn)
}

// RecordUndoToken records an inverse operation for a field sub-resource. The
// token lets generated collection setters independently capture multiple map
// keys while still coalescing repeated writes to the same key.
func (tx *RollbackTx) RecordUndoToken(owner any, field uint64, token any, fn func() error) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if owner == nil || fn == nil {
		return errors.New("nest: invalid undo operation")
	}
	t := reflect.TypeOf(owner)
	if !t.Comparable() {
		return errors.New("nest: undo owner is not comparable")
	}
	if token != nil && !reflect.TypeOf(token).Comparable() {
		return errors.New("nest: undo token is not comparable")
	}
	if tx.undoKeys == nil {
		tx.undoKeys = make(map[undoKey]struct{}, 8)
	}
	key := undoKey{owner: owner, field: field, token: token}
	if _, exists := tx.undoKeys[key]; exists {
		return nil
	}
	tx.undoKeys[key] = struct{}{}
	tx.rollbacks = append(tx.rollbacks, fn)
	return nil
}

// RecordUndo adds an inverse operation to the active Nest transaction. It
// returns false outside a rollback=undo handler.
func RecordUndo(owner any, field uint64, fn func() error) bool {
	tx := CurrentRollbackTx()
	if tx == nil || tx.policy != RollbackUndo {
		return false
	}
	return tx.RecordUndo(owner, field, fn) == nil
}

// RecordUndoToken is the active-transaction counterpart of
// (*RollbackTx).RecordUndoToken.
func RecordUndoToken(owner any, field uint64, token any, fn func() error) bool {
	tx := CurrentRollbackTx()
	if tx == nil || tx.policy != RollbackUndo {
		return false
	}
	return tx.RecordUndoToken(owner, field, token, fn) == nil
}

func (tx *RollbackTx) AfterCommit(fn func()) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.commits = append(tx.commits, fn)
	}
}

// AfterAdmission runs after the transaction has crossed its durable admission
// point but before Nest releases entity locks. It is reserved for lifecycle
// transitions, such as removing an Entity whose delete tombstone is already
// in the WAL. External side effects belong in AfterCommit instead.
// pipelined 的准入早于 ticket 持久化确认；这里可冻结同步内容，但不能提前外发。
func (tx *RollbackTx) AfterAdmission(fn func()) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.afterAdmission = append(tx.afterAdmission, fn)
	}
}

// RequestEntityDelete records one transaction-local aggregate delete intent.
// The bool is true only for the first request for this entity in the current
// transaction, allowing callers to register one lifecycle finalizer.
func (tx *RollbackTx) RequestEntityDelete(entityID int64) (bool, error) {
	if tx == nil || tx.state != rollbackTxOpen || entityID == 0 {
		return false, ErrTransactionClosed
	}
	if tx.deleteIntents == nil {
		tx.deleteIntents = make(map[int64]struct{}, 1)
	}
	if _, exists := tx.deleteIntents[entityID]; exists {
		return false, nil
	}
	tx.deleteIntents[entityID] = struct{}{}
	if tx.durability < DurabilityStrict {
		tx.durability = DurabilityStrict
	}
	return true, nil
}

// CancelEntityDelete removes an intent that could not be prepared. It is only
// valid before persistence preparation begins.
func (tx *RollbackTx) CancelEntityDelete(entityID int64) {
	if tx == nil || tx.state != rollbackTxOpen || tx.persistencePrepared {
		return
	}
	delete(tx.deleteIntents, entityID)
}

// RemoteDeleteRequested implements entity.RemoteDeleteIntentSource.
func (tx *RollbackTx) RemoteDeleteRequested(entityID int64) bool {
	if tx == nil {
		return false
	}
	_, requested := tx.deleteIntents[entityID]
	return requested
}

func (tx *RollbackTx) RegisterCommitParticipant(participant CommitParticipant) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if isNilCommitParticipant(participant) {
		return nil
	}
	t := reflect.TypeOf(participant)
	if !t.Comparable() {
		return errors.New("nest: commit participant is not comparable")
	}
	if tx.participantSet == nil {
		tx.participantSet = make(map[CommitParticipant]struct{}, 4)
	}
	if _, exists := tx.participantSet[participant]; exists {
		return nil
	}
	tx.participantSet[participant] = struct{}{}
	tx.participants = append(tx.participants, participant)
	return nil
}

func isNilCommitParticipant(participant CommitParticipant) bool {
	if participant == nil {
		return true
	}
	v := reflect.ValueOf(participant)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func (tx *RollbackTx) AddMutation(mutation EntityMutation) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if mutation.Key != (dataengine.DocumentKey{}) {
		if mutation.EntityID != 0 || mutation.Database != "" || mutation.DatabaseScope != 0 || mutation.Resource != "" || mutation.Version != 0 {
			return dataengine.ErrMixedMutationForms
		}
		if err := dataengine.ValidateMutation(mutation); err != nil {
			return err
		}
	} else if mutation.EntityID == 0 || mutation.Resource == "" || (len(mutation.Data) == 0 && mutation.Remote == nil) {
		return errors.New("nest: invalid entity mutation")
	}
	if tx.mutationKeys == nil {
		tx.mutationKeys = make(map[mutationKey]struct{}, 4)
	}
	key := keyOfMutation(mutation)
	if _, exists := tx.mutationKeys[key]; exists {
		return fmt.Errorf("nest: duplicate entity mutation %s/%s/%d", key.database, key.resource, key.entityID)
	}
	tx.mutationKeys[key] = struct{}{}
	tx.mutations = append(tx.mutations, cloneMutation(mutation))
	return nil
}

// keyOfMutation 是 AddMutation 去重用的身份：DocumentKey 形式取 Key，旧形式取 EntityID / Database / Resource。
func keyOfMutation(mutation EntityMutation) mutationKey {
	if mutation.Key != (dataengine.DocumentKey{}) {
		return mutationKey{database: mutation.Key.Database, resource: mutation.Key.Resource, entityID: mutation.Key.ID}
	}
	return mutationKey{database: mutation.Database, resource: mutation.Resource, entityID: mutation.EntityID}
}

func (tx *RollbackTx) Emit(effect Effect) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if effect.Topic == "" {
		return errors.New("nest: effect topic is empty")
	}
	if effect.ID == "" {
		effect.ID = tx.id.String() + ":" + fmt.Sprint(len(tx.effects)+1)
	}
	if tx.effectIDs == nil {
		tx.effectIDs = make(map[string]struct{}, 4)
	}
	if _, exists := tx.effectIDs[effect.ID]; exists {
		return fmt.Errorf("nest: duplicate effect id %q", effect.ID)
	}
	tx.effectIDs[effect.ID] = struct{}{}
	// An outbox item is only useful if its admission is durable before the
	// entity lock is released. Upgrade handlers that did not explicitly select
	// a durability policy instead of silently providing a lossy "outbox".
	if tx.durability == DurabilityMemory {
		tx.durability = DurabilityStrict
	}
	tx.effects = append(tx.effects, cloneEffect(effect))
	return nil
}

func Emit(effect Effect) error {
	tx := CurrentRollbackTx()
	if tx == nil {
		return ErrTransactionClosed
	}
	return tx.Emit(effect)
}

func (tx *RollbackTx) Rollback() error {
	if tx == nil {
		return nil
	}
	if tx.state == rollbackTxRolledBack {
		return nil
	}
	if tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	defer observeNestStage(tx.handler, "rollback", startNestStage(tx.stageMetrics))
	tx.state = rollbackTxRolledBack
	var errs []error
	for i := len(tx.rollbacks) - 1; i >= 0; i-- {
		if tx.rollbacks[i] == nil {
			continue
		}
		if err := tx.rollbacks[i](); err != nil {
			errs = append(errs, err)
		}
	}
	tx.commits = nil
	tx.afterAdmission = nil
	tx.admissionErr = nil
	tx.deleteIntents = nil
	tx.created = nil
	tx.participantChanges = nil
	tx.participantOrder = nil
	tx.remoteParticipants = nil
	tx.preparedMutations = nil
	tx.mutations = nil
	tx.mutationKeys = nil
	tx.effects = nil
	tx.effectIDs = nil
	tx.receipts = nil
	tx.receiptDigests = nil
	return errors.Join(errs...)
}

func (tx *RollbackTx) Commit() error {
	return tx.commit(false)
}

// entitiesReleased 只由 pipelined 在统一释放完成后传 true。
// Guard scope 此时可能仍挂在 dispatch goroutine 上，但已不持锁；不能再把
// AfterCommit 排到 scope 的末尾，否则降级完成会提前回复并跳过回调错误报告。
func (tx *RollbackTx) commit(entitiesReleased bool) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return nil
	}
	tx.state = rollbackTxCommitted
	tx.rollbacks = nil
	tx.undoKeys = nil
	tx.participants = nil
	tx.participantSet = nil
	tx.participantChanges = nil
	tx.participantOrder = nil
	tx.remoteParticipants = nil
	tx.preparedMutations = nil
	tx.mutationKeys = nil
	tx.effectIDs = nil
	tx.receipts = nil
	tx.receiptDigests = nil
	tx.deleteIntents = nil
	// Every callback runs, whatever the previous one did. Commit has
	// already marked the transaction committed and the durable fact is not
	// coming back; a business hook that panics must not cancel the
	// framework's own obligations queued behind it — the
	// TransactionReleased notification is one of these callbacks, and the
	// caller's reply depends on this function returning (RR-20260911-06).
	// 直接完成时把回调异常返回给调用者；Guard/remote 延迟执行的路径仍由各自
	// runner 处理异常，不能从 Commit 返回 nil 推断延迟回调已经全部执行成功。
	tx.runAfterAdmission()
	failed := tx.admissionErr
	tx.admissionErr = nil
	if len(tx.commits) == 0 {
		return failed
	}
	if msg := currentNestDispatchMsg(); msg != nil && msg.RemoteWriteBatch != nil {
		msg.addPostRemoteCommit(tx.commits...)
		return failed
	}
	if scope := entity.CurrentGuardScope(); !entitiesReleased && scope != nil && scope.Guard() != nil {
		// The guard's post-release runner already isolates each callback.
		for _, fn := range tx.commits {
			scope.Guard().AppendPostRelease(fn)
		}
		return failed
	}
	for _, fn := range tx.commits {
		failed = errors.Join(failed, runCommitCallback(fn))
	}
	return failed
}

// runAfterAdmission 在交出 Entity 锁和事务所有权前完成生命周期变更。
// pipelined 已被 WAL 接纳，回调失败不能再回滚；保留错误供最终完成路径报告。
func (tx *RollbackTx) runAfterAdmission() {
	if tx.stageMetrics && !tx.admissionObserved {
		tx.admissionObserved = true
		defer observeNestStage(tx.handler, "admission", time.Now())
	}
	// 业务可能在 CreateInScope 之后才启用新实体的同步状态；准入前再纳入一次（Include 按状态去重），
	// 保证新实体与参数实体、Cast 实体共用同一个提交屏障。
	if len(tx.created) > 0 {
		tx.syncMutation.Include(tx.created)
		tx.created = nil
	}
	callbacks := tx.afterAdmission
	tx.afterAdmission = nil
	for _, fn := range callbacks {
		tx.admissionErr = errors.Join(tx.admissionErr, runCommitCallback(fn))
	}
	tx.syncMutation.Admit()
}

// runCommitCallback runs one after-commit callback inside its own recovery
// boundary and turns a panic into ErrAfterCommitFailed.
func runCommitCallback(fn func()) (err error) {
	if fn == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			// panic 值是 error 时以 %w 保留原因链（RR-20260926-49），调用方仍可 errors.Is 原因。
			if cause, ok := r.(error); ok {
				err = fmt.Errorf("%w: %w", ErrAfterCommitFailed, cause)
			} else {
				err = fmt.Errorf("%w: %v", ErrAfterCommitFailed, r)
			}
			slog.Error("nest after-commit callback panic", "err", r)
		}
	}()
	fn()
	return nil
}

// abandon closes an indeterminate transaction without executing rollback or
// after-commit hooks. The hosting process is expected to stop accepting writes
// and recover the authoritative outcome from WAL.
func (tx *RollbackTx) abandon() {
	if tx == nil || tx.state != rollbackTxOpen {
		return
	}
	tx.state = rollbackTxCommitted
	tx.rollbacks = nil
	tx.afterAdmission = nil
	tx.admissionErr = nil
	tx.commits = nil
	tx.undoKeys = nil
	tx.participants = nil
	tx.participantSet = nil
	tx.participantChanges = nil
	tx.participantOrder = nil
	tx.remoteParticipants = nil
	tx.preparedMutations = nil
	tx.mutationKeys = nil
	tx.effectIDs = nil
	tx.receipts = nil
	tx.receiptDigests = nil
	tx.deleteIntents = nil
	tx.created = nil
}

func (tx *RollbackTx) prepareCommitRecord() (CommitRecord, error) {
	if tx != nil {
		defer observeNestStage(tx.handler, "prepare", startNestStage(tx.stageMetrics))
	}
	if tx == nil || tx.state != rollbackTxOpen {
		return CommitRecord{}, ErrTransactionClosed
	}
	for _, participant := range tx.participants {
		if isNilCommitParticipant(participant) {
			continue
		}
		if err := participant.PrepareCommit(tx); err != nil {
			return CommitRecord{}, fmt.Errorf("nest: prepare commit: %w", err)
		}
	}
	if err := tx.preparePersistence(); err != nil {
		return CommitRecord{}, fmt.Errorf("nest: prepare persistence: %w", err)
	}
	requestID := tx.requestID()
	mutations := make([]EntityMutation, len(tx.mutations))
	for i := range tx.mutations {
		canonical, err := dataengine.CanonicalizeMutation(tx.mutations[i])
		if err != nil {
			return CommitRecord{}, fmt.Errorf("nest: canonicalize mutation %d: %w", i, err)
		}
		mutations[i] = canonical
	}
	record := CommitRecord{
		ID: tx.id, Handler: tx.handler, RequestID: requestID, CreatedAt: time.Now().UnixNano(), Durability: tx.durability,
		Mutations: mutations,
		Effects:   append([]Effect(nil), tx.effects...),
		Receipts:  append([]dataengine.Receipt(nil), tx.receipts...),
	}
	if !record.Empty() {
		if err := validateCommitRecord(record); err != nil {
			return CommitRecord{}, err
		}
	}
	return record, nil
}

func (tx *RollbackTx) requestID() string {
	requestID := ""
	if current := fctx.CurrentContext(); current != nil {
		requestID = current.Trace.TraceID
		if requestID == "" && (current.Meta.PlayerID != 0 || current.Meta.MsgID != 0 || current.Meta.Seq != 0) {
			requestID = fmt.Sprintf("player:%d/msg:%d/seq:%d", current.Meta.PlayerID, current.Meta.MsgID, current.Meta.Seq)
		}
	}
	return requestID
}

func (tx *RollbackTx) durableCommit(ctx context.Context, committer TransactionCommitter) error {
	// Memory-only handlers persist through entity release hooks. Avoid
	// materializing after-images when no WAL/outbox admission is involved.
	if tx.durability == DurabilityMemory && len(tx.effects) == 0 {
		if tx.dispatch != nil && tx.dispatch.RemoteWriteBatch != nil {
			// 带 Remote 批次、没有 effect 的 memory handler：没有记录交给 committer，但返回 nil 会让 commitDurable 置
			// remoteCommitted，finishRemoteWriteBatch 随即以 Durability 0 直写权威。引擎已 fence 时这条写同样要拒绝
			// （RR-20260930-12，维护者收紧 N21）：之前 fence 检查只在下面交给 committer 之前做，这条路径在此就返回了，
			// 别的消息 fence 引擎之后它仍把 Remote 写了。返回 ErrNestFenced 后 commitDurable 按明确拒绝 Abort 批次并
			// 加 ErrCommitRejected；RollbackNone 不撤销内存修改（与 RR-20260927-32 同）。
			if err := tx.refuseCommitAfterFence(); err != nil {
				return err
			}
		}
		return nil
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		return err
	}
	if record.Empty() {
		return nil
	}
	if err := tx.refuseWriteUnderEnclosingRollback(); err != nil {
		return err
	}
	if err := tx.refuseCommitAfterFence(); err != nil {
		return err
	}
	if committer == nil {
		if tx.durability != DurabilityMemory || len(record.Effects) > 0 {
			return ErrCommitterRequired
		}
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	commitStart := startNestStage(tx.stageMetrics)
	commitErr := committer.Commit(ctx, record)
	observeNestStage(tx.handler, "durable_commit", commitStart)
	if err := commitErr; err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			return err
		}
		return errors.Join(ErrCommitRejected, err)
	}
	return tx.acceptPersistence()
}

// refuseWriteUnderEnclosingRollback 在嵌套事务写任何持久记录之前检查（RR-20260926-74）：本事务要持久写的 DAO 若已被外层
// 可回滚事务登记回滚快照，外层随后失败回滚会把快照恢复到内存（RollbackState 值与 tracker 版本、RollbackUndo 的 tracker 版本），
// 覆盖本事务已持久的结果，内存与持久分叉、下一次写以旧 ExpectedVersion 撞冲突。旧实现照常提交。维护者决定拒绝而不是改外层
// 快照的基准：返回 ErrNestedTransactionRollbackConflict，由 commitDurable 按明确拒绝回滚本事务，外层快照不动。
// 外层是 RollbackNone（memory 快路径没有 RollbackTx，带 Remote 批次的 memory handler 不登记快照）时不受影响。
func (tx *RollbackTx) refuseWriteUnderEnclosingRollback() error {
	for outer := tx.enclosing; outer != nil; outer = outer.enclosing {
		if outer.policy == RollbackNone || outer.state != rollbackTxOpen {
			continue
		}
		if id, ok := outer.restoresWriteOf(tx.participantOrder); ok {
			return fmt.Errorf("%w: id %d (nested %q, enclosing %s transaction %q)", ErrNestedTransactionRollbackConflict, id, tx.handler, outer.policy, outer.handler)
		}
		if id, ok := outer.snapshotsRawMutationOf(tx); ok {
			return fmt.Errorf("%w: raw mutation for entity %d (nested %q, enclosing %s transaction %q)", ErrNestedTransactionRollbackConflict, id, tx.handler, outer.policy, outer.handler)
		}
	}
	return nil
}

// snapshotsRawMutationOf 报告 nested 里不经 DAO、由 AddMutation 直接加入的原始 mutation 是否按实体 ID 命中本（外层）事务快照过的
// 实体（RR-20260927-07）。restoresWriteOf 只看 MarkPersist 登记的 DAO 参与方：嵌套事务用导出的 AddMutation 直写外层已快照实体时，
// 旧实现照常提交，外层失败回滚后内存与持久分叉（audit4 探针 B）。原始 mutation 没有 DAO 实例可比，只能按实体 ID
// （DocumentKey 形式取 Key.ID，与 AddMutation 去重的身份一致）。preparePersistence 由 DAO 生成的 mutation 已由 restoresWriteOf
// 按实例判断，这里按去重身份跳过它们：生成 DAO 的 Id() 是 StorageID，不一定等于实体 ID，按 ID 比较会误判。
// 仓内生产调用不受影响：msg.go 的 finalizeRemoteWriteBatch 只在消息自己的事务里调用（enclosing 为 nil，带 Remote 批次的消息里
// 嵌套事务在 runTransaction 入口已被拒绝），persist_change.go 的 preparePersistence 生成的正是被跳过的 DAO mutation。
func (tx *RollbackTx) snapshotsRawMutationOf(nested *RollbackTx) (int64, bool) {
	if len(tx.snapshotted) == 0 {
		return 0, false
	}
	for i := range nested.mutations {
		key := keyOfMutation(nested.mutations[i])
		if nested.preparedMutationKey(key) {
			continue
		}
		for _, e := range tx.snapshotted {
			if e.GUId() == key.entityID {
				return key.entityID, true
			}
		}
	}
	return 0, false
}

// preparedMutationKey 报告 key 是否是 preparePersistence 由 DAO 参与方生成的 mutation。AddMutation 按同一身份去重，所以同一 key
// 在 mutations 里只有一条，命中即说明它来自 DAO。
func (tx *RollbackTx) preparedMutationKey(key mutationKey) bool {
	for _, prepared := range tx.preparedMutations {
		if keyOfMutation(prepared) == key {
			return true
		}
	}
	return false
}

// refuseCommitAfterFence 在消息自己的事务交给 committer 之前检查所在引擎是否已 fence（RR-20260927-06）。handler 内嵌套独立
// 事务结果未知时 RR-76 已 fence 引擎，但外层 handler 会继续执行到结束；结果未知若来自 acceptPersistence（committer 已成功、
// DAO AcceptMutation 失败），真实 WAL 并未进入 terminal，旧实现把外层自己的记录照常交给 committer 并被接受——引擎 fence 之后
// 仍在写。现在返回 ErrNestFenced：记录没有交给 committer，是明确拒绝，调用方（commitDurable / commitPipelined）按拒绝回滚（rejectCommit
// 另加 ErrCommitRejected；RollbackNone 的事务不撤销内存修改，RR-20260927-32）。
// 错误里不带 fence 原因的哨兵（%v）：原因链上的 ErrCommitIndeterminate 会让调用方误走结果未知分支（abandon 而不回滚）。
// 嵌套独立事务（dispatch 为 nil）不在此列：C07 的约束只针对外层自己的提交。只读一次 lifecycleMu 保护的字段，不等待、不做 I/O，
// 与派发入口 runNestLogic 在快 worker 上读 FenceError 相同。
// 带 Remote 批次、没有 effect 的 memory handler 没有记录交给 committer，但它的 Durability 0 Remote 直写同样经这里拒绝
// （durableCommit 的 memory 分支，RR-20260930-12）。
//
// 契约（REMAINING §3 N20，维护者 2026-09-30 接受）：这是交给 committer 之前的一次性检查，不是临界区。别的 goroutine 在检查之后、
// committer 接受之前 fence 引擎的窗口不在这里拦：那种 fence 来自另一笔事务的结果未知，本笔记录进入 WAL 后由 WAL terminal
// （引擎 fence 后 WAL 拒绝 / 停止推进）兜底；嵌套场景（同一 goroutine 先 fence 再提交外层）时序确定，本检查必然命中。
// 不把检查放进 committer 的临界区：那会让 WAL 准入依赖 Nest 的生命周期锁，且对已越过提交点的记录无法撤销。
func (tx *RollbackTx) refuseCommitAfterFence() error {
	if tx.dispatch == nil || tx.dispatch.engine == nil {
		return nil
	}
	fenced := tx.dispatch.engine.FenceError()
	if fenced == nil {
		return nil
	}
	return fmt.Errorf("%w: transaction of %q was not handed to the committer (fence cause: %v)", ErrNestFenced, tx.handler, fenced)
}

// restoresWriteOf 报告本（外层）事务回滚时是否会恢复 participants 写到的状态：participant 是本事务 MarkPersist 过的 DAO
// （已登记 tracker 快照），或属于本事务快照过的实体。按 DAO 实例判断，不按 ID：生成 DAO 的 Id() 是 StorageID，不一定等于实体 ID。
// 返回命中的 ID（用于错误信息）。
func (tx *RollbackTx) restoresWriteOf(participants []MutationParticipant) (int64, bool) {
	for _, participant := range participants {
		if _, ok := tx.participantChanges[participant]; ok {
			return persistParticipantID(participant), true
		}
		for _, e := range tx.snapshotted {
			if entityOwnsParticipant(e, participant) {
				return e.GUId(), true
			}
		}
	}
	return 0, false
}

// entityOwnsParticipant 按实例判断 participant 是否是 e 的某个 DAO。比较经 any 进行：动态类型不同直接不等；
// 相同时 participant 的类型已由 persistChange 保证可比较。
func entityOwnsParticipant(e entity.IThreadSafeEntity, participant MutationParticipant) bool {
	guardable, ok := e.(entity.Guardable)
	if !ok {
		return false
	}
	owned := false
	guardable.RangeDao(func(dao entity.DaoInterface) {
		if !owned && dao != nil && any(dao) == any(participant) {
			owned = true
		}
	})
	return owned
}

func persistParticipantID(participant MutationParticipant) int64 {
	if dao, ok := participant.(interface{ Id() int64 }); ok {
		return dao.Id()
	}
	return 0
}

// pipelinedEnqueue performs the in-lock half of a pipelined commit. It is the
// only rejection point: prepare and Enqueue run synchronously while the
// caller still holds entity locks and can roll back. A nil ticket with nil
// error means the record was empty and nothing needs to become durable.
func (tx *RollbackTx) pipelinedEnqueue(ctx context.Context, committer PipelinedTransactionCommitter) (CommitTicket, error) {
	record, err := tx.prepareCommitRecord()
	if err != nil {
		return nil, err
	}
	if record.Empty() {
		return nil, nil
	}
	if err := tx.refuseCommitAfterFence(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	enqueueStart := startNestStage(tx.stageMetrics)
	ticket, err := committer.Enqueue(ctx, record)
	observeNestStage(tx.handler, "enqueue", enqueueStart)
	if err != nil {
		return nil, errors.Join(ErrCommitRejected, err)
	}
	if ticket == nil {
		return nil, errors.Join(ErrCommitRejected, errors.New("nest: pipelined committer returned nil ticket"))
	}
	if err := tx.acceptPersistence(); err != nil {
		return ticket, err
	}
	return ticket, nil
}

type rollbackContextKey struct{}

func CurrentRollbackTx() *RollbackTx {
	c := fctx.CurrentContext()
	if c == nil {
		return nil
	}
	v, ok := c.Get(rollbackContextKey{})
	if !ok {
		return nil
	}
	tx, _ := v.(*RollbackTx)
	return tx
}

func AfterCommit(fn func()) bool {
	tx := CurrentRollbackTx()
	if tx == nil {
		return false
	}
	tx.AfterCommit(fn)
	return true
}

func withRollbackTx(tx *RollbackTx, fn func() (any, error)) (any, error) {
	c := fctx.CurrentContext()
	if c == nil || tx == nil {
		return fn()
	}
	old, hadOld := c.Get(rollbackContextKey{})
	c.Set(rollbackContextKey{}, tx)
	defer func() {
		if hadOld {
			c.Set(rollbackContextKey{}, old)
		} else {
			c.Set(rollbackContextKey{}, nil)
		}
	}()
	return fn()
}

// CaptureCreatedEntity 实现 entity.CreatedEntityCapturer：handler 内 CreateInScope 新建的实体与动态
// Cast 一样由本事务捕获回滚与持久化参与者（RR-20260926-35）。revoke 先于捕获登记，回滚时最后执行
// （回滚函数逆序），所以即使后续捕获部分失败，回滚 / 拒绝也会撤销这次发布。
func (tx *RollbackTx) CaptureCreatedEntity(created entity.IThreadSafeEntity, revoke func()) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if created == nil {
		return nil
	}
	tx.DeferRollback(func() error {
		revoke()
		return nil
	})
	if err := tx.CaptureEntities([]entity.IThreadSafeEntity{created}); err != nil {
		tx.noteCaptureFailed(err)
		return err
	}
	tx.created = append(tx.created, created)
	return nil
}

// noteCaptureFailed 记下 handler 内动态纳入实体时第一次捕获失败的错误，handler 结束时由 invokeWithTransaction 并入
// 结果、整条回滚（RR-20260927-11 / 31）。
func (tx *RollbackTx) noteCaptureFailed(err error) {
	if tx.captureFailed == nil {
		tx.captureFailed = err
	}
}

// CreatedEntityLockBusy 实现 entity.CreatedEntityCapturer：新实体的锁按锁序不能等待且已被占用（RR-20260926-48）。
// 记录第一次冲突并返回交给业务的错误。本事务可回滚、且所属消息自己的事务也可回滚时是可重试的锁超时：
// invokeWithTransaction 在 handler 结束时据此整条回滚，dispatchNest 重新准入。否则（本事务或外层消息的事务是
// RollbackNone）冲突前的内存修改不会被撤销，给不带 ErrLockTimeout 的冲突错误，消息不重排（RR-20260926-64）。
func (tx *RollbackTx) CreatedEntityLockBusy(id int64) error {
	rollbackable := tx != nil && tx.policy != RollbackNone
	if msg := currentNestDispatchMsg(); msg != nil && msg.txNoRollback {
		// 嵌套的 RunIsolatedTransaction 自己可回滚，但外层消息不能：重排外层会重复外层已做的修改。
		rollbackable = false
	}
	err := createdEntityLockBusy(id, rollbackable)
	if tx != nil && tx.createLockBusy == nil {
		tx.createLockBusy = err
	}
	return err
}

var _ entity.CreatedEntityCapturer = (*RollbackTx)(nil)

// createdEntityLockBusy 是 handler 内新建实体锁冲突交给业务的错误，总满足 errors.Is(ErrCreatedEntityLockConflict)。
// rollbackable 时同时满足 errors.Is(ErrLockTimeout)：事务回滚后由 requeueTransientDispatch 重新准入（与动态 Cast、
// 嵌套派发的锁冲突同一类，RR-20260926-48）。不能回滚时不带 ErrLockTimeout，并在当前消息上记下冲突，
// dispatchNest 据此不再重排这条消息（RR-20260926-64）。
func createdEntityLockBusy(id int64, rollbackable bool) error {
	if rollbackable {
		return fmt.Errorf("%w: %w: entity %d cannot be waited for in lock order", ErrLockTimeout, ErrCreatedEntityLockConflict, id)
	}
	currentNestDispatchMsg().markCreateLockConflictWithoutRollback()
	return fmt.Errorf("%w: entity %d cannot be waited for in lock order; the handler cannot roll back, so the message is not requeued", ErrCreatedEntityLockConflict, id)
}

// memoryHandlerCreates 在 memory 快路径（RollbackNone + DurabilityMemory、无 Remote 批次）的 handler 期间
// 绑定到 Guard（RR-20260926-48）：handler 内新建的实体沿用本 Guard 持锁到 handler 结束，并进入本次
// SyncMutation。memory 快路径没有回滚，所以不捕获、不撤销；锁冲突给不能重排的冲突错误（RR-20260926-64）。
type memoryHandlerCreates struct{}

func (memoryHandlerCreates) CaptureCreatedEntity(entity.IThreadSafeEntity, func()) error { return nil }
func (memoryHandlerCreates) CreatedEntityLockBusy(id int64) error {
	return createdEntityLockBusy(id, false)
}

func (tx *RollbackTx) CaptureEntities(es []entity.IThreadSafeEntity) error {
	if tx == nil {
		return nil
	}
	defer observeNestStage(tx.handler, "capture", startNestStage(tx.stageMetrics))
	seen := make(map[int64]struct{}, len(es))
	for _, e := range es {
		if e == nil {
			continue
		}
		if _, ok := seen[e.GUId()]; ok {
			continue
		}
		seen[e.GUId()] = struct{}{}
		remoteManaged := entity.IsEntityKindRemoteManaged(e.GetEntityKind())
		if remoteManaged {
			tx.remoteWrite = true
			msg := currentNestDispatchMsg()
			if tx.durability != DurabilityMemory && (msg == nil || msg.RemoteWriteBatch == nil) {
				return fmt.Errorf("%w: entity=%d kind=%d", ErrDurableRemoteWriteUnsupported, e.ID(), e.GetEntityKind())
			}
		}
		if participant, ok := e.(CommitParticipant); ok && !remoteManaged {
			if err := tx.RegisterCommitParticipant(participant); err != nil {
				return err
			}
		}
		if tx.policy == RollbackNone {
			continue
		}
		if tx.snapshotted == nil {
			// 一次分配覆盖声明目标加少量 Cast / 新建实体。
			tx.snapshotted = make([]entity.IThreadSafeEntity, 0, len(es)+2)
		}
		tx.snapshotted = append(tx.snapshotted, e)
		if tx.policy == RollbackState {
			if custom, ok := e.(RollbackParticipant); ok {
				if err := custom.CaptureRollback(tx); err != nil {
					return fmt.Errorf("nest rollback capture entity %d: %w", e.ID(), err)
				}
			}
		}
		guardable, ok := e.(entity.Guardable)
		if !ok {
			continue
		}
		var captureErr error
		guardable.RangeDao(func(dao entity.DaoInterface) {
			if captureErr != nil || dao == nil {
				return
			}
			if participant, ok := dao.(CommitParticipant); ok && !remoteManaged {
				if err := tx.RegisterCommitParticipant(participant); err != nil {
					captureErr = err
					return
				}
			}
			captureErr = tx.captureDao(dao)
		})
		if captureErr != nil {
			return captureErr
		}
	}
	return nil
}

type dataEngineTrackerDao interface {
	DirtyTracker() *dataengine.Tracker
}

// RollbackSnapshotter captures all rollback-relevant state without consuming
// persistence patch metadata.
type RollbackSnapshotter interface {
	CaptureRollbackState() ([]byte, error)
	RestoreRollbackState([]byte) error
}

func (tx *RollbackTx) captureDao(dao entity.DaoInterface) error {
	var engineTracker *dataengine.Tracker
	var engineSnapshot dataengine.TrackerSnapshot
	if d, ok := dao.(dataEngineTrackerDao); ok {
		engineTracker = d.DirtyTracker()
		engineSnapshot = engineTracker.Snapshot()
	}
	if tx.policy == RollbackUndo {
		if engineTracker != nil {
			tx.DeferRollback(func() error {
				engineTracker.Restore(engineSnapshot)
				return nil
			})
		}
		return nil
	}
	if custom, ok := dao.(RollbackParticipant); ok {
		return custom.CaptureRollback(tx)
	}
	if snapshotter, ok := dao.(RollbackSnapshotter); ok {
		raw, err := snapshotter.CaptureRollbackState()
		if err != nil {
			return err
		}
		raw = append([]byte(nil), raw...)
		tx.DeferRollback(func() error {
			if err := snapshotter.RestoreRollbackState(raw); err != nil {
				return err
			}
			if engineTracker != nil {
				engineTracker.Restore(engineSnapshot)
			}
			return nil
		})
		return nil
	}
	return fmt.Errorf("%w: dao %s/%d requires RollbackSnapshotter or RollbackParticipant", ErrRollbackUnsupported, dao.CollName(), dao.Id())
}
