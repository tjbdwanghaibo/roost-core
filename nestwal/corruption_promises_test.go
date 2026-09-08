package nestwal

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// writeOneRecordAndClose opens a WAL, appends one record and closes it,
// returning the active segment number so the test can corrupt the file.
func writeOneRecordAndClose(t *testing.T, opts Options) uint64 {
	t.Helper()
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
	return segment
}

func patchSegment(t *testing.T, dir string, segment uint64, offset int64, raw []byte) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, segmentName(segment)), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt(raw, offset); err != nil {
		t.Fatal(err)
	}
}

// resealHeader recomputes the first frame's header CRC after a deliberate
// field change, so the test exercises the rule for that field rather than the
// checksum rule.
func resealHeader(t *testing.T, dir string, segment uint64) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, segmentName(segment)), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	header := make([]byte, frameHeaderSize)
	if _, err := file.ReadAt(header, 0); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(header[:16]))
	if _, err := file.WriteAt(header[16:20], 16); err != nil {
		t.Fatal(err)
	}
}

func expectCorrupt(t *testing.T, opts Options, text string) {
	t.Helper()
	w, err := Open(opts)
	if err == nil {
		_ = w.Close(context.Background())
		t.Fatalf("corrupt log opened; want ErrCorrupt containing %q", text)
	}
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), text) {
		t.Fatalf("Open = %v, want ErrCorrupt containing %q", err, text)
	}
}

// The directory layout is part of the durability contract: a missing segment
// in the middle, or a log whose first segment is not 1 with nothing ever
// acknowledged, means records are gone. Open must refuse rather than replay a
// hole as "nothing happened".
func TestOpenRefusesSegmentGapsAndUnacknowledgedTruncation(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	segment := writeOneRecordAndClose(t, opts)
	if err := os.WriteFile(filepath.Join(dir, segmentName(segment+2)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expectCorrupt(t, opts, "non-contiguous WAL segments")
	if err := os.Remove(filepath.Join(dir, segmentName(segment+2))); err != nil {
		t.Fatal(err)
	}

	dir2 := t.TempDir()
	opts2 := testOptions(dir2)
	segment2 := writeOneRecordAndClose(t, opts2)
	if err := os.Rename(filepath.Join(dir2, segmentName(segment2)), filepath.Join(dir2, segmentName(segment2+1))); err != nil {
		t.Fatal(err)
	}
	expectCorrupt(t, opts2, "WAL starts after segment 1 without an acknowledgement")
}

// Every field of the frame header is checked on replay; only the payload
// checksum had a test. A flipped magic, a flipped header CRC and an absurd
// size must each be a distinct, named refusal.
func TestOpenRefusesEachCorruptFrameHeaderField(t *testing.T) {
	cases := []struct {
		name  string
		patch func(t *testing.T, dir string, segment uint64)
		want  string
	}{
		// The header CRC covers the magic and the size, so a plain flip is
		// caught by the checksum rule first; reseal the CRC so only the rule
		// for the changed field can fire.
		{"foreign magic", func(t *testing.T, dir string, segment uint64) {
			patchSegment(t, dir, segment, 0, []byte{0xde, 0xad, 0xbe, 0xef})
			resealHeader(t, dir, segment)
		}, "invalid frame header"},
		{"flipped header checksum", func(t *testing.T, dir string, segment uint64) {
			patchSegment(t, dir, segment, 16, []byte{0x00, 0x00, 0x00, 0x00})
		}, "invalid frame header checksum"},
		{"size beyond the record limit", func(t *testing.T, dir string, segment uint64) {
			size := make([]byte, 4)
			binary.BigEndian.PutUint32(size, 1<<30)
			patchSegment(t, dir, segment, 8, size)
			resealHeader(t, dir, segment)
		}, ErrRecordTooLarge.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(dir)
			segment := writeOneRecordAndClose(t, opts)
			tc.patch(t, dir, segment)
			expectCorrupt(t, opts, tc.want)
		})
	}
}
