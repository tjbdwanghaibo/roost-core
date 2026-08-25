package nestwal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

func testRecord(sequence byte, durability corenest.DurabilityPolicy) corenest.CommitRecord {
	var id corenest.TransactionID
	id[len(id)-1] = sequence
	return corenest.CommitRecord{
		ID:         id,
		Handler:    "test.handler",
		RequestID:  "request",
		CreatedAt:  123,
		Durability: durability,
		Mutations: []corenest.EntityMutation{{
			EntityID: int64(sequence) + 1,
			Database: "game",
			Resource: "players",
			Version:  uint64(sequence),
			Mask:     3,
			Schema:   2,
			Codec:    "bson",
			Data:     []byte{sequence, 1, 2, 3},
		}},
		Effects: []corenest.Effect{{
			ID:      id.String() + ":1",
			Topic:   "player.changed",
			Key:     "player",
			Payload: []byte{4, 5, sequence},
			Headers: map[string]string{"trace": "abc", "kind": "test"},
		}},
	}
}

func testOptions(dir string) Options {
	opts := DefaultOptions(dir)
	opts.SegmentBytes = 1024
	opts.MaxRecordBytes = 768
	opts.BatchDelay = 100 * time.Microsecond
	opts.GroupCommitInterval = time.Millisecond
	opts.RetainSegments = 8
	return opts
}

func TestCodecRoundTrip(t *testing.T) {
	want := testRecord(7, corenest.DurabilityStrict)
	raw, err := encodeRecord(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Handler != want.Handler || got.RequestID != want.RequestID || got.Durability != want.Durability {
		t.Fatalf("record header mismatch: got=%+v want=%+v", got, want)
	}
	if len(got.Mutations) != 1 || string(got.Mutations[0].Data) != string(want.Mutations[0].Data) {
		t.Fatalf("mutation mismatch: %+v", got.Mutations)
	}
	if len(got.Effects) != 1 || got.Effects[0].Headers["trace"] != "abc" {
		t.Fatalf("effect mismatch: %+v", got.Effects)
	}
}

func TestWALReplayAckAndReopen(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := byte(1); i <= 3; i++ {
		if _, err := w.Append(context.Background(), testRecord(i, corenest.DurabilityStrict)); err != nil {
			t.Fatal(err)
		}
	}
	var replayed []byte
	if err := w.Replay(context.Background(), func(fence corenest.CommitFence, record corenest.CommitRecord) error {
		replayed = append(replayed, record.ID[15])
		return w.Ack(context.Background(), fence)
	}); err != nil {
		t.Fatal(err)
	}
	if string(replayed) != string([]byte{1, 2, 3}) {
		t.Fatalf("replay order=%v", replayed)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	w, err = Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	count := 0
	if err := w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("replayed acknowledged records: %d", count)
	}
}

func TestWALReplayStartsAtAckFenceAcrossRotation(t *testing.T) {
	// Regression: Replay used to scan every retained segment from offset 0,
	// re-checksumming acknowledged data on each pass — Stats/Healthy paid
	// that cost on every probe. Replay now starts at the acknowledgement
	// fence, which is always a frame boundary, and must still return exactly
	// the unacknowledged suffix after segment rotation.
	opts := testOptions(t.TempDir())
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	const total = 8
	fences := make([]corenest.CommitFence, 0, total)
	for i := byte(1); i <= total; i++ {
		fence, err := w.Append(context.Background(), testRecord(i, corenest.DurabilityStrict))
		if err != nil {
			t.Fatal(err)
		}
		fences = append(fences, fence)
	}
	if w.Stats().Segment < 2 {
		t.Fatalf("test needs a rotation to cover the cross-segment fence path: segment=%d", w.Stats().Segment)
	}
	// Acknowledge up to a record beyond the first segment boundary.
	ackUpTo := 5
	if err := w.Ack(context.Background(), fences[ackUpTo-1]); err != nil {
		t.Fatal(err)
	}

	var replayed []byte
	if err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		replayed = append(replayed, record.ID[15])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(replayed) != string([]byte{6, 7, 8}) {
		t.Fatalf("replay=%v, want exactly the unacknowledged suffix", replayed)
	}

	// The oldest-unacked cache must not survive an acknowledgement: age
	// drops as soon as the tail is fully acked.
	if age := w.Stats().OldestUnackedAge; age <= 0 {
		t.Fatalf("unacked records must report a positive age, got %v", age)
	}
	if err := w.Ack(context.Background(), fences[total-1]); err != nil {
		t.Fatal(err)
	}
	if age := w.Stats().OldestUnackedAge; age != 0 {
		t.Fatalf("fully acknowledged log must report zero age, got %v", age)
	}
}

func TestWALConcurrentAppendAndRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	const total = 64
	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := w.Append(context.Background(), testRecord(byte(i+1), corenest.DurabilityStrict))
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	seen := make(map[corenest.TransactionID]struct{}, total)
	if err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		count++
		seen[record.ID] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != total || len(seen) != total {
		t.Fatalf("count=%d unique=%d want=%d", count, len(seen), total)
	}
	if w.Stats().Segment < 2 {
		t.Fatal("expected segment rotation")
	}
}

func TestWALRecoversTornTail(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict)); err != nil {
		t.Fatal(err)
	}
	segment := w.Stats().Segment
	wantSize := w.Stats().Offset
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, segmentName(segment))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	w, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	if w.Stats().Offset != wantSize {
		t.Fatalf("recovered offset=%d want=%d", w.Stats().Offset, wantSize)
	}
}

func TestWALRejectsChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict)); err != nil {
		t.Fatal(err)
	}
	segment := w.Stats().Segment
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, segmentName(segment))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, frameHeaderSize+10); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	_, err = Open(opts)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err=%v, want %v", err, ErrCorrupt)
	}
}

func TestWALDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	second, err := Open(testOptions(dir))
	if second != nil {
		_ = second.Close(context.Background())
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err=%v, want %v", err, ErrLocked)
	}
}

func TestWALCloseDrainsAdmittedAppends(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.BatchDelay = time.Second
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	const total = 32
	start := make(chan struct{})
	results := make(chan error, total)
	for i := 0; i < total; i++ {
		go func(i int) {
			<-start
			_, err := w.Append(context.Background(), testRecord(byte(i+1), corenest.DurabilityAsync))
			results <- err
		}(i)
	}
	close(start)
	deadline := time.Now().Add(time.Second)
	for w.Stats().Queued == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < total; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		case <-ctx.Done():
			t.Fatal("admitted append did not complete during close")
		}
	}
	if w.Stats().Appended != total {
		t.Fatalf("appended=%d want=%d", w.Stats().Appended, total)
	}
}

func TestCommitterRetriesAndFlushesOutbox(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var applyCalls atomic.Int32
	var publishCalls atomic.Int32
	applier := MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error {
		applyCalls.Add(1)
		return nil
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error {
		if publishCalls.Add(1) == 1 {
			return errors.New("temporary publish failure")
		}
		return nil
	})
	opts := DefaultCommitterOptions()
	opts.RetryMin = time.Millisecond
	opts.RetryMax = 5 * time.Millisecond
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())
	record := testRecord(9, corenest.DurabilityStrict)
	if err := committer.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	committer.TransactionReleased(record.ID)
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		err = committer.Flush(deadline)
		if err == nil {
			break
		}
		if deadline.Err() != nil {
			t.Fatalf("flush: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if applyCalls.Load() < 2 || publishCalls.Load() < 2 {
		t.Fatalf("apply=%d publish=%d, expected at-least-once retry", applyCalls.Load(), publishCalls.Load())
	}
	count := 0
	if err := w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unacknowledged records=%d", count)
	}
}

func TestCommitterWaitsForEntityRelease(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	applier := MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error {
		calls.Add(1)
		return nil
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error { return nil })
	opts := DefaultCommitterOptions()
	opts.RetryMin = time.Millisecond
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())
	record := testRecord(10, corenest.DurabilityStrict)
	if err := committer.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("mutation applied %d times while entity was still locked", calls.Load())
	}
	committer.TransactionReleased(record.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := committer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("mutation apply calls=%d, want 1", calls.Load())
	}
}

func TestWALHealthReportsDiskAndOldestUnackedLimits(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.MaxDiskBytes = 1 << 20
	opts.MaxUnackedAge = time.Millisecond
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	record := testRecord(44, corenest.DurabilityStrict)
	record.CreatedAt = time.Now().Add(-time.Second).UnixNano()
	if _, err := w.Append(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	stats := w.Stats()
	if stats.DiskBytes <= 1 || stats.SegmentFiles == 0 || stats.OldestUnackedAge < time.Second {
		t.Fatalf("wal stats = %+v", stats)
	}
	w.opts.MaxDiskBytes = 1
	if err := w.Healthy(); err == nil {
		t.Fatal("WAL health ignored production capacity limits")
	}
}

func TestWALRejectsAppendBeforeDiskLimitIsExceeded(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.MaxDiskBytes = 1
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	record := testRecord(45, corenest.DurabilityStrict)
	record.CreatedAt = time.Now().UnixNano()
	if _, err := w.Append(context.Background(), record); !errors.Is(err, ErrCapacity) {
		t.Fatalf("append error = %v, want ErrCapacity", err)
	}
	if w.Stats().Appended != 0 {
		t.Fatal("capacity-rejected append was counted as durable")
	}
}
