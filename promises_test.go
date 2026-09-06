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
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

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
		{"unknown writer version", func(o *Options) { o.WriterVersion = 9 }, "unsupported writer version 9"},
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
	_, err = encodeRecordVersion(many, WriterVersionV2)
	expectErr(t, err, "too many entries in commit record")
	_, err = encodeRecordVersion(testRecord(4, corenest.DurabilityStrict), 7)
	expectErr(t, err, "invalid writer version 7")
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
