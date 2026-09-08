package skillsync

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

func TestProjectorBuildsStronglyTypedPackets(t *testing.T) {
	projector, err := NewProjector(2)
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{Kind: 1, ID: 9, Scope: "match-7"}
	plan := skill.PresentationPlan{Identity: skill.ProgramIdentityView{GameplayDigest: "game", PresentationDigest: "view"}}
	manifest, err := projector.ManifestPacket(observer, 7, plan)
	if err != nil || !manifest.Full || !manifest.Critical || manifest.Stream.Topic != TopicManifest {
		t.Fatalf("manifest packet = %#v err=%v", manifest, err)
	}
	snapshot := skill.RuntimeStateSnapshot{Tick: 3, WorldRevision: 4, Casts: []skill.CastStateSnapshot{{ID: 1}}, LatestStateEventSequence: 8}
	state, err := projector.StateSnapshotPacket(observer, 7, snapshot)
	if err != nil || !state.Full || state.Stream.Topic != TopicState {
		t.Fatalf("state packet = %#v err=%v", state, err)
	}
	delta, err := projector.StateDeltaPacket(observer, 7, skill.StateMutation{Sequence: 9, Kind: skill.StateMutationClock, Tick: 3, WorldRevision: 4})
	if err != nil || delta.Full || !delta.Critical {
		t.Fatalf("delta packet = %#v err=%v", delta, err)
	}
	if _, err := projector.StateDeltaPacket(observer, 7, skill.StateMutation{}); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("invalid delta error = %v", err)
	}
	presentation, err := projector.PresentationPacket(observer, 7, skill.PresentationEvent{Sequence: 10, Kind: skill.PresentationEffect, Tick: 3, WorldRevision: 4, GameplayDigest: "game", PresentationDigest: "view"})
	if err != nil || presentation.Full || presentation.Critical || presentation.Stream.Topic != TopicPresentation {
		t.Fatalf("presentation packet = %#v err=%v", presentation, err)
	}
}

type recordingConsumer struct {
	manifests     int
	snapshots     int
	deltas        int
	presentations int
	resets        int
}

func (consumer *recordingConsumer) ApplyManifest(int64, skill.PresentationPlan) error {
	consumer.manifests++
	return nil
}
func (consumer *recordingConsumer) ApplyStateSnapshot(int64, skill.RuntimeStateSnapshot) error {
	consumer.snapshots++
	return nil
}
func (consumer *recordingConsumer) ApplyStateDelta(int64, skill.StateMutation) error {
	consumer.deltas++
	return nil
}
func (consumer *recordingConsumer) ResetPresentation(int64, PresentationReset) error {
	consumer.resets++
	return nil
}
func (consumer *recordingConsumer) ApplyPresentation(int64, skill.PresentationEvent) error {
	consumer.presentations++
	return nil
}

func TestApplierValidatesChainObserverAndManifestDependency(t *testing.T) {
	projector, _ := NewProjector(1)
	observer := syncstream.Observer{ID: 3, Scope: "match"}
	consumer := &recordingConsumer{}
	applier, _ := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, Manifest: consumer, State: consumer, Presentation: consumer})
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	plan := skill.PresentationPlan{Identity: skill.ProgramIdentityView{PresentationDigest: "visual-v1"}}
	manifest, _ := projector.ManifestPacket(observer, 11, plan)
	manifest, _ = history.Append(manifest)
	if result, err := applier.Apply(manifest); err != nil || !result.Applied || consumer.manifests != 1 {
		t.Fatalf("manifest apply = %#v, %v", result, err)
	}
	event := skill.PresentationEvent{Sequence: 1, Kind: skill.PresentationCast, PresentationDigest: "visual-v1"}
	presentation, _ := projector.PresentationPacket(observer, 11, event)
	presentation, _ = history.Append(presentation)
	if _, err := applier.Apply(presentation); err != nil || consumer.presentations != 1 {
		t.Fatalf("presentation apply error = %v", err)
	}
	if result, err := applier.Apply(presentation); err != nil || !result.Duplicate || consumer.presentations != 1 {
		t.Fatalf("duplicate apply = %#v, %v", result, err)
	}

	missing, _ := projector.PresentationPacket(observer, 12, skill.PresentationEvent{Sequence: 1, Kind: skill.PresentationCast, PresentationDigest: "missing"})
	missing.Sequence, missing.Epoch = 1, manifest.Epoch
	if _, err := applier.Apply(missing); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("missing manifest error = %v", err)
	}
	gap := presentation.Clone()
	gap.Sequence = 3
	gap.BaseSequence = 2
	if _, err := applier.Apply(gap); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("gap error = %v", err)
	}
	foreign := manifest.Clone()
	foreign.Observer.ID++
	if _, err := applier.Apply(foreign); !errors.Is(err, ErrObserverMismatch) {
		t.Fatalf("observer error = %v", err)
	}
}

type recordingPublisher struct {
	packets  []syncstream.Packet
	failNext bool
}

func (publisher *recordingPublisher) Publish(packet syncstream.Packet) error {
	if publisher.failNext {
		publisher.failNext = false
		return errors.New("backpressure")
	}
	publisher.packets = append(publisher.packets, packet.Clone())
	return nil
}

func TestCoordinatorRetainsPublishFailureForRecovery(t *testing.T) {
	host := skill.NewMemoryHost(skill.AuthorityIdentity{})
	runtime := skill.NewRuntime(host, skill.RuntimeOptions{})
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	projector, _ := NewProjector(1)
	publisher := &recordingPublisher{failNext: true}
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: history, Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 4}
	if err := coordinator.PublishSnapshot(observer, 8); err == nil {
		t.Fatal("expected injected publish failure")
	}
	result, err := coordinator.Recover(syncstream.ResyncRequest{Observer: observer, Stream: syncstream.Stream{Topic: TopicState, Key: 8}, SchemaVersion: 1})
	if err != nil || result.FullRequired || len(result.Packets) != 1 || len(publisher.packets) != 1 {
		t.Fatalf("recover = %#v, packets=%d, err=%v", result, len(publisher.packets), err)
	}
	metrics := coordinator.Metrics()
	if metrics.PublishFailures != 1 || metrics.Published != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestCoordinatorRequiresExplicitVisibilityPolicy(t *testing.T) {
	projector, _ := NewProjector(1)
	_, err := NewCoordinator(CoordinatorOptions{
		Runtime: skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{}),
		History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector,
	})
	if !errors.Is(err, ErrVisibilityRequired) {
		t.Fatalf("visibility error = %v", err)
	}
}

func TestCoordinatorReclaimsViewLocksAndRequiresExplicitReopen(t *testing.T) {
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Runtime: skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{}),
		History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{},
	})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 90}
	for key := int64(1); key <= 100; key++ {
		if err := coordinator.PublishSnapshot(observer, key); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.mutex.RLock()
	locks := len(coordinator.viewLocks)
	coordinator.mutex.RUnlock()
	if locks != 0 {
		t.Fatalf("view locks retained = %d", locks)
	}
	coordinator.mutex.Lock()
	coordinator.closingObservers[observer] = struct{}{}
	coordinator.mutex.Unlock()
	if err := coordinator.CloseObserver(observer); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("concurrent close error = %v", err)
	}
	coordinator.mutex.Lock()
	delete(coordinator.closingObservers, observer)
	coordinator.mutex.Unlock()
	if err := coordinator.CloseObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.PublishSnapshot(observer, 1); !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("closed error = %v", err)
	}
	if err := coordinator.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
}

type reentrantStateConsumer struct {
	applier *Applier
	stream  syncstream.Stream
	calls   int
}

func (consumer *reentrantStateConsumer) ApplyStateSnapshot(int64, skill.RuntimeStateSnapshot) error {
	_ = consumer.applier.Sequence(consumer.stream)
	consumer.calls++
	return nil
}
func (consumer *reentrantStateConsumer) ApplyStateDelta(int64, skill.StateMutation) error {
	_ = consumer.applier.Sequence(consumer.stream)
	consumer.calls++
	return nil
}

func TestApplierDoesNotHoldMutexDuringConsumerCallback(t *testing.T) {
	observer := syncstream.Observer{ID: 8}
	stream := syncstream.Stream{Topic: TopicState, Key: 4}
	consumer := &reentrantStateConsumer{stream: stream}
	applier, _ := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	consumer.applier = applier
	projector, _ := NewProjector(1)
	history := syncstream.NewHistory(syncstream.HistoryOptions{Epoch: 9})
	packet, _ := projector.StateSnapshotPacket(observer, stream.Key, skill.RuntimeStateSnapshot{})
	packet, _ = history.Append(packet)
	if _, err := applier.Apply(packet); err != nil || consumer.calls != 1 {
		t.Fatalf("calls=%d err=%v", consumer.calls, err)
	}
}

type testApplyTransaction struct {
	commitErr error
	commits   *int
	rollbacks *int
}

func (transaction *testApplyTransaction) Commit() error {
	if transaction.commitErr != nil {
		return transaction.commitErr
	}
	*transaction.commits++
	return nil
}

func (transaction *testApplyTransaction) Rollback() { *transaction.rollbacks++ }

type transactionalStateConsumer struct {
	recordingConsumer
	commitErr error
	commits   int
	rollbacks int
}

func (consumer *transactionalStateConsumer) PrepareStateSnapshot(int64, skill.RuntimeStateSnapshot) (ApplyTransaction, error) {
	return &testApplyTransaction{commitErr: consumer.commitErr, commits: &consumer.commits, rollbacks: &consumer.rollbacks}, nil
}

func (consumer *transactionalStateConsumer) PrepareStateDelta(int64, skill.StateMutation) (ApplyTransaction, error) {
	return &testApplyTransaction{commitErr: consumer.commitErr, commits: &consumer.commits, rollbacks: &consumer.rollbacks}, nil
}

func TestTransactionalApplyRollsBackWithoutAdvancingEpochOrSequence(t *testing.T) {
	observer := syncstream.Observer{ID: 18}
	consumer := &transactionalStateConsumer{commitErr: errors.New("renderer unavailable")}
	applier, err := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	if err != nil {
		t.Fatal(err)
	}
	projector, _ := NewProjector(1)
	packet, _ := projector.StateSnapshotPacket(observer, 2, skill.RuntimeStateSnapshot{})
	packet.Epoch, packet.Sequence = 40, 1
	if _, err := applier.Apply(packet); err == nil {
		t.Fatal("expected transaction commit failure")
	}
	if applier.Epoch() != 0 || applier.Sequence(packet.Stream) != 0 || consumer.commits != 0 || consumer.rollbacks != 1 {
		t.Fatalf("epoch=%d sequence=%d commits=%d rollbacks=%d", applier.Epoch(), applier.Sequence(packet.Stream), consumer.commits, consumer.rollbacks)
	}
	consumer.commitErr = nil
	if result, err := applier.Apply(packet); err != nil || !result.Applied {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	if applier.Epoch() != 40 || applier.Sequence(packet.Stream) != 1 || consumer.commits != 1 {
		t.Fatalf("epoch=%d sequence=%d commits=%d", applier.Epoch(), applier.Sequence(packet.Stream), consumer.commits)
	}
}

type jsonSchemaMigrator struct{}

func (jsonSchemaMigrator) Migrate(packet syncstream.Packet, target uint32) ([]byte, error) {
	var value map[string]any
	if err := json.Unmarshal(packet.Payload, &value); err != nil {
		return nil, err
	}
	value["schema_version"] = target
	return json.Marshal(value)
}

func TestSchemaNegotiationMigrationAndEpochSwitch(t *testing.T) {
	if version, err := NegotiateSchema(SchemaRange{Min: 1, Max: 3}, SchemaRange{Min: 2, Max: 4}); err != nil || version != 3 {
		t.Fatalf("schema=%d err=%v", version, err)
	}
	observer := syncstream.Observer{ID: 1}
	consumer := &recordingConsumer{}
	applier, _ := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 2, SupportedSchema: SchemaRange{Min: 1, Max: 2}, Migrator: jsonSchemaMigrator{}, Manifest: consumer})
	projector, _ := NewProjector(1)
	plan := skill.PresentationPlan{Identity: skill.ProgramIdentityView{PresentationDigest: "v1"}}
	packet, _ := projector.ManifestPacket(observer, 1, plan)
	packet.Epoch, packet.Sequence = 10, 1
	if _, err := applier.Apply(packet); err != nil {
		t.Fatal(err)
	}
	newEpoch := packet.Clone()
	newEpoch.Epoch = 11
	if _, err := applier.Apply(newEpoch); err != nil || applier.Epoch() != 11 {
		t.Fatalf("epoch=%d err=%v", applier.Epoch(), err)
	}
	oldDelta := packet.Clone()
	oldDelta.Full, oldDelta.BaseSequence, oldDelta.Sequence = false, 1, 2
	if _, err := applier.Apply(oldDelta); !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("old epoch error = %v", err)
	}
}

func TestEntityVisibilityPolicyRedactsNestedTargets(t *testing.T) {
	policy := EntityVisibilityPolicy{Visible: func(_ syncstream.Observer, entity skill.EntityID) (bool, error) { return entity != 99, nil }}
	snapshot := skill.RuntimeStateSnapshot{Casts: []skill.CastStateSnapshot{{ID: 1, Caster: 1, PrimaryTarget: 99}, {ID: 2, Caster: 99}}}
	filtered, err := policy.FilterStateSnapshot(syncstream.Observer{}, snapshot)
	if err != nil || len(filtered.Casts) != 1 || filtered.Casts[0].PrimaryTarget != 0 {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
	event := skill.PresentationEvent{Anchor: skill.PresentationAnchor{Source: 1, Target: 99}, PrimaryTarget: 99}
	event, allowed, err := policy.FilterPresentation(syncstream.Observer{}, event)
	if err != nil || !allowed || event.Anchor.Target != 0 || event.PrimaryTarget != 0 {
		t.Fatalf("event=%#v allowed=%v err=%v", event, allowed, err)
	}
}

func TestEntityVisibilityPolicyDefaultDenyAndRecursiveValueRedaction(t *testing.T) {
	policy := EntityVisibilityPolicy{
		Visible:           func(_ syncstream.Observer, entity skill.EntityID) (bool, error) { return entity != 99, nil },
		DefaultDenyFields: true,
		RedactSpatial:     true,
		FieldVisible: func(_ syncstream.Observer, field VisibilityField, _ string) (bool, error) {
			switch field {
			case VisibilityPersistentState, VisibilityPersistentValue, VisibilityProcesses, VisibilityPresentation:
				return true, nil
			default:
				return false, nil
			}
		},
	}
	snapshot := skill.RuntimeStateSnapshot{
		Casts:            []skill.CastStateSnapshot{{ID: 1, Caster: 1}},
		Processes:        []skill.ProcessStateSnapshot{{ID: 1, Owner: 1, Motion: skill.MotionState{Position: skill.Position{X: 10}, CarryTarget: 99}}},
		PersistentStates: []skill.PersistentStateSnapshot{{Handle: skill.StateHandle{GameplayDigest: "g", Slot: 1}, Binding: skill.StateScopeBinding{Owner: 1}, Value: skill.EntityListRuntimeValue([]skill.EntityID{1, 99})}},
	}
	filtered, err := policy.FilterStateSnapshot(syncstream.Observer{}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Casts) != 0 || len(filtered.Processes) != 1 || filtered.Processes[0].Motion.Position != (skill.Position{}) || filtered.Processes[0].Motion.CarryTarget != 0 {
		t.Fatalf("filtered process = %#v", filtered)
	}
	entities, ok := filtered.PersistentStates[0].Value.Entities()
	if !ok || len(entities) != 1 || entities[0] != 1 {
		t.Fatalf("persistent entities = %v, %v", entities, ok)
	}
	event := skill.PresentationEvent{Anchor: skill.PresentationAnchor{Source: 1, Position: &skill.Position{X: 5}, Path: []skill.Position{{X: 6}}}}
	redacted, allowed, err := policy.FilterPresentation(syncstream.Observer{}, event)
	if err != nil || !allowed || redacted.Anchor.Position != nil || redacted.Anchor.Path != nil {
		t.Fatalf("event=%#v allowed=%v err=%v", redacted, allowed, err)
	}
}

func TestFileOutboxSurvivesRestartAndDeletesOnAck(t *testing.T) {
	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, AckRetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 2}, Stream: syncstream.Stream{Topic: TopicState, Key: 3}, Epoch: 7, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Metrics().Pending != 1 {
		t.Fatalf("pending=%d", restarted.Metrics().Pending)
	}
	if err := restarted.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); err != nil {
		t.Fatal(err)
	}
	third, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil || third.Metrics().Pending != 0 {
		t.Fatalf("pending=%d err=%v", third.Metrics().Pending, err)
	}
}

func TestOutboxEnforcesHardCapacityAndPersistedAge(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{MaxPendingPackets: 1, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 1, MaxPendingAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Stream.Key, second.Sequence = 2, 2
	if err := box.Put(second); !errors.Is(err, ErrOutboxCapacityExceeded) {
		t.Fatalf("capacity error = %v", err)
	}
	metrics := box.Metrics()
	if metrics.Pending != 1 || metrics.PendingBytes == 0 {
		t.Fatalf("metrics = %#v", metrics)
	}

	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(OutboxRecord{Packet: first, CreatedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, MaxPendingAge: time.Hour}); !errors.Is(err, ErrOutboxPendingTooOld) {
		t.Fatalf("stale recovery error = %v", err)
	}
}

func TestFileOutboxRejectsChecksumAndIdentityCorruption(t *testing.T) {
	directory := t.TempDir()
	store, err := NewFileOutboxStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 4}, Stream: syncstream.Stream{Topic: TopicState, Key: 2}, Epoch: 3, Sequence: 1, Payload: []byte("state")}
	if err := store.Put(packet); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	path := directory + string(os.PathSeparator) + entries[0].Name()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope fileOutboxEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Checksum = "00"
	data, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRecords(); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("corrupt load=%v", err)
	}
}

type failingBatchDeleteStore struct {
	records []OutboxRecord
	err     error
}

func (store *failingBatchDeleteStore) Load() ([]syncstream.Packet, error) {
	result := make([]syncstream.Packet, len(store.records))
	for index := range store.records {
		result[index] = store.records[index].Packet.Clone()
	}
	return result, nil
}
func (store *failingBatchDeleteStore) LoadRecords() ([]OutboxRecord, error) {
	return append([]OutboxRecord(nil), store.records...), nil
}
func (store *failingBatchDeleteStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet, CreatedAt: time.Now()})
}
func (store *failingBatchDeleteStore) PutRecord(record OutboxRecord) error {
	store.records = append(store.records, record)
	return nil
}
func (*failingBatchDeleteStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}
func (store *failingBatchDeleteStore) DeleteBatch([]OutboxDelete) error { return store.err }

func TestOutboxBatchDeleteFailureDoesNotPartiallyAcknowledge(t *testing.T) {
	injected := errors.New("injected batch delete failure")
	store := &failingBatchDeleteStore{err: injected}
	box, err := NewOutbox(OutboxOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 8}
	stream := syncstream.Stream{Topic: TopicState, Key: 9}
	if err := box.PutBatch([]syncstream.Packet{
		{Observer: observer, Stream: stream, Epoch: 1, Sequence: 1},
		{Observer: observer, Stream: stream, Epoch: 1, Sequence: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.Acknowledge(observer, stream, 1, 2); !errors.Is(err, injected) {
		t.Fatalf("ack=%v", err)
	}
	if metrics := box.Metrics(); metrics.Pending != 2 || metrics.Acknowledged != 0 {
		t.Fatalf("metrics=%#v", metrics)
	}
}
