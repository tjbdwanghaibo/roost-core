package skillsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var (
	ErrObserverMismatch    = errors.New("skillsync: packet observer mismatch")
	ErrSchemaMismatch      = errors.New("skillsync: packet schema mismatch")
	ErrEpochMismatch       = errors.New("skillsync: packet epoch mismatch")
	ErrSequenceGap         = errors.New("skillsync: packet sequence gap")
	ErrPacketShape         = errors.New("skillsync: packet shape does not match record kind")
	ErrApplyInProgress     = errors.New("skillsync: another packet is being applied to the stream")
	ErrTransactionRequired = errors.New("skillsync: prepare returned a nil transaction")
)

// ApplyTransaction separates validation/preparation from the externally
// visible mutation. Commit must be atomic; Rollback must be idempotent.
type ApplyTransaction interface {
	Commit() error
	Rollback()
}

type ManifestConsumer interface {
	ApplyManifest(int64, skill.PresentationPlan) error
}

type TransactionalManifestConsumer interface {
	PrepareManifest(int64, skill.PresentationPlan) (ApplyTransaction, error)
}
type StateConsumer interface {
	ApplyStateSnapshot(int64, skill.RuntimeStateSnapshot) error
	ApplyStateDelta(int64, skill.StateMutation) error
}

type TransactionalStateConsumer interface {
	PrepareStateSnapshot(int64, skill.RuntimeStateSnapshot) (ApplyTransaction, error)
	PrepareStateDelta(int64, skill.StateMutation) (ApplyTransaction, error)
}
type PresentationConsumer interface {
	ResetPresentation(int64, PresentationReset) error
	ApplyPresentation(int64, skill.PresentationEvent) error
}

type TransactionalPresentationConsumer interface {
	PreparePresentationReset(int64, PresentationReset) (ApplyTransaction, error)
	PreparePresentation(int64, skill.PresentationEvent) (ApplyTransaction, error)
}

type ApplierOptions struct {
	Observer        syncstream.Observer
	SchemaVersion   uint32
	SupportedSchema SchemaRange
	Migrator        SchemaMigrator
	Manifest        ManifestConsumer
	State           StateConsumer
	Presentation    PresentationConsumer
}

type ApplyResult struct {
	Applied   bool
	Duplicate bool
	Sequence  uint64
	Epoch     uint64
}
type streamCursor struct {
	epoch    uint64
	sequence uint64
}

// Applier uses optimistic per-stream admission. No mutex is held while invoking
// consumer callbacks; reentrant/concurrent application returns
// ErrApplyInProgress and can be retried.
type Applier struct {
	mutex                sync.Mutex
	observer             syncstream.Observer
	schemaVersion        uint32
	supported            SchemaRange
	migrator             SchemaMigrator
	manifestConsumer     ManifestConsumer
	stateConsumer        StateConsumer
	presentationConsumer PresentationConsumer
	epoch                uint64
	pendingEpoch         uint64
	sequences            map[syncstream.Stream]streamCursor
	inflight             map[syncstream.Stream]struct{}
	stateMutations       map[int64]uint64
	presentationEvents   map[int64]uint64
	manifests            map[string]skill.PresentationPlan
}

func NewApplier(options ApplierOptions) (*Applier, error) {
	if options.SchemaVersion == 0 {
		return nil, ErrSchemaVersionRequired
	}
	if options.SupportedSchema.Min == 0 {
		options.SupportedSchema = SchemaRange{Min: options.SchemaVersion, Max: options.SchemaVersion}
	}
	if !options.SupportedSchema.Contains(options.SchemaVersion) {
		return nil, ErrSchemaMismatch
	}
	return &Applier{observer: options.Observer, schemaVersion: options.SchemaVersion, supported: options.SupportedSchema, migrator: options.Migrator, manifestConsumer: options.Manifest, stateConsumer: options.State, presentationConsumer: options.Presentation, sequences: make(map[syncstream.Stream]streamCursor), inflight: make(map[syncstream.Stream]struct{}), stateMutations: make(map[int64]uint64), presentationEvents: make(map[int64]uint64), manifests: make(map[string]skill.PresentationPlan)}, nil
}

func (applier *Applier) Apply(packet syncstream.Packet) (ApplyResult, error) {
	packet, result, err := applier.admit(packet)
	if err != nil || result.Duplicate {
		return result, err
	}
	prepared, err := applier.applyRecord(packet)
	if err == nil && prepared.transaction != nil {
		if commitErr := prepared.transaction.Commit(); commitErr != nil {
			prepared.transaction.Rollback()
			err = commitErr
		}
	}
	applier.mutex.Lock()
	delete(applier.inflight, packet.Stream)
	if err == nil {
		if applier.pendingEpoch == packet.Epoch {
			if applier.epoch != packet.Epoch {
				applier.sequences = make(map[syncstream.Stream]streamCursor)
				applier.stateMutations = make(map[int64]uint64)
				applier.presentationEvents = make(map[int64]uint64)
				applier.manifests = make(map[string]skill.PresentationPlan)
			}
			applier.epoch = packet.Epoch
		}
		if prepared.commitState != nil {
			prepared.commitState()
		}
		applier.sequences[packet.Stream] = streamCursor{epoch: packet.Epoch, sequence: packet.Sequence}
	}
	if applier.pendingEpoch == packet.Epoch {
		applier.pendingEpoch = 0
	}
	applier.mutex.Unlock()
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: true, Sequence: packet.Sequence, Epoch: packet.Epoch}, nil
}

func (applier *Applier) admit(packet syncstream.Packet) (syncstream.Packet, ApplyResult, error) {
	if packet.Observer != applier.observer {
		return packet, ApplyResult{}, ErrObserverMismatch
	}
	if packet.Epoch == 0 {
		return packet, ApplyResult{}, ErrEpochMismatch
	}
	if !applier.supported.Contains(packet.SchemaVersion) {
		return packet, ApplyResult{}, ErrSchemaMismatch
	}
	if packet.SchemaVersion != applier.schemaVersion {
		if applier.migrator == nil {
			return packet, ApplyResult{}, ErrSchemaMigratorRequired
		}
		payload, err := applier.migrator.Migrate(packet.Clone(), applier.schemaVersion)
		if err != nil {
			return packet, ApplyResult{}, err
		}
		packet.Payload, packet.SchemaVersion = payload, applier.schemaVersion
	}
	if packet.Stream.Topic == "" || packet.Sequence == 0 {
		return packet, ApplyResult{}, ErrPacketShape
	}
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	if applier.pendingEpoch != 0 {
		return packet, ApplyResult{}, ErrApplyInProgress
	}
	if _, busy := applier.inflight[packet.Stream]; busy {
		return packet, ApplyResult{}, ErrApplyInProgress
	}
	if applier.epoch != 0 && packet.Epoch != applier.epoch {
		if !packet.Full {
			return packet, ApplyResult{}, ErrEpochMismatch
		}
		if len(applier.inflight) > 0 {
			return packet, ApplyResult{}, ErrApplyInProgress
		}
		applier.pendingEpoch = packet.Epoch
	} else if applier.epoch == 0 {
		if !packet.Full || len(applier.inflight) > 0 {
			return packet, ApplyResult{}, ErrEpochMismatch
		}
		applier.pendingEpoch = packet.Epoch
	}
	current := applier.sequences[packet.Stream]
	if applier.epoch != packet.Epoch {
		current = streamCursor{}
	}
	if current.epoch == packet.Epoch && packet.Sequence <= current.sequence {
		return packet, ApplyResult{Duplicate: true, Sequence: current.sequence, Epoch: packet.Epoch}, nil
	}
	if packet.Full {
		if packet.BaseSequence != 0 {
			return packet, ApplyResult{}, ErrPacketShape
		}
	} else if (current.epoch != 0 && (packet.BaseSequence != current.sequence || packet.Sequence != current.sequence+1)) || (current.epoch == 0 && (packet.BaseSequence != 0 || packet.Sequence != 1)) {
		return packet, ApplyResult{}, fmt.Errorf("%w: current=%d base=%d sequence=%d", ErrSequenceGap, current.sequence, packet.BaseSequence, packet.Sequence)
	}
	applier.inflight[packet.Stream] = struct{}{}
	return packet, ApplyResult{}, nil
}

type preparedRecord struct {
	commitState func()
	transaction ApplyTransaction
}

func (applier *Applier) applyRecord(packet syncstream.Packet) (preparedRecord, error) {
	var header Header
	if err := json.Unmarshal(packet.Payload, &header); err != nil {
		return preparedRecord{}, fmt.Errorf("%w: %v", ErrRecordInvalid, err)
	}
	if header.SchemaVersion != packet.SchemaVersion {
		return preparedRecord{}, ErrSchemaMismatch
	}
	switch packet.Stream.Topic {
	case TopicManifest:
		if !packet.Full || header.Kind != RecordManifest {
			return preparedRecord{}, ErrPacketShape
		}
		var record ManifestRecord
		if err := decodeStrict(packet.Payload, &record); err != nil {
			return preparedRecord{}, err
		}
		digest := record.Plan.Identity.PresentationDigest
		if digest == "" || digest != record.PresentationDigest {
			return preparedRecord{}, fmt.Errorf("%w: manifest presentation digest", ErrRecordInvalid)
		}
		transaction, err := prepareManifest(applier.manifestConsumer, packet.Stream.Key, record.Plan)
		if err != nil {
			return preparedRecord{}, err
		}
		return preparedRecord{commitState: func() { applier.manifests[digest] = record.Plan }, transaction: transaction}, nil
	case TopicState:
		var record StateRecord
		if err := decodeStrict(packet.Payload, &record); err != nil {
			return preparedRecord{}, err
		}
		switch record.Kind {
		case RecordStateFull:
			if !packet.Full || record.Snapshot == nil || record.Delta != nil {
				return preparedRecord{}, ErrPacketShape
			}
			transaction, err := prepareStateSnapshot(applier.stateConsumer, packet.Stream.Key, *record.Snapshot)
			if err != nil {
				return preparedRecord{}, err
			}
			sequence := record.Snapshot.LatestStateMutationSequence
			return preparedRecord{commitState: func() { applier.stateMutations[packet.Stream.Key] = sequence }, transaction: transaction}, nil
		case RecordStateDelta:
			if packet.Full || record.Delta == nil || record.Snapshot != nil || record.Delta.Sequence == 0 || record.Delta.Kind == "" {
				return preparedRecord{}, ErrPacketShape
			}
			applier.mutex.Lock()
			current := applier.stateMutations[packet.Stream.Key]
			applier.mutex.Unlock()
			if record.Delta.Sequence <= current {
				return preparedRecord{}, nil
			}
			transaction, err := prepareStateDelta(applier.stateConsumer, packet.Stream.Key, *record.Delta)
			if err != nil {
				return preparedRecord{}, err
			}
			sequence := record.Delta.Sequence
			return preparedRecord{commitState: func() { applier.stateMutations[packet.Stream.Key] = sequence }, transaction: transaction}, nil
		default:
			return preparedRecord{}, ErrPacketShape
		}
	case TopicPresentation:
		var record PresentationRecord
		if err := decodeStrict(packet.Payload, &record); err != nil {
			return preparedRecord{}, err
		}
		switch record.Kind {
		case RecordPresentationReset:
			if !packet.Full || record.Reset == nil || record.Event != nil {
				return preparedRecord{}, ErrPacketShape
			}
			transaction, err := preparePresentationReset(applier.presentationConsumer, packet.Stream.Key, *record.Reset)
			if err != nil {
				return preparedRecord{}, err
			}
			sequence := record.Reset.Recovery.LatestPresentationSequence
			return preparedRecord{commitState: func() { applier.presentationEvents[packet.Stream.Key] = sequence }, transaction: transaction}, nil
		case RecordPresentation:
			if packet.Full || record.Event == nil || record.Reset != nil || record.Event.Sequence == 0 || record.Event.Kind == "" {
				return preparedRecord{}, ErrPacketShape
			}
			applier.mutex.Lock()
			_, manifest := applier.manifests[record.Event.PresentationDigest]
			current := applier.presentationEvents[packet.Stream.Key]
			applier.mutex.Unlock()
			if record.Event.PresentationDigest != "" && !manifest {
				return preparedRecord{}, ErrManifestMissing
			}
			if record.Event.Sequence <= current {
				return preparedRecord{}, nil
			}
			transaction, err := preparePresentation(applier.presentationConsumer, packet.Stream.Key, *record.Event)
			if err != nil {
				return preparedRecord{}, err
			}
			sequence := record.Event.Sequence
			return preparedRecord{commitState: func() { applier.presentationEvents[packet.Stream.Key] = sequence }, transaction: transaction}, nil
		default:
			return preparedRecord{}, ErrPacketShape
		}
	default:
		return preparedRecord{}, ErrTopicUnsupported
	}
}

func prepareManifest(consumer ManifestConsumer, key int64, plan skill.PresentationPlan) (ApplyTransaction, error) {
	if consumer == nil {
		return nil, nil
	}
	if transactional, ok := consumer.(TransactionalManifestConsumer); ok {
		transaction, err := transactional.PrepareManifest(key, plan)
		return requireTransaction(transaction, err)
	}
	return nil, consumer.ApplyManifest(key, plan)
}

func prepareStateSnapshot(consumer StateConsumer, key int64, snapshot skill.RuntimeStateSnapshot) (ApplyTransaction, error) {
	if consumer == nil {
		return nil, nil
	}
	if transactional, ok := consumer.(TransactionalStateConsumer); ok {
		transaction, err := transactional.PrepareStateSnapshot(key, snapshot)
		return requireTransaction(transaction, err)
	}
	return nil, consumer.ApplyStateSnapshot(key, snapshot)
}

func prepareStateDelta(consumer StateConsumer, key int64, mutation skill.StateMutation) (ApplyTransaction, error) {
	if consumer == nil {
		return nil, nil
	}
	if transactional, ok := consumer.(TransactionalStateConsumer); ok {
		transaction, err := transactional.PrepareStateDelta(key, mutation)
		return requireTransaction(transaction, err)
	}
	return nil, consumer.ApplyStateDelta(key, mutation)
}

func preparePresentationReset(consumer PresentationConsumer, key int64, reset PresentationReset) (ApplyTransaction, error) {
	if consumer == nil {
		return nil, nil
	}
	if transactional, ok := consumer.(TransactionalPresentationConsumer); ok {
		transaction, err := transactional.PreparePresentationReset(key, reset)
		return requireTransaction(transaction, err)
	}
	return nil, consumer.ResetPresentation(key, reset)
}

func preparePresentation(consumer PresentationConsumer, key int64, event skill.PresentationEvent) (ApplyTransaction, error) {
	if consumer == nil {
		return nil, nil
	}
	if transactional, ok := consumer.(TransactionalPresentationConsumer); ok {
		transaction, err := transactional.PreparePresentation(key, event)
		return requireTransaction(transaction, err)
	}
	return nil, consumer.ApplyPresentation(key, event)
}

func requireTransaction(transaction ApplyTransaction, err error) (ApplyTransaction, error) {
	if err != nil {
		return nil, err
	}
	if transaction == nil {
		return nil, ErrTransactionRequired
	}
	return transaction, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", ErrRecordInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON content", ErrRecordInvalid)
	}
	return nil
}

func (applier *Applier) Sequence(stream syncstream.Stream) uint64 {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	return applier.sequences[stream].sequence
}
func (applier *Applier) Epoch() uint64 {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	return applier.epoch
}
