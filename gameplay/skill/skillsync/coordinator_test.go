package skillsync

import (
	"errors"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

type deleteFailureStore struct {
	records map[pendingID]OutboxRecord
	err     error
	failAt  int
	deletes int
}

type acknowledgeFailureJournal struct {
	fail error
}

func (journal *acknowledgeFailureJournal) Load() (syncstream.HistorySnapshot, error) {
	return syncstream.HistorySnapshot{Version: syncstream.HistorySnapshotVersion, Epoch: 1}, nil
}
func (journal *acknowledgeFailureJournal) Record(mutation syncstream.HistoryMutation) error {
	if mutation.Kind == syncstream.HistoryMutationAcknowledge {
		return journal.fail
	}
	return nil
}
func (*acknowledgeFailureJournal) Checkpoint(syncstream.HistorySnapshot) error { return nil }

func coordinatorForDurabilityTest(t *testing.T, history *syncstream.History, box *Outbox) *Coordinator {
	t.Helper()
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{}), History: history, Outbox: box, Projector: projector, Publisher: &recordingPublisher{}, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range history.Export().Streams {
		if err := c.OpenObserver(stream.Observer); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (store *deleteFailureStore) Load() ([]syncstream.Packet, error) {
	result := make([]syncstream.Packet, 0, len(store.records))
	for _, record := range store.records {
		result = append(result, record.Packet.Clone())
	}
	return result, nil
}
func (store *deleteFailureStore) LoadRecords() ([]OutboxRecord, error) {
	result := make([]OutboxRecord, 0, len(store.records))
	for _, record := range store.records {
		result = append(result, record)
	}
	return result, nil
}
func (store *deleteFailureStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet})
}
func (store *deleteFailureStore) PutRecord(record OutboxRecord) error {
	if store.records == nil {
		store.records = make(map[pendingID]OutboxRecord)
	}
	store.records[packetID(record.Packet)] = record
	return nil
}
func (store *deleteFailureStore) Delete(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) error {
	store.deletes++
	if store.err != nil && (store.failAt == 0 || store.deletes == store.failAt) {
		return store.err
	}
	delete(store.records, pendingID{observer: observer, stream: stream, epoch: epoch, sequence: sequence})
	return nil
}

func TestCoordinatorRepairsPartialOutboxDeleteFailure(t *testing.T) {
	injected := errors.New("second delete failed")
	store := &deleteFailureStore{err: injected, failAt: 2}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	var latest syncstream.Packet
	for index := 0; index < 2; index++ {
		latest, err = history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := box.Put(latest); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := coordinatorForDurabilityTest(t, history, box)
	if err := coordinator.Acknowledge(latest.Observer, latest.Stream, latest.Epoch, latest.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	if metrics := box.Metrics(); metrics.Pending != 2 {
		t.Fatalf("partial delete was not reconciled: %#v", metrics)
	}
	if status := history.Status(latest.Observer, latest.Stream); status.AckedSequence != 0 {
		t.Fatalf("partial outbox failure advanced History: %#v", status)
	}
}

func TestCoordinatorAckKeepsHistoryWhenOutboxDeleteFails(t *testing.T) {
	injected := errors.New("delete failed")
	store := &deleteFailureStore{err: injected}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(t, history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	status := history.Status(packet.Observer, packet.Stream)
	if status.AckedSequence != 0 || status.Retained != 1 {
		t.Fatalf("history advanced before outbox durability: %#v", status)
	}
}

func TestCoordinatorRejectsAckAheadBeforeDeletingOutbox(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(t, history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence+1); !errors.Is(err, syncstream.ErrAckAhead) {
		t.Fatalf("ack error = %v", err)
	}
	if metrics := box.Metrics(); metrics.Pending != 1 {
		t.Fatalf("outbox deleted before ACK validation: %#v", metrics)
	}
}

func TestCoordinatorRepairsOutboxWhenHistoryAckFails(t *testing.T) {
	injected := errors.New("journal failed")
	journal := &acknowledgeFailureJournal{fail: injected}
	history, err := syncstream.NewHistoryWithJournal(syncstream.HistoryOptions{SchemaVersion: 1, Epoch: 1}, journal)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(t, history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	if status := history.Status(packet.Observer, packet.Stream); status.AckedSequence != 0 {
		t.Fatalf("failed ACK advanced History: %#v", status)
	}
	if metrics := box.Metrics(); metrics.Pending != 1 {
		t.Fatalf("failed ACK did not repair outbox: %#v", metrics)
	}
}

func TestCoordinatorCloseKeepsHistoryWhenOutboxDeleteFails(t *testing.T) {
	injected := errors.New("delete failed")
	store := &deleteFailureStore{err: injected}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(t, history, box)
	if err := coordinator.CloseObserver(packet.Observer); !errors.Is(err, injected) {
		t.Fatalf("close error = %v", err)
	}
	if status := history.Status(packet.Observer, packet.Stream); status.Retained != 1 {
		t.Fatalf("history deleted before outbox durability: %#v", status)
	}
}

// 固定 acquire 已登记引用、尚未拿到 view 锁时发生关闭的交错。
func TestAcquireRechecksObserverAfterWaiting(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	view := observerKey{observer: observer, key: 1}
	entry := &viewLockEntry{}
	entry.mutex.Lock()
	c.viewLocks[view] = entry
	done := make(chan error, 1)
	go func() {
		release, err := c.acquireView(view)
		if err == nil {
			release()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.mutex.Lock()
		refs := entry.refs
		c.mutex.Unlock()
		if refs == 1 {
			break
		}
		if time.Now().After(deadline) {
			entry.mutex.Unlock()
			t.Fatal("acquire did not register view")
		}
		goruntime.Gosched()
	}
	c.mutex.Lock()
	delete(c.activeObservers, observer) // CloseObserver 的拒绝新请求状态。
	c.mutex.Unlock()
	entry.mutex.Unlock()
	if err := <-done; !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("waiting acquire entered closed view: %v", err)
	}
}

func TestCoordinatorExplicitLifecycleCapacityAndManifestIdentity(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	publisher := &recordingPublisher{}
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}, MaxObservers: 1, MaxPrograms: 1})
	if err != nil {
		t.Fatal(err)
	}
	one, two := syncstream.Observer{ID: 1}, syncstream.Observer{ID: 2}
	if err := c.PublishSnapshot(one, 1); !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("implicit observer opened: %v", err)
	}
	if err := c.OpenObserver(one); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenObserver(two); !errors.Is(err, ErrCoordinatorCapacity) {
		t.Fatalf("observer limit=%v", err)
	}
	if err := c.RegisterProgram(1, program); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterProgram(1, program); err != nil {
		t.Fatalf("idempotent registration=%v", err)
	}
	if err := c.PublishManifest(one, 1); err != nil {
		t.Fatal(err)
	}
	if len(publisher.packets) != 1 || publisher.packets[0].Stream.Topic != TopicManifest {
		t.Fatal("registered manifest not published")
	}
	definition, err := skill.Parse([]byte(strings.ReplaceAll(visibleWaitSkill, "visible_wait", "other_wait")))
	if err != nil {
		t.Fatal(err)
	}
	other, diagnostics := skill.Compile(definition, skill.DefaultCompileEnvironment())
	for _, d := range diagnostics {
		if d.Severity == skill.DiagnosticError {
			t.Fatal(d)
		}
	}
	if err := c.RegisterProgram(1, other); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("manifest overwritten: %v", err)
	}
	if err := c.RegisterProgram(2, other); !errors.Is(err, ErrCoordinatorCapacity) {
		t.Fatalf("manifest capacity=%v", err)
	}
	c.UnregisterProgram(1)
	if err := c.RegisterProgram(2, other); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseObserver(one); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenObserver(two); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(one, 1); !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("old observer reopened: %v", err)
	}
}

func TestCoordinatorCloseMustFinishBeforeReopen(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	publisher := &perObserverGate{entered: make(chan int64, 1), releases: map[int64]chan struct{}{1: make(chan struct{})}}
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.PublishSnapshot(observer, 1) }()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		close(publisher.releases[1])
		t.Fatal("publisher did not enter")
	}
	closeErr := c.CloseObserver(observer)
	openErr := c.OpenObserver(observer)
	close(publisher.releases[1])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(closeErr, ErrApplyInProgress) || !errors.Is(openErr, ErrApplyInProgress) {
		t.Fatalf("close=%v reopen=%v", closeErr, openErr)
	}
	if err := c.CloseObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryPending(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c.outbox.Metrics().Pending != 0 {
		t.Fatal("closed observer retained pending packets")
	}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
}

func TestClosedObserverIdentitiesDoNotAccumulate(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 2000; id++ {
		observer := syncstream.Observer{ID: id}
		if err := c.OpenObserver(observer); err != nil {
			t.Fatal(err)
		}
		if err := c.CloseObserver(observer); err != nil {
			t.Fatal(err)
		}
	}
	// 检查 Coordinator 自己持有的索引；不把外部 History/Runtime 的状态混进来。
	value := reflect.ValueOf(c).Elem()
	for index := range value.NumField() {
		field := value.Field(index)
		if field.Kind() == reflect.Map && field.Len() != 0 {
			t.Errorf("closed observer retained in %s: %d entries", value.Type().Field(index).Name, field.Len())
		}
	}
}

func TestRegisteredManifestPlansAreBounded(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	for key := int64(1); key <= 2048; key++ {
		c.RegisterProgram(key, program)
	}
	if len(c.plans) > 1024 {
		t.Fatalf("manifest registry retained %d plans", len(c.plans))
	}
}

type perObserverGate struct {
	entered  chan int64
	releases map[int64]chan struct{}
}

func (p *perObserverGate) Publish(packet syncstream.Packet) error {
	p.entered <- packet.Observer.ID
	<-p.releases[packet.Observer.ID]
	return nil
}

// 两个发布窗口重叠不能重复累加全局 outbox 计数：每次发布只计一次。
func TestConcurrentCoordinatorMetricsCountEachPublishOnce(t *testing.T) {
	publisher := &perObserverGate{entered: make(chan int64, 2), releases: map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}}
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{}), History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	stream := syncstream.Stream{Topic: TopicState, Key: 1}
	for id := int64(1); id <= 2; id++ {
		if err := coordinator.OpenObserver(syncstream.Observer{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.outbox.Put(syncstream.Packet{Observer: syncstream.Observer{ID: id}, Stream: stream, Epoch: 1, Sequence: 1}); err != nil {
			t.Fatal(err)
		}
	}
	results := []chan error{make(chan error, 1), make(chan error, 1)}
	for i := range results {
		go func(i int) { results[i] <- coordinator.publishDue(syncstream.Observer{ID: int64(i + 1)}, stream) }(i)
	}
	defer func() {
		for _, ch := range publisher.releases {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	for range 2 {
		select {
		case <-publisher.entered:
		case <-time.After(time.Second):
			t.Fatal("publish did not enter")
		}
	}
	close(publisher.releases[1])
	if err := <-results[0]; err != nil {
		t.Fatal(err)
	}
	close(publisher.releases[2])
	if err := <-results[1]; err != nil {
		t.Fatal(err)
	}
	if got := coordinator.Metrics().Published; got != 2 {
		t.Fatalf("published=%d want 2", got)
	}
}
