package skillsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

func batchTestPackets(spec ...[2]int) []syncstream.Packet {
	var out []syncstream.Packet
	for _, item := range spec {
		stream, count := item[0], item[1]
		for seq := 1; seq <= count; seq++ {
			out = append(out, syncstream.Packet{
				Observer: syncstream.Observer{ID: 1},
				Stream:   syncstream.Stream{Topic: TopicState, Key: int64(stream)},
				Epoch:    1, Sequence: uint64(seq), SchemaVersion: 1,
				Payload: []byte(fmt.Sprintf("stream-%d-seq-%d-payload", stream, seq)),
			})
		}
	}
	return out
}

func TestPutAndPutBatchReachTheSameAdmissionVerdict(t *testing.T) {
	wide := OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}
	duplicated := batchTestPackets([2]int{1, 2})
	duplicated = append(duplicated, duplicated[0]) // same packet twice in one batch
	cases := []struct {
		name    string
		options OutboxOptions
		packets []syncstream.Packet
	}{
		{"within every limit", wide, batchTestPackets([2]int{1, 3}, [2]int{2, 2})},
		{"total packet limit", OutboxOptions{MaxPendingPackets: 3, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 2}, [2]int{2, 2})},
		{"total byte limit", OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 64, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 4})},
		{"per-stream limit", OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 2}, batchTestPackets([2]int{1, 3}, [2]int{2, 1})},
		{"duplicates in the batch", wide, duplicated},
		{"exactly at the packet limit", OutboxOptions{MaxPendingPackets: 4, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 2}, [2]int{2, 2})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sequential, err := NewOutbox(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			var putErr error
			for _, packet := range tc.packets {
				if putErr = sequential.Put(packet); putErr != nil {
					break
				}
			}
			batched, err := NewOutbox(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			batchErr := batched.PutBatch(tc.packets)

			if (putErr == nil) != (batchErr == nil) {
				t.Fatalf("verdicts differ: sequential Put=%v, PutBatch=%v", putErr, batchErr)
			}
			if putErr != nil {
				if !errors.Is(batchErr, putErr) {
					t.Fatalf("refusals differ: sequential Put=%v, PutBatch=%v", putErr, batchErr)
				}
				// 拒绝的批不能留下半批：PutBatch 是全有或全无。
				if batched.Metrics().Pending != 0 {
					t.Fatalf("refused PutBatch left %d packets pending", batched.Metrics().Pending)
				}
				return
			}
			seq, bat := sequential.Metrics(), batched.Metrics()
			if seq.Pending != bat.Pending || seq.PendingBytes != bat.PendingBytes {
				t.Fatalf("accepted state differs: sequential pending=%d bytes=%d, batch pending=%d bytes=%d", seq.Pending, seq.PendingBytes, bat.Pending, bat.PendingBytes)
			}
		})
	}
}

type sequencePublisher struct {
	sequences []uint64
	fail      uint64
}

func (p *sequencePublisher) Publish(packet syncstream.Packet) error {
	p.sequences = append(p.sequences, packet.Sequence)
	if packet.Sequence == p.fail {
		return errors.New("transport rejected packet")
	}
	return nil
}

func TestOutboxFailureBlocksLaterPacketsInSameStream(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := box.Put(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: sequence}); err != nil {
			t.Fatal(err)
		}
	}
	p := &sequencePublisher{fail: 2}
	now := time.Now()
	if err := box.PublishDue(p, now, nil, nil); err == nil {
		t.Fatal("publish failure hidden")
	}
	if len(p.sequences) != 2 {
		t.Fatalf("sent sequences=%v; sequence 3 crossed failed sequence 2", p.sequences)
	}
	p.sequences = nil
	if err := box.PublishDue(p, now.Add(time.Millisecond), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 0 {
		t.Fatalf("retry backoff bypassed: %v", p.sequences)
	}
	p.fail = 0
	if err := box.PublishDue(p, now.Add(time.Second), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 2 || p.sequences[0] != 2 || p.sequences[1] != 3 {
		t.Fatalf("recovery order=%v", p.sequences)
	}
}

func TestOverageOutboxCanRestartPublishAndAcknowledge(t *testing.T) {
	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}
	if err := store.PutRecord(OutboxRecord{Packet: packet, CreatedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{Store: store, MaxPendingAge: time.Hour})
	if err != nil {
		t.Fatalf("restart cannot recover old pending packet: %v", err)
	}
	p := &sequencePublisher{}
	if err := box.PublishDue(p, time.Now(), nil, nil); err != nil {
		t.Fatalf("old pending packet cannot be retried: %v", err)
	}
	if len(p.sequences) != 1 {
		t.Fatalf("published=%v", p.sequences)
	}
	if err := box.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); err != nil {
		t.Fatal(err)
	}
	if box.Metrics().Pending != 0 {
		t.Fatal("ACK did not drain old packet")
	}
}

func TestIdleSweepPreservesUnacknowledgedDelivery(t *testing.T) {
	history := syncstream.NewHistory(syncstream.HistoryOptions{IdleTTL: time.Second})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := history.SweepIdle(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := history.AcknowledgeEpoch(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); err != nil {
		t.Fatalf("idle sweep orphaned pending ACK: %v", err)
	}
	if removed, err := history.SweepIdle(time.Now().Add(time.Hour)); err != nil || removed != 1 {
		t.Fatalf("acknowledged idle stream not reclaimed: removed=%d err=%v", removed, err)
	}
}

func TestDiscardObserverCannotSucceedWithPublishInFlight(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := box.Put(syncstream.Packet{Observer: observer, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	publisher := &perObserverGate{entered: make(chan int64, 1), releases: map[int64]chan struct{}{1: make(chan struct{})}}
	done := make(chan error, 1)
	go func() { done <- box.PublishDue(publisher, time.Now(), nil, nil) }()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		close(publisher.releases[1])
		t.Fatal("publish did not enter")
	}
	err = box.DiscardObserver(observer)
	close(publisher.releases[1])
	if publishErr := <-done; publishErr != nil {
		t.Fatal(publishErr)
	}
	if !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("discard reported completion while publisher still active: %v", err)
	}
	if err := box.DiscardObserver(observer); err != nil {
		t.Fatal(err)
	}
	if box.Metrics().Pending != 0 {
		t.Fatal("drained observer retained packets")
	}
}

func TestEpochRotationRetiresOldOutboxBeforeRetry(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	history := syncstream.NewHistory(syncstream.HistoryOptions{Epoch: 1})
	p := &sequencePublisher{}
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: history, Publisher: p, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
	p.sequences = nil
	if err := history.RotateEpoch(2); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryPending(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 0 || c.outbox.Metrics().Pending != 0 {
		t.Fatalf("old epoch still pending: published=%v pending=%d", p.sequences, c.outbox.Metrics().Pending)
	}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
	status := history.Status(observer, syncstream.Stream{Topic: TopicState, Key: 1})
	if err := c.Acknowledge(observer, syncstream.Stream{Topic: TopicState, Key: 1}, 2, status.LatestSequence); err != nil {
		t.Fatal(err)
	}
}

type failingUpdateStore struct {
	record    OutboxRecord
	writes    int
	updateErr error
}

func (store *failingUpdateStore) Load() ([]syncstream.Packet, error) { return nil, nil }
func (store *failingUpdateStore) LoadRecords() ([]OutboxRecord, error) {
	if store.writes == 0 {
		return nil, nil
	}
	return []OutboxRecord{store.record}, nil
}
func (store *failingUpdateStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet, CreatedAt: time.Now()})
}
func (store *failingUpdateStore) PutRecord(record OutboxRecord) error {
	store.writes++
	if store.writes > 1 {
		return store.updateErr
	}
	store.record = record
	return nil
}
func (*failingUpdateStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}

type fixedErrorPublisher struct{ err error }

func (publisher fixedErrorPublisher) Publish(syncstream.Packet) error { return publisher.err }

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (publisher *blockingPublisher) Publish(syncstream.Packet) error {
	publisher.once.Do(func() { close(publisher.started) })
	<-publisher.release
	return nil
}

func TestPublishPersistsRetryMetadata(t *testing.T) {
	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, AckRetryInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := box.PublishDue(&recordingPublisher{}, now, nil, nil); err != nil {
		t.Fatal(err)
	}
	records, err := store.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Attempts != 1 || !records[0].NextAttempt.Equal(now.Add(time.Hour)) {
		t.Fatalf("records=%#v", records)
	}
	restarted, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, AckRetryInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	if err := restarted.PublishDue(publisher, now.Add(time.Minute), nil, nil); err != nil || len(publisher.packets) != 0 {
		t.Fatalf("packets=%d err=%v", len(publisher.packets), err)
	}
}

func TestPublishDueHonorsBatchLimit(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{MaxPublishBatch: 2})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := box.Put(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: sequence}); err != nil {
			t.Fatal(err)
		}
	}
	publisher := &recordingPublisher{}
	if err := box.PublishDue(publisher, time.Now(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(publisher.packets) != 2 {
		t.Fatalf("published=%d", len(publisher.packets))
	}
}

func TestConcurrentPublishDueDoesNotPublishSamePacket(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	publisher := &blockingPublisher{started: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() { firstDone <- box.PublishDue(publisher, time.Now(), nil, nil) }()
	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		t.Fatal("first publisher did not start")
	}

	second := &recordingPublisher{}
	if err := box.PublishDue(second, time.Now(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(second.packets) != 0 {
		t.Fatalf("concurrent duplicate publishes=%d", len(second.packets))
	}
	close(publisher.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestPublishDueJoinsPublisherAndMetadataErrors(t *testing.T) {
	publishErr, updateErr := errors.New("publish"), errors.New("metadata")
	store := &failingUpdateStore{updateErr: updateErr}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	err = box.PublishDue(fixedErrorPublisher{err: publishErr}, time.Now(), nil, nil)
	if !errors.Is(err, publishErr) || !errors.Is(err, updateErr) {
		t.Fatalf("joined error = %v", err)
	}
}

func TestFileOutboxEnforcesRecordLimitOnPut(t *testing.T) {
	store, err := NewFileOutboxStoreWithOptions(t.TempDir(), FileOutboxOptions{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := store.Put(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Sequence = 2
	if err := store.Put(second); !errors.Is(err, ErrOutboxStoreLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

// 旧式 store 只能存包，无法保存 CreatedAt。不能把重启时间伪装成包创建时间。
type packetOnlyStore struct{ packet syncstream.Packet }

func (s *packetOnlyStore) Load() ([]syncstream.Packet, error) {
	return []syncstream.Packet{s.packet}, nil
}
func (*packetOnlyStore) Put(syncstream.Packet) error { return nil }
func (*packetOnlyStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}

func TestOutboxRefusesStoreWithoutPersistentAge(t *testing.T) {
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}
	if _, err := NewOutbox(OutboxOptions{Store: &packetOnlyStore{packet: packet}, RequireDurable: true}); err == nil {
		t.Error("packet-only store accepted: each restart resets packet age")
	}
	store := &failingUpdateStore{writes: 1, record: OutboxRecord{Packet: packet}}
	if _, err := NewOutbox(OutboxOptions{Store: store}); err == nil {
		t.Error("record without CreatedAt accepted: restart invents a new age")
	}
}

// PutRecord 先写 outbox-*.tmp 再原子替换，写入中途崩溃会留下临时文件：打开 store 时删掉崩溃遗留的、确认是本 outbox
// 生成的临时文件（os.CreateTemp 的 “outbox-<十进制数>.tmp” 普通文件），不动其他任何文件（名字不完全匹配的、目录、
// 符号链接、.packet），已有记录照常装载。
func TestFileOutboxOpenRemovesCrashLeftoverTemporaryFiles(t *testing.T) {
	directory := t.TempDir()
	first, err := NewFileOutboxStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := first.Put(packet); err != nil {
		t.Fatal(err)
	}

	// 用 PutRecord 同样的方式造出崩溃遗留：CreateTemp 生成的名字，写了一半、没有替换。
	var leftovers []string
	for range 2 {
		temporary, err := os.CreateTemp(directory, "outbox-*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := temporary.Write([]byte(`{"version":1,"rec`)); err != nil {
			t.Fatal(err)
		}
		if err := temporary.Close(); err != nil {
			t.Fatal(err)
		}
		leftovers = append(leftovers, temporary.Name())
	}
	keep := []string{"outbox-abc.tmp", "outbox-.tmp", "outbox-12.tmp.keep", "xoutbox-12.tmp", "outbox-12.TMP", "notes.tmp"}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "outbox-7.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, "outbox-7.tmp")
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "outbox-8.tmp")); err == nil {
		keep = append(keep, "outbox-8.tmp")
	}

	reopened, err := NewFileOutboxStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range leftovers {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("crash leftover %s survived reopening the outbox (stat err=%v)", filepath.Base(name), err)
		}
	}
	for _, name := range keep {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s is not an outbox temporary file and must be kept: %v", name, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("symlink target touched: %q %v", data, err)
	}
	records, err := reopened.LoadRecords()
	if err != nil || len(records) != 1 || records[0].Packet.Sequence != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
}
