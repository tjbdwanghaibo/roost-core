// WAL：回放 / 确认 / 重开、轮转、并发追加、撕裂尾恢复、目录锁、关闭排空、健康与容量、Sync 覆盖已准入票据、选项与记录准入。

package nestwal

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func testRecord(sequence byte, durability corenest.DurabilityPolicy) corenest.CommitRecord {
	var id corenest.TransactionID
	id[len(id)-1] = sequence
	return corenest.CommitRecord{
		ID:         id,
		Handler:    "test.handler",
		RequestID:  "request",
		CreatedAt:  123,
		Durability: durability.Record(),
		Mutations: []corenest.EntityMutation{{Key: dataengine.DocumentKey{ID: int64(sequence) + 1, Database: "game", Resource: "players"}, Kind: dataengine.MutationPut, ExpectedVersion: (uint64(sequence)) - 1, NextVersion: uint64(sequence), Mask: 3,
			Schema: 2,
			Codec:  "bson",
			Data:   []byte{sequence, 1, 2, 3},
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
	// Every append must be ADMITTED before Close, or the ones still on their
	// way legitimately get ErrClosed and the test measures scheduling, not
	// draining. Waiting for one queued append (the previous precondition) let
	// a slow Windows runner close the WAL under 31 goroutines that had not
	// reached Append yet ("append 0: nestwal: closed", kit v1.12.2 tag CI).
	deadline := time.Now().Add(5 * time.Second)
	for w.Stats().Admitted < total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if admitted := w.Stats().Admitted; admitted != total {
		t.Fatalf("only %d of %d appends were admitted before close; the precondition of this test did not hold", admitted, total)
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

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// Options that cannot produce a working log must be refused at Open, with the
// rule named — a WAL that opens on a segment smaller than its largest record
// would fail on the first big commit instead.
func TestOpenRefusesEachInvalidOption(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"blank directory", func(o *Options) { o.Dir = "  " }, "directory is required"},
		{"negative retain", func(o *Options) { o.RetainSegments = -1 }, "retain segments cannot be negative"},
		{"segment smaller than a record", func(o *Options) { o.SegmentBytes = 512; o.MaxRecordBytes = 600 }, "segment must be larger than maximum record"},
		{"segment too small for a frame header", func(o *Options) { o.SegmentBytes = frameHeaderSize; o.MaxRecordBytes = 1 }, "segment must be larger than maximum record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions(dir)
			tc.mutate(&opts)
			_, err := Open(opts)
			expectErr(t, err, tc.want)
		})
	}
}

// The codec refuses records the replay side could not interpret; a record
// that is too large for the configured segment is refused before it is
// queued, so the caller learns synchronously.
func TestAppendRefusesRecordsTheLogCannotHold(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ctx := context.Background()

	_, err = w.Append(ctx, corenest.CommitRecord{ID: corenest.TransactionID{1}})
	expectErr(t, err, "empty commit record")

	bad := testRecord(1, corenest.DurabilityPipelined+1)
	_, err = w.Append(ctx, bad)
	expectErr(t, err, "invalid durability policy")

	huge := testRecord(2, corenest.DurabilityStrict)
	huge.Mutations[0].Data = make([]byte, 2048)
	_, err = w.Append(ctx, huge)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized record = %v, want ErrRecordTooLarge", err)
	}

	many := testRecord(3, corenest.DurabilityStrict)
	many.Effects = make([]corenest.Effect, maxEntryCount+1)
	_, err = encodeRecord(many)
	expectErr(t, err, "too many entries in commit record")
}

// Acknowledgements move the replay checkpoint; a fence that names nothing or
// points past the log end must not be persisted — the next reopen would skip
// records that were never projected.
func TestAckRefusesFencesThatWouldSkipRecords(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ctx := context.Background()
	fence, err := w.Append(ctx, testRecord(1, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	expectErr(t, w.Ack(ctx, corenest.CommitFence{}), "invalid acknowledgement fence")
	expectErr(t, w.Ack(ctx, corenest.CommitFence{Segment: fence.Segment, Offset: 0}), "invalid acknowledgement fence")
	expectErr(t, w.Ack(ctx, corenest.CommitFence{Segment: fence.Segment + 5, Offset: 1}), "acknowledgement is beyond log end")
	expectErr(t, w.Ack(ctx, corenest.CommitFence{Segment: fence.Segment, Offset: fence.Offset + 100000}), "acknowledgement is beyond log end")
	if err := w.Ack(ctx, fence); err != nil {
		t.Fatalf("acking the appended fence: %v", err)
	}
}

// The WAL's own limits: the disk budget is enforced at admission; the oldest
// unacknowledged age is reported by Healthy(), because nothing else will.
func TestHealthyReportsDiskAndAgeLimits(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.MaxDiskBytes = 1
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	// The disk budget is enforced at admission, before the frame is queued:
	// the record is refused and the log stays healthy, rather than the log
	// growing past its limit and Healthy() reporting it afterwards.
	_, err = w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict))
	expectErr(t, err, "disk capacity limit reached")
	if err := w.Healthy(); err != nil {
		t.Fatalf("a refused append must not make the log unhealthy: %v", err)
	}

	opts2 := testOptions(t.TempDir())
	opts2.MaxUnackedAge = time.Nanosecond
	w2, err := Open(opts2)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close(context.Background())
	if _, err := w2.Append(context.Background(), testRecord(1, corenest.DurabilityStrict)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	expectErr(t, w2.Healthy(), "oldest unacknowledged record age")
}

// The checkpoint file is 36 bytes with a magic, a version and a CRC. Each of
// the three refusals is pinned: a truncated file, a foreign header, and a
// bit-flipped body must all be rejected rather than decoded into a fence.
func TestReadCheckpointRefusesTruncatedForeignAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	good := make([]byte, checkpointSize)
	binary.BigEndian.PutUint32(good[0:4], checkpointMagic)
	binary.BigEndian.PutUint16(good[4:6], checkpointVersion)
	binary.BigEndian.PutUint64(good[8:16], 3)
	binary.BigEndian.PutUint64(good[16:24], 2)
	binary.BigEndian.PutUint64(good[24:32], 64)
	binary.BigEndian.PutUint32(good[32:36], crc32.ChecksumIEEE(good[:32]))
	write := func(name string, raw []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := readCheckpoint(write("good", good)); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}
	if _, err := readCheckpoint(write("short", good[:20])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated checkpoint = %v", err)
	}
	foreign := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(foreign[0:4], 0xDEADBEEF)
	_, err := readCheckpoint(write("foreign", foreign))
	expectErr(t, err, "invalid checkpoint header")
	flipped := append([]byte(nil), good...)
	flipped[20] ^= 0x01
	_, err = readCheckpoint(write("flipped", flipped))
	expectErr(t, err, "invalid checkpoint checksum")
}

func TestAckSyncsAsyncRecordBeforeCheckpoint(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.GroupCommitInterval = time.Hour
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	fence, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityAsync))
	if err != nil {
		t.Fatal(err)
	}
	if w.Stats().Syncs != 0 {
		t.Fatal("async append unexpectedly synced")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = w.Ack(canceled, fence); err != nil {
		t.Fatal(err)
	}
	if w.Stats().Syncs != 1 {
		t.Fatal("checkpoint persisted before WAL data sync")
	}
}

func TestCloseRetainsDirectoryUntilExternalReplayReturns(t *testing.T) {
	opts := testOptions(t.TempDir())
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = w.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close=%v", err)
	}
	if other, err := Open(opts); err == nil {
		other.Close(context.Background())
		t.Fatal("directory reused while replay active")
	}
	if err = w.Ack(context.Background(), fence); !errors.Is(err, ErrClosed) {
		t.Fatalf("late ack=%v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err = w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("late replay=%v", err)
	}
}

func TestWALSyncPromiseCoversAdmittedTickets(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.BatchDelay = time.Hour
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	ticket, err := w.Enqueue(context.Background(), testRecord(1, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := w.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatalf("ticket failed: %v", err)
		}
	default:
		t.Fatalf("Sync returned nil before admitted ticket resolved: admitted=%d appended=%d durable_lsn=%d ticket_lsn=%d",
			w.Stats().Admitted, w.Stats().Appended, w.DurableLSN(), ticket.LSN())
	}
	if got := w.DurableLSN(); got < ticket.LSN() {
		t.Fatalf("durable watermark=%d < ticket lsn=%d after Sync", got, ticket.LSN())
	}
}

// 屏障不能让 Sync 在 WAL 已关闭后无限阻塞：Close 已经排空并 fsync 了全部队列，
// 所以关闭后的 Sync 是空满足——必须立刻返回 nil，而不是卡在没人接收的 appendCh 上。
func TestWALSyncPromiseDoesNotBlockAfterClose(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Sync(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync after clean Close: got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sync blocked after Close")
	}
}

func TestWALSyncPromiseWaitsForDrainWhileClosing(t *testing.T) {
	for _, closing := range []bool{false, true} {
		name := "open_control"
		if closing {
			name = "closing"
		}
		t.Run(name, func(t *testing.T) {
			entered, resume := make(chan struct{}), make(chan struct{})
			first := true
			opts := testOptions(t.TempDir())
			opts.BatchMaxRecords = 1
			opts.BatchDelay = time.Hour
			opts.beforeProcessBatch = func() {
				if first {
					first = false
					close(entered)
					<-resume
				}
			}
			w, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			closed := make(chan error, 1)
			defer func() {
				close(resume)
				if closing {
					<-closed
				} else {
					_ = w.Close(context.Background())
				}
			}()
			ticket, err := w.Enqueue(context.Background(), testRecord(91, corenest.DurabilityPipelined))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("writer not captured")
			}
			if closing {
				go func() { closed <- w.Close(context.Background()) }()
				select {
				case <-w.closeCh:
				case <-time.After(time.Second):
					t.Fatal("Close not begun")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err = w.Sync(ctx)
			select {
			case <-ticket.Done():
				t.Fatal("premise violated: ticket done while writer held")
			default:
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Sync while writer held: closing=%v err=%v admitted=%d appended=%d durable=%d ticket=%d",
					closing, err, w.Stats().Admitted, w.Stats().Appended, w.DurableLSN(), ticket.LSN())
			}
		})
	}
}

// 关闭完成后 Sync 仍是幂等的 nil,且 Close 已让 ticket 完成。
func TestWALSyncPromiseAfterCompletedClose(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := w.Enqueue(context.Background(), testRecord(92, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("completed close left ticket pending")
	}
}
