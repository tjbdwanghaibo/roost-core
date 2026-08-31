package nest

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/cube-core/dataengine"
)

var ErrReceiptConflict = errors.New("nest: transaction receipt conflict")

// PersistChange is the transaction-local accumulation of persistence changes
// for one DAO. It is discarded on rollback; no persistence dirty mask is
// written back to the DAO.
type PersistChange struct {
	Mask       uint64
	Set        map[string]any
	Unset      []string
	FullFields map[string]struct{}
}

func (change PersistChange) Clone() PersistChange {
	cloned := PersistChange{
		Mask:  change.Mask,
		Unset: slices.Clone(change.Unset),
	}
	if change.Set != nil {
		cloned.Set = make(map[string]any, len(change.Set))
		for path, value := range change.Set {
			cloned.Set[path] = value
		}
	}
	if change.FullFields != nil {
		cloned.FullFields = make(map[string]struct{}, len(change.FullFields))
		for field := range change.FullFields {
			cloned.FullFields[field] = struct{}{}
		}
	}
	return cloned
}

// MutationParticipant freezes a transaction-local change into one mutation
// and advances its accepted in-memory persistence version after admission.
type MutationParticipant interface {
	PrepareMutation(PersistChange) (dataengine.Mutation, error)
	AcceptMutation(dataengine.Mutation) error
}

func (tx *RollbackTx) persistChange(participant MutationParticipant, mask uint64) (*PersistChange, error) {
	if tx == nil || tx.state != rollbackTxOpen || tx.persistencePrepared {
		return nil, ErrTransactionClosed
	}
	if isNilMutationParticipant(participant) {
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
		change = &PersistChange{}
		tx.participantChanges[participant] = change
		tx.participantOrder = append(tx.participantOrder, participant)
	}
	change.Mask |= mask
	return change, nil
}

func isNilMutationParticipant(participant MutationParticipant) bool {
	if participant == nil {
		return true
	}
	v := reflect.ValueOf(participant)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func (tx *RollbackTx) MarkPersist(participant MutationParticipant, mask uint64) error {
	_, err := tx.persistChange(participant, mask)
	return err
}

func MarkPersist(participant MutationParticipant, mask uint64) error {
	return CurrentRollbackTx().MarkPersist(participant, mask)
}

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

func MarkPersistSet(participant MutationParticipant, mask uint64, path string, value any) error {
	return CurrentRollbackTx().MarkPersistSet(participant, mask, path, value)
}

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
	for _, existing := range change.Unset {
		if existing == path {
			return nil
		}
	}
	change.Unset = append(change.Unset, path)
	return nil
}

func MarkPersistUnset(participant MutationParticipant, mask uint64, path string) error {
	return CurrentRollbackTx().MarkPersistUnset(participant, mask, path)
}

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

func MarkPersistFull(participant MutationParticipant, mask uint64, field string) error {
	return CurrentRollbackTx().MarkPersistFull(participant, mask, field)
}

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

func AddReceipt(receipt dataengine.Receipt) error {
	return CurrentRollbackTx().AddReceipt(receipt)
}

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
		mutation, err := participant.PrepareMutation(tx.participantChanges[participant].Clone())
		if err != nil {
			return err
		}
		canonical, err := dataengine.CanonicalizeMutation(mutation)
		if err != nil {
			return err
		}
		if err := tx.AddMutation(canonical); err != nil {
			return err
		}
		tx.preparedMutations[participant] = canonical
	}
	return nil
}

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

func coveredByFullField(fields map[string]struct{}, path string) bool {
	for field := range fields {
		if pathCovers(field, path) {
			return true
		}
	}
	return false
}

func pathCovers(parent, path string) bool {
	return path == parent || strings.HasPrefix(path, parent+".")
}

func removePath(paths []string, target string) []string {
	for i, path := range paths {
		if path == target {
			return append(paths[:i], paths[i+1:]...)
		}
	}
	return paths
}
