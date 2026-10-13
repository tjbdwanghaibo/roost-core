package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// RollbackPolicy 是 handler 失败时撤销内存修改的方式。
type RollbackPolicy uint8

const (
	// RollbackNone 不撤销内存修改；handler 开始执行后失败不再重排。
	RollbackNone RollbackPolicy = iota
	// RollbackState 在事务开始时为实体与 DAO 取状态快照，失败时恢复。
	RollbackState
	// RollbackUndo 由 setter 按字段登记首次逆操作，失败时逆序执行。
	RollbackUndo
)

// ParseRollbackPolicy 解析 //roost:nest 与配置中的回滚策略名，空串视为 none。
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

// HandlerMeta 是 handler 的事务声明：回滚策略与持久级别。
type HandlerMeta struct {
	Rollback   RollbackPolicy
	Durability DurabilityPolicy
}

// RollbackParticipant 由需要自定义状态回滚（超出生成 DAO 快照兜底）的实体或 DAO 实现。
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
	// created 是 handler 内 CreateInScope 新建的实体，准入前再次加入 SyncMutation。
	created []entity.IThreadSafeEntity
	// createLockBusy 是 handler 内新建实体时第一次锁冲突的错误。可回滚的事务据此在
	// handler 结束时整条回滚并以锁超时重新准入，即使业务吞掉了 Create 返回的错误。
	createLockBusy error
	// captureFailed 是 handler 内动态纳入事务的实体（CreateInScope 新建或 Cast 取得）第一次事务捕获失败的错误。
	// 与 createLockBusy 同一种处理：handler 结束时即使业务吞掉了返回的错误也整条回滚——捕获失败前可能已登记了
	// 提交参与者，照常提交会让它们准备记录，本事务的其他修改带着一个未进入回滚 / 持久化参与的实体持久化。
	captureFailed error
	// dispatch 是以本事务为自身事务的派发消息（嵌套的 RunIsolatedTransaction / 无派发调用为 nil）；
	// 越过提交点时在它上面记录，dispatchNest 据此不再重新准入。
	dispatch *Msg
	// enclosing 是本事务开始时已在执行的外层事务（handler 内嵌套的 RunIsolatedTransaction 才有；消息自己的事务为 nil）。
	// snapshotted 是本事务以可回滚策略捕获（登记了快照 / tracker 恢复）的实体。嵌套事务提交前据此拒绝写外层会回滚的实体。
	enclosing   *RollbackTx
	snapshotted []entity.IThreadSafeEntity
}

// rollbackTxState 是事务的终态机：open → committed（含 abandon）或 rolledBack，只前进一次。
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

// NewRollbackTx 新建一个 open 状态、durability 为 memory 的事务；Emit / RequestEntityDelete 会把它升到 strict。
func NewRollbackTx(policy RollbackPolicy) *RollbackTx {
	return &RollbackTx{id: newTransactionID(), policy: policy, durability: DurabilityMemory}
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

// DeferRollback 登记一个回滚函数；回滚时按登记的逆序执行。事务已结束时忽略。
func (tx *RollbackTx) DeferRollback(fn func() error) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.rollbacks = append(tx.rollbacks, fn)
	}
}

// RecordUndo 为本事务内的 owner/field 至多登记一次逆操作。owner 必须可比较，生成代码传 DAO 指针。
func (tx *RollbackTx) RecordUndo(owner any, field uint64, fn func() error) error {
	return tx.RecordUndoToken(owner, field, nil, fn)
}

// HasUndo 查询此 DAO 字段是否已登记首次逆操作，供复制成本较高的手写 DAO 延迟取快照。
// 与 RecordUndo 一样，owner 必须可比较，调用方必须在同一串行事务内查询后登记。
func (tx *RollbackTx) HasUndo(owner any, field uint64) bool {
	if tx == nil {
		return false
	}
	_, found := tx.undoKeys[undoKey{owner: owner, field: field}]
	return found
}

// RecordUndoToken 为字段的子资源登记逆操作。token 让生成的集合 setter 能分别捕获多个 map 键，
// 同时对同一键的重复写入只登记一次。
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

// RecordUndo 向当前 Nest 事务登记逆操作；不在 rollback=undo 的 handler 内时返回 false。
func RecordUndo(owner any, field uint64, fn func() error) bool {
	tx := CurrentRollbackTx()
	if tx == nil || tx.policy != RollbackUndo {
		return false
	}
	return tx.RecordUndo(owner, field, fn) == nil
}

// RecordUndoToken 是 (*RollbackTx).RecordUndoToken 的当前事务版本；不在 rollback=undo 的 handler 内时返回 false。
func RecordUndoToken(owner any, field uint64, token any, fn func() error) bool {
	tx := CurrentRollbackTx()
	if tx == nil || tx.policy != RollbackUndo {
		return false
	}
	return tx.RecordUndoToken(owner, field, token, fn) == nil
}

// AfterCommit 登记提交后回调：事务持久且实体锁释放后执行，每个回调独立恢复 panic。
func (tx *RollbackTx) AfterCommit(fn func()) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.commits = append(tx.commits, fn)
	}
}

// AfterAdmission 登记准入后回调：事务越过持久准入点之后、Nest 释放实体锁之前执行。它只用于生命周期变更
// （例如删除 tombstone 已在 WAL 中的实体）；外部副作用应放在 AfterCommit。
// pipelined 的准入早于 ticket 持久化确认；这里可冻结同步内容，但不能提前外发。
func (tx *RollbackTx) AfterAdmission(fn func()) {
	if tx != nil && tx.state == rollbackTxOpen && fn != nil {
		tx.afterAdmission = append(tx.afterAdmission, fn)
	}
}

// RequestEntityDelete 记录一个事务内的聚合删除意图，并把 durability 升到 strict。同一实体在本事务内
// 只有第一次返回 true，调用方据此只登记一次生命周期收尾。
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

// CancelEntityDelete 撤销一个无法准备的删除意图；只在持久化准备开始之前有效。
func (tx *RollbackTx) CancelEntityDelete(entityID int64) {
	if tx == nil || tx.state != rollbackTxOpen || tx.persistencePrepared {
		return
	}
	delete(tx.deleteIntents, entityID)
}

// RemoteDeleteRequested 实现 entity.RemoteDeleteIntentSource。
func (tx *RollbackTx) RemoteDeleteRequested(entityID int64) bool {
	if tx == nil {
		return false
	}
	_, requested := tx.deleteIntents[entityID]
	return requested
}

// RegisterCommitParticipant 登记提交参与者（按实例去重），准备提交记录时调用它的 PrepareCommit。
func (tx *RollbackTx) RegisterCommitParticipant(participant CommitParticipant) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if isNilParticipant(participant) {
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

// isNilParticipant 报告参与方是 nil 接口，或是装着 nil 指针的接口。
func isNilParticipant(participant any) bool {
	if participant == nil {
		return true
	}
	v := reflect.ValueOf(participant)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

// AddMutation 直接加入一条原始 mutation（不经 DAO），按文档身份去重。
func (tx *RollbackTx) AddMutation(mutation EntityMutation) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if err := dataengine.ValidateMutation(mutation); err != nil {
		return err
	}
	if tx.mutationKeys == nil {
		tx.mutationKeys = make(map[mutationKey]struct{}, 4)
	}
	key := keyOfMutation(mutation)
	if _, exists := tx.mutationKeys[key]; exists {
		return fmt.Errorf("nest: duplicate entity mutation %s/%s/%d", key.database, key.resource, key.entityID)
	}
	tx.mutationKeys[key] = struct{}{}
	tx.mutations = append(tx.mutations, dataengine.CloneMutation(mutation))
	return nil
}

// keyOfMutation 使用唯一文档身份对同一事务内的 mutation 去重。
func keyOfMutation(mutation EntityMutation) mutationKey {
	return mutationKey{database: mutation.Key.Database, resource: mutation.Key.Resource, entityID: mutation.Key.ID}
}

// Emit 登记一条随事务持久的外部效果（outbox）。未指定 ID 时按事务 ID 与序号生成。
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
	// outbox 只有在实体锁释放前持久准入才有意义：memory 事务升到 strict，不提供会丢失的“outbox”。
	if tx.durability == DurabilityMemory {
		tx.durability = DurabilityStrict
	}
	tx.effects = append(tx.effects, dataengine.CloneEffect(effect))
	return nil
}

// Emit 向当前 Nest 事务登记外部效果；不在事务内时返回 ErrTransactionClosed。
func Emit(effect Effect) error {
	tx := CurrentRollbackTx()
	if tx == nil {
		return ErrTransactionClosed
	}
	return tx.Emit(effect)
}

// Rollback 逆序执行全部回滚函数并丢弃提交内容。重复回滚返回 nil，已提交的事务返回 ErrTransactionClosed。
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

// Commit 结束事务：执行准入后回调，再执行或转交提交后回调。
func (tx *RollbackTx) Commit() error {
	return tx.commit(false)
}

// commit 结束事务并安排提交后回调：带 Remote 批次的消息交给批次收尾；在 Guard 内且实体未释放时排到
// Guard 释放之后；其余立即执行并返回回调错误。
//
// entitiesReleased 只由 pipelined 在统一释放完成后传 true。Guard scope 此时可能仍挂在 dispatch goroutine 上，
// 但已不持锁；不能再把 AfterCommit 排到 scope 的末尾，否则降级完成会提前回复并跳过回调错误报告。
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
	// 不论前一个回调结果如何，每个回调都执行：事务已标记提交、持久事实不会撤回，业务回调 panic 不能取消排在
	// 它后面的框架义务（TransactionReleased 通知就是其中之一，调用方的回复依赖本函数返回）。
	// 直接完成时把回调异常返回给调用者；Guard / Remote 延迟执行的路径由各自 runner 处理异常，
	// 不能从 Commit 返回 nil 推断延迟回调已经全部执行成功。
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
		// Guard 的 post-release runner 已逐个隔离回调异常。
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

// runCommitCallback 在独立的恢复边界内执行一个提交后回调，把 panic 转为 ErrAfterCommitFailed。
func runCommitCallback(fn func()) (err error) {
	if fn == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			// panic 值是 error 时以 %w 保留原因链，调用方仍可 errors.Is 原因。
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

// abandon 关闭结果不确定的事务，不执行回滚，也不执行准入后 / 提交后回调。宿主进程应停止接受写入，
// 并从 WAL 恢复权威结果。
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

// prepareCommitRecord 让提交参与者与 DAO 准备持久内容，生成提交记录（mutation、effect、receipt 都已复制）。
func (tx *RollbackTx) prepareCommitRecord() (CommitRecord, error) {
	if tx != nil {
		defer observeNestStage(tx.handler, "prepare", startNestStage(tx.stageMetrics))
	}
	if tx == nil || tx.state != rollbackTxOpen {
		return CommitRecord{}, ErrTransactionClosed
	}
	for _, participant := range tx.participants {
		if isNilParticipant(participant) {
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
		mutations[i] = dataengine.CloneMutation(tx.mutations[i])
	}
	record := CommitRecord{
		ID: tx.id, Handler: tx.handler, RequestID: requestID, CreatedAt: time.Now().UnixNano(), Durability: tx.durability.Record(),
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

// requestID 取当前上下文的 TraceID；没有时按 player/msg/seq 拼出，都没有时为空。
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

// durableCommit 是锁内持久提交：准备记录、做提交前的拒绝检查（嵌套写外层快照、引擎已 fence），交给 committer，
// 成功后让 DAO 接受持久结果。返回的错误或带 ErrCommitIndeterminate（结果未知），或是明确拒绝。
func (tx *RollbackTx) durableCommit(ctx context.Context, committer TransactionCommitter) error {
	// durability=memory 且没有 effect 的事务不写任何持久记录：不准备 after-image、不交给 committer（Emit / RequestEntityDelete
	// 会把 durability 升到 strict，走不到这里）。本地持久内容在这里无处可去，所以提交点拒绝（见 refuseMemoryPersistentWrite）。
	if tx.durability == DurabilityMemory && len(tx.effects) == 0 {
		if tx.dispatch != nil && tx.dispatch.RemoteWriteBatch != nil {
			// 带 Remote 批次、没有 effect 的 memory handler：没有记录交给 committer，但返回 nil 会让 commitDurable 置
			// remoteCommitted，finishRemoteWriteBatch 随即以 Durability 0 直写权威。引擎已 fence 时这条写同样要拒绝
			// （维护者收紧）：返回 ErrNestFenced 后 commitDurable 按明确拒绝 Abort 批次并加 ErrCommitRejected；
			// RollbackNone 不撤销内存修改。
			if err := tx.refuseCommitAfterFence(); err != nil {
				return err
			}
		}
		return tx.refuseMemoryPersistentWrite()
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

// PersistFieldNamer 由生成 DAO 实现：按 PersistChange 的位给出持久字段的存储名，用于错误里点名字段。
// 没实现的参与方按路径或位掩码描述。
type PersistFieldNamer interface {
	PersistFieldNames(mask uint64) []string
}

// refuseMemoryPersistentWrite 是 memory 事务（没有 effect）的提交点检查（维护者 2026-10-07 选 A）：handler 留下了
// 本该写进提交记录的本地持久内容——DAO 持久字段的改动（MarkPersist* 登记的参与方，Remote 批次认领的除外）、AddMutation 加的
// 本地 mutation（Remote 批次 finalize 加的 Remote mutation 除外，它们随批次提交）、AddReceipt 绑定的 receipt——就整笔拒绝，
// 错误点名实体与字段。调用方 commitDurable 按明确拒绝处理：加 ErrCommitRejected、按回滚策略撤销内存修改，带 Remote 批次的
// Abort 批次。若照常返回成功，内存是新值、库里是旧值，PersistChange 提交后即丢弃，实体重新加载后回到旧值。
//
// 只看事务本地的登记（participantOrder / mutations / receipts），代价是 O(被改实体)，不扫实体全量。只改非持久字段
// （nopersist，生成 setter 不调用 MarkPersist）的事务没有登记，照常成功。rollback=none 的 memory 快路径没有 RollbackTx，
// 持久 setter 在 MarkPersist 处以 ErrTransactionClosed panic，不经过这里，行为不变。
func (tx *RollbackTx) refuseMemoryPersistentWrite() error {
	var lost []string
	for _, participant := range tx.participantOrder {
		if _, remote := tx.remoteParticipants[participant]; remote {
			continue
		}
		lost = append(lost, describePersistChange(participant, tx.participantChanges[participant]))
	}
	for i := range tx.mutations {
		if tx.mutations[i].Remote != nil {
			continue
		}
		key := keyOfMutation(tx.mutations[i])
		lost = append(lost, fmt.Sprintf("%s/%s/%d (AddMutation)", key.database, key.resource, key.entityID))
	}
	for _, receipt := range tx.receipts {
		lost = append(lost, fmt.Sprintf("receipt %s/%s", receipt.Namespace, receipt.ID))
	}
	if len(lost) == 0 {
		return nil
	}
	return fmt.Errorf("%w: handler %q (rollback=%s, durability=memory) would lose %s; declare a durable policy (strict/async/pipelined) or make the fields nopersist",
		ErrMemoryTransactionPersistentWrite, tx.handler, tx.policy, strings.Join(lost, "; "))
}

// describePersistChange 给出 “collection/id fields [a b]” 形式的描述：实体取 DAO 的 CollName / Id，字段优先取
// PersistFieldNamer，其次取事务里登记的路径，都没有时给位掩码。
func describePersistChange(participant MutationParticipant, change *PersistChange) string {
	owner := fmt.Sprintf("%T", participant)
	if dao, ok := participant.(interface {
		CollName() string
		Id() int64
	}); ok {
		owner = fmt.Sprintf("%s/%d", dao.CollName(), dao.Id())
	}
	if change == nil {
		return owner
	}
	if change.Delete {
		return owner + " delete"
	}
	var fields []string
	if namer, ok := participant.(PersistFieldNamer); ok {
		fields = namer.PersistFieldNames(change.Mask)
	}
	if len(fields) == 0 {
		for path := range change.Set {
			fields = append(fields, path)
		}
		fields = append(fields, change.Unset...)
		for field := range change.FullFields {
			fields = append(fields, field)
		}
		slices.Sort(fields)
	}
	if len(fields) == 0 {
		return fmt.Sprintf("%s fields mask=%#x", owner, change.Mask)
	}
	return fmt.Sprintf("%s fields %v", owner, fields)
}

// refuseWriteUnderEnclosingRollback 在嵌套事务写任何持久记录之前检查：本事务要持久写的 DAO 若已被外层
// 可回滚事务登记回滚快照，外层随后失败回滚会把快照恢复到内存（RollbackState 值与 tracker 版本、RollbackUndo 的 tracker 版本），
// 覆盖本事务已持久的结果，内存与持久分叉、下一次写以旧 ExpectedVersion 撞冲突。维护者决定拒绝而不是改外层
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
// 实体。restoresWriteOf 只看 MarkPersist 登记的 DAO 参与方，覆盖不到嵌套事务用导出的 AddMutation 直写外层已快照实体的情形
// （外层失败回滚后内存与持久分叉）。原始 mutation 没有 DAO 实例可比，只能按实体 ID
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

// refuseCommitAfterFence 在消息自己的事务交给 committer 之前检查所在引擎是否已 fence。handler 内嵌套独立
// 事务结果未知时已 fence 引擎，但外层 handler 会继续执行到结束；结果未知若来自 acceptPersistence（committer 已成功、
// DAO AcceptMutation 失败），真实 WAL 并未进入 terminal，外层自己的记录不能在引擎 fence 之后继续写入。这里返回
// ErrNestFenced：记录没有交给 committer，是明确拒绝，调用方（commitDurable / commitPipelined）按拒绝回滚（rejectCommit
// 另加 ErrCommitRejected；RollbackNone 的事务不撤销内存修改）。
// 错误里不带 fence 原因的哨兵（%v）：原因链上的 ErrCommitIndeterminate 会让调用方误走结果未知分支（abandon 而不回滚）。
// 嵌套独立事务（dispatch 为 nil）不在此列：本约束只针对外层自己的提交。只读一次 lifecycleMu 保护的字段，不等待、不做 I/O，
// 与派发入口 runNestLogic 在业务 worker 上读 FenceError 相同。
// 带 Remote 批次、没有 effect 的 memory handler 没有记录交给 committer，但它的 Durability 0 Remote 直写同样经这里拒绝
// （durableCommit 的 memory 分支）。
//
// 契约（维护者 2026-09-30 接受）：这是交给 committer 之前的一次性检查，不是临界区。别的 goroutine 在检查之后、
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

// persistParticipantID 取参与方 DAO 的 Id()（用于错误信息）；没有时为 0。
func persistParticipantID(participant MutationParticipant) int64 {
	if dao, ok := participant.(interface{ Id() int64 }); ok {
		return dao.Id()
	}
	return 0
}

// pipelinedEnqueue 是 pipelined 提交的锁内半程，也是唯一的拒绝点：准备与 Enqueue 在调用方仍持实体锁、仍能回滚时
// 同步执行。返回 nil ticket 且 nil 错误表示记录为空，没有内容需要持久。
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

// CurrentRollbackTx 返回当前上下文正在执行的事务；不在事务内时为 nil。
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

// AfterCommit 向当前事务登记提交后回调；不在事务内时返回 false。
func AfterCommit(fn func()) bool {
	tx := CurrentRollbackTx()
	if tx == nil {
		return false
	}
	tx.AfterCommit(fn)
	return true
}

// withRollbackTx 在执行 fn 期间把 tx 设为当前上下文的事务，结束后恢复原值。
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
// Cast 一样由本事务捕获回滚与持久化参与者。revoke 先于捕获登记，回滚时最后执行
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

// noteCaptureFailed 记下 handler 内动态纳入实体时第一次捕获失败的错误，handler 结束时由 runTransaction 并入
// 结果、整条回滚。
func (tx *RollbackTx) noteCaptureFailed(err error) {
	if tx.captureFailed == nil {
		tx.captureFailed = err
	}
}

// CreatedEntityLockBusy 实现 entity.CreatedEntityCapturer：新实体的锁按锁序不能等待且已被占用。
// 记录第一次冲突并返回交给业务的错误。本事务可回滚、且所属消息自己的事务也可回滚时是可重试的锁超时：
// runTransaction 在 handler 结束时据此整条回滚，dispatchNest 重新准入。否则（本事务或外层消息的事务是
// RollbackNone）冲突前的内存修改不会被撤销，给不带 ErrLockTimeout 的冲突错误，消息不重排。
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
// 嵌套派发的锁冲突同一类）。不能回滚时不带 ErrLockTimeout，并在当前消息上记下冲突，
// dispatchNest 据此不再重排这条消息。
func createdEntityLockBusy(id int64, rollbackable bool) error {
	if rollbackable {
		return fmt.Errorf("%w: %w: entity %d cannot be waited for in lock order", ErrLockTimeout, ErrCreatedEntityLockConflict, id)
	}
	currentNestDispatchMsg().markCreateLockConflictWithoutRollback()
	return fmt.Errorf("%w: entity %d cannot be waited for in lock order; the handler cannot roll back, so the message is not requeued", ErrCreatedEntityLockConflict, id)
}

// memoryHandlerCreates 在 memory 快路径（RollbackNone + DurabilityMemory、无 Remote 批次）的 handler 期间
// 绑定到 Guard：handler 内新建的实体沿用本 Guard 持锁到 handler 结束，并进入本次
// SyncMutation。memory 快路径没有回滚，所以不捕获、不撤销；锁冲突给不能重排的冲突错误。
type memoryHandlerCreates struct{}

func (memoryHandlerCreates) CaptureCreatedEntity(entity.IThreadSafeEntity, func()) error { return nil }
func (memoryHandlerCreates) CreatedEntityLockBusy(id int64) error {
	return createdEntityLockBusy(id, false)
}

// CaptureEntities 把实体纳入本事务（按 GUId 去重）：登记提交参与者，按回滚策略为实体与它的 DAO 登记快照或
// tracker 恢复。Remote 托管实体不登记本地提交参与者；它们只能在 memory 事务或带 Remote 批次的消息中持久写。
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

// RollbackSnapshotter 捕获全部与回滚相关的状态，且不消耗持久化 patch 元数据。
type RollbackSnapshotter interface {
	CaptureRollbackState() ([]byte, error)
	RestoreRollbackState([]byte) error
}

// captureDao 为一个 DAO 登记回滚：undo 策略只恢复 tracker（字段由 setter 登记逆操作）；state 策略优先用
// RollbackParticipant，其次 RollbackSnapshotter 快照，两者都没有时返回 ErrRollbackUnsupported。
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
