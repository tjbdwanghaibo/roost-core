package nest

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// ErrReceiptConflict 表示同一事务内以相同 namespace/id 绑定了摘要不同的回执。
var ErrReceiptConflict = errors.New("nest: transaction receipt conflict")

// PersistChange 是一个 DAO 在本事务内累积的持久化变化：字段位掩码、删除标记、按路径的 $set / $unset，以及整字段
// 重写。回滚时丢弃，不把持久化脏掩码写回 DAO。
type PersistChange struct {
	Mask       uint64
	Delete     bool
	Set        map[string]any
	Unset      []string
	FullFields map[string]struct{}
}

// Clone 返回独立的副本（Set 的值浅拷贝）。
func (change PersistChange) Clone() PersistChange {
	return PersistChange{
		Mask:       change.Mask,
		Delete:     change.Delete,
		Set:        maps.Clone(change.Set),
		Unset:      slices.Clone(change.Unset),
		FullFields: maps.Clone(change.FullFields),
	}
}

// MutationParticipant 把事务内的变化冻结成一条 mutation，并在准入后推进它已接受的内存持久化版本。
type MutationParticipant interface {
	PrepareMutation(PersistChange) (dataengine.Mutation, error)
	AcceptMutation(dataengine.Mutation) error
}

// persistChange 返回参与方在本事务中的变化记录（首次登记时创建，并为它的 tracker 登记回滚恢复），并合入 mask。
// 持久化准备开始后不再接受登记。
func (tx *RollbackTx) persistChange(participant MutationParticipant, mask uint64) (*PersistChange, error) {
	if tx == nil || tx.state != rollbackTxOpen || tx.persistencePrepared {
		return nil, ErrTransactionClosed
	}
	if isNilParticipant(participant) {
		return nil, errors.New("nest: nil mutation participant")
	}
	if !reflect.TypeOf(participant).Comparable() {
		return nil, errors.New("nest: mutation participant is not comparable")
	}
	if tx.participantChanges == nil {
		tx.participantChanges = make(map[MutationParticipant]*PersistChange, 4)
	}
	change := tx.participantChanges[participant]
	if change == nil {
		if tracked, ok := participant.(dataEngineTrackerDao); ok && tracked.DirtyTracker() != nil {
			tracker := tracked.DirtyTracker()
			snapshot := tracker.Snapshot()
			tx.DeferRollback(func() error {
				tracker.Restore(snapshot)
				return nil
			})
		}
		change = &PersistChange{}
		tx.participantChanges[participant] = change
		tx.participantOrder = append(tx.participantOrder, participant)
	}
	change.Mask |= mask
	return change, nil
}

// MarkPersist 登记参与方按 mask 指示的字段有持久化变化（整字段覆盖由 DAO 在准备时决定）。
func (tx *RollbackTx) MarkPersist(participant MutationParticipant, mask uint64) error {
	_, err := tx.persistChange(participant, mask)
	return err
}

// MarkPersist 在当前事务上登记持久化变化；不在事务内时返回 ErrTransactionClosed。
func MarkPersist(participant MutationParticipant, mask uint64) error {
	return CurrentRollbackTx().MarkPersist(participant, mask)
}

// MarkPersistSet 登记对 path 的 $set（已被整字段重写覆盖时忽略），并撤销同路径的 $unset。
func (tx *RollbackTx) MarkPersistSet(participant MutationParticipant, mask uint64, path string, value any) error {
	if !validPersistPath(path) {
		return fmt.Errorf("nest: invalid persistence path %q", path)
	}
	change, err := tx.persistChange(participant, mask)
	if err != nil {
		return err
	}
	if coveredByFullField(change.FullFields, path) {
		return nil
	}
	if change.Set == nil {
		change.Set = make(map[string]any, 4)
	}
	change.Set[path] = value
	change.Unset = removePath(change.Unset, path)
	return nil
}

// MarkPersistSet 在当前事务上登记 $set。
func MarkPersistSet(participant MutationParticipant, mask uint64, path string, value any) error {
	return CurrentRollbackTx().MarkPersistSet(participant, mask, path, value)
}

// MarkPersistUnset 登记对 path 的 $unset（已被整字段重写覆盖时忽略），并撤销同路径的 $set。
func (tx *RollbackTx) MarkPersistUnset(participant MutationParticipant, mask uint64, path string) error {
	if !validPersistPath(path) {
		return fmt.Errorf("nest: invalid persistence path %q", path)
	}
	change, err := tx.persistChange(participant, mask)
	if err != nil {
		return err
	}
	if coveredByFullField(change.FullFields, path) {
		return nil
	}
	delete(change.Set, path)
	if slices.Contains(change.Unset, path) {
		return nil
	}
	change.Unset = append(change.Unset, path)
	return nil
}

// MarkPersistUnset 在当前事务上登记 $unset。
func MarkPersistUnset(participant MutationParticipant, mask uint64, path string) error {
	return CurrentRollbackTx().MarkPersistUnset(participant, mask, path)
}

// MarkPersistFull 登记整字段重写 field：它覆盖并清除 field 之下已登记的 $set / $unset 与更细的整字段重写。
func (tx *RollbackTx) MarkPersistFull(participant MutationParticipant, mask uint64, field string) error {
	if !validPersistPath(field) {
		return fmt.Errorf("nest: invalid persistence field %q", field)
	}
	change, err := tx.persistChange(participant, mask)
	if err != nil {
		return err
	}
	if coveredByFullField(change.FullFields, field) {
		return nil
	}
	if change.FullFields == nil {
		change.FullFields = make(map[string]struct{}, 2)
	}
	for existing := range change.FullFields {
		if pathCovers(field, existing) {
			delete(change.FullFields, existing)
		}
	}
	change.FullFields[field] = struct{}{}
	for path := range change.Set {
		if pathCovers(field, path) {
			delete(change.Set, path)
		}
	}
	kept := change.Unset[:0]
	for _, path := range change.Unset {
		if !pathCovers(field, path) {
			kept = append(kept, path)
		}
	}
	change.Unset = kept
	return nil
}

// MarkPersistFull 在当前事务上登记整字段重写。
func MarkPersistFull(participant MutationParticipant, mask uint64, field string) error {
	return CurrentRollbackTx().MarkPersistFull(participant, mask, field)
}

// MarkPersistDelete 登记删除参与方的文档，清除其他已登记的变化。
func (tx *RollbackTx) MarkPersistDelete(participant MutationParticipant) error {
	change, err := tx.persistChange(participant, dataengine.AllFields)
	if err != nil {
		return err
	}
	change.Delete = true
	change.Set = nil
	change.Unset = nil
	change.FullFields = nil
	return nil
}

// MarkPersistDelete 在当前事务上登记删除。
func MarkPersistDelete(participant MutationParticipant) error {
	return CurrentRollbackTx().MarkPersistDelete(participant)
}

// RemotePersistChangeFor 把一个 DAO 在本事务内的变化认领给 Remote 聚合提交。被认领的参与方不再进入普通 mutation
// 列表，避免同一变化走两条持久化路径。
func (tx *RollbackTx) RemotePersistChangeFor(value any) (entity.RemotePersistChange, bool) {
	if tx == nil || tx.state != rollbackTxOpen || tx.persistencePrepared {
		return entity.RemotePersistChange{}, false
	}
	participant, ok := value.(MutationParticipant)
	if !ok || isNilParticipant(participant) {
		return entity.RemotePersistChange{}, false
	}
	change := tx.participantChanges[participant]
	if change == nil {
		return entity.RemotePersistChange{}, false
	}
	if tx.remoteParticipants == nil {
		tx.remoteParticipants = make(map[MutationParticipant]struct{}, 2)
	}
	tx.remoteParticipants[participant] = struct{}{}
	return entity.RemotePersistChange{Mask: change.Mask, Delete: change.Delete}, true
}

// AddReceipt 绑定一条随事务持久的幂等回执；同一 namespace/id 摘要相同时幂等，不同时返回 ErrReceiptConflict。
func (tx *RollbackTx) AddReceipt(receipt dataengine.Receipt) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	if receipt.Namespace == "" || receipt.ID == "" {
		return errors.New("nest: invalid transaction receipt")
	}
	if tx.receiptDigests == nil {
		tx.receiptDigests = make(map[receiptKey][]byte, 2)
	}
	key := receiptKey{namespace: receipt.Namespace, id: receipt.ID}
	if digest, exists := tx.receiptDigests[key]; exists {
		if !bytes.Equal(digest, receipt.Digest) {
			return fmt.Errorf("%w: %s/%s", ErrReceiptConflict, receipt.Namespace, receipt.ID)
		}
		return nil
	}
	cloned := dataengine.CloneReceipt(receipt)
	tx.receiptDigests[key] = slices.Clone(cloned.Digest)
	tx.receipts = append(tx.receipts, cloned)
	return nil
}

// AddReceipt 在当前事务上绑定回执。
func AddReceipt(receipt dataengine.Receipt) error {
	return CurrentRollbackTx().AddReceipt(receipt)
}

// SetReceiptPayload 为本事务已绑定的回执附上操作的持久结果。身份与摘要不能修改。
func (tx *RollbackTx) SetReceiptPayload(namespace, id string, payload []byte) error {
	if tx == nil || tx.state != rollbackTxOpen {
		return ErrTransactionClosed
	}
	key := receiptKey{namespace: namespace, id: id}
	if _, exists := tx.receiptDigests[key]; !exists {
		return fmt.Errorf("nest: transaction receipt %s/%s is not bound", namespace, id)
	}
	for index := range tx.receipts {
		if tx.receipts[index].Namespace == namespace && tx.receipts[index].ID == id {
			tx.receipts[index].Payload = slices.Clone(payload)
			return nil
		}
	}
	return fmt.Errorf("nest: transaction receipt %s/%s is not bound", namespace, id)
}

// SetReceiptPayload 在当前事务上为回执附上结果。
func SetReceiptPayload(namespace, id string, payload []byte) error {
	tx := CurrentRollbackTx()
	if tx == nil {
		return ErrTransactionClosed
	}
	return tx.SetReceiptPayload(namespace, id, payload)
}

// preparePersistence 按登记顺序让每个未被 Remote 认领的参与方把变化冻结成 mutation，校验后加入事务。只执行一次；
// 之后不再接受新的持久化登记。
func (tx *RollbackTx) preparePersistence() error {
	if tx.persistencePrepared {
		return nil
	}
	tx.persistencePrepared = true
	if len(tx.participantOrder) == 0 {
		return nil
	}
	tx.preparedMutations = make(map[MutationParticipant]dataengine.Mutation, len(tx.participantOrder))
	for _, participant := range tx.participantOrder {
		if _, remote := tx.remoteParticipants[participant]; remote {
			continue
		}
		mutation, err := participant.PrepareMutation(tx.participantChanges[participant].Clone())
		if err != nil {
			return err
		}
		canonical := dataengine.CloneMutation(mutation)
		if err := dataengine.ValidateMutation(canonical); err != nil {
			return err
		}
		if err := tx.AddMutation(canonical); err != nil {
			return err
		}
		tx.preparedMutations[participant] = canonical
	}
	return nil
}

// acceptPersistence 在 committer 接受记录后通知参与方推进内存持久化版本。失败时记录已持久而内存未推进，
// 只能按结果不确定处理（ErrCommitIndeterminate）。
func (tx *RollbackTx) acceptPersistence() error {
	if tx == nil || tx.accepted {
		return nil
	}
	for _, participant := range tx.participantOrder {
		mutation, ok := tx.preparedMutations[participant]
		if !ok {
			continue
		}
		if err := participant.AcceptMutation(dataengine.CloneMutation(mutation)); err != nil {
			return errors.Join(ErrCommitIndeterminate, err)
		}
	}
	tx.accepted = true
	return nil
}

// validPersistPath 拒绝空路径、含 NUL、空段或以 $ 开头的段。
func validPersistPath(path string) bool {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	for _, segment := range strings.Split(path, ".") {
		if segment == "" || strings.HasPrefix(segment, "$") {
			return false
		}
	}
	return true
}

// coveredByFullField 报告 path 是否落在某个整字段重写之下。
func coveredByFullField(fields map[string]struct{}, path string) bool {
	for field := range fields {
		if pathCovers(field, path) {
			return true
		}
	}
	return false
}

// pathCovers 报告 path 等于 parent 或位于 parent 之下。
func pathCovers(parent, path string) bool {
	return path == parent || strings.HasPrefix(path, parent+".")
}

// removePath 删除 paths 中第一个等于 target 的元素（原地）。
func removePath(paths []string, target string) []string {
	for i, path := range paths {
		if path == target {
			return append(paths[:i], paths[i+1:]...)
		}
	}
	return paths
}
