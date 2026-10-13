// WAL 损坏处理：段缺口与未确认截断、帧头字段逐项、零填充尾只在安全条件下截断、跨页半写回拒绝。

package nestwal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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

// 断电 / 快照后最后一段尾部可能是零填充（文件长度已持久、数据块未写回，读出为 0）。只有同时满足四个条件才截断：(1) 仅最后一段；
// (2) 坏点不早于 checkpoint fence，不与已确认前缀重叠；(3) 自坏帧头起至文件末尾全为 0，而不是 CRC 不符的非零数据；(4) 截断后
// 告警并计指标（reason 低基数）。否则仍 ErrCorrupt，且拒绝打开时不改动文件。不完整尾帧的既有截断不变，但同样先核对 fence。
// 历史：RR-20260926-41。

const (
	tailTruncatedTotal = "nestwal.recovery.tail_truncated.total"
	tailTruncatedBytes = "nestwal.recovery.tail_truncated.bytes"
)

func tailCounter(name, reason string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && metric.Labels["reason"] == reason {
			return metric.Value
		}
	}
	return 0
}

func segmentPath(dir string, segment uint64) string {
	return filepath.Join(dir, segmentName(segment))
}

func appendRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func replayCount(t *testing.T, w *WAL) int {
	t.Helper()
	count := 0
	if err := w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	return count
}

// expectRefusedUntouched 断言打开被 ErrCorrupt 拒绝且段文件原样保留。
func expectRefusedUntouched(t *testing.T, opts Options, path string) error {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(opts)
	if err == nil {
		_ = w.Close(context.Background())
		t.Fatal("corrupt log opened; want ErrCorrupt")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("refused open modified the segment: size %d -> %d", len(before), len(after))
	}
	return err
}

// 最后一段、无 checkpoint、帧后零尾：5 / 19 / 20 / 4096 及跨多块的零尾都按撕裂截断，保留完整前缀并计指标。
func TestOpenTruncatesZeroFilledTailOfLastSegment(t *testing.T) {
	for _, n := range []int{5, 19, 20, 4096, 3*4096 + 7} {
		dir := t.TempDir()
		opts := testOptions(dir)
		segment := writeOneRecordAndClose(t, opts)
		path := segmentPath(dir, segment)
		want := fileSize(t, path)
		appendRaw(t, path, make([]byte, n))
		totalBefore, bytesBefore := tailCounter(tailTruncatedTotal, "zero_fill"), tailCounter(tailTruncatedBytes, "zero_fill")

		w, err := Open(opts)
		if err != nil {
			t.Errorf("zero tail %4d bytes: open err=%v", n, err)
			continue
		}
		if got := w.Stats().Offset; got != want || fileSize(t, path) != want || replayCount(t, w) != 1 {
			t.Errorf("zero tail %d bytes: offset=%d size=%d, want %d and 1 record", n, got, fileSize(t, path), want)
		}
		if dt, db := tailCounter(tailTruncatedTotal, "zero_fill")-totalBefore, tailCounter(tailTruncatedBytes, "zero_fill")-bytesBefore; dt != 1 || db != int64(n) {
			t.Errorf("zero tail %d bytes: metric delta total=%d bytes=%d, want 1 and %d", n, dt, db, n)
		}
		if _, err := w.Append(context.Background(), testRecord(2, corenest.DurabilityStrict)); err != nil {
			t.Errorf("append after truncation: %v", err)
		}
		_ = w.Close(context.Background())
	}
}

// 坏点恰在 checkpoint fence（fence 是最后一个已确认帧的结束偏移，坏帧从 fence 开始）：不与已确认前缀重叠，可截断。
func TestOpenTruncatesZeroFilledTailStartingAtCheckpointFence(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Ack(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, segmentPath(dir, fence.Segment), make([]byte, 4096))
	w, err = Open(opts)
	if err != nil {
		t.Fatalf("zero tail at the checkpoint fence: open err=%v", err)
	}
	defer w.Close(context.Background())
	if got := w.Stats().Offset; got != fence.Offset {
		t.Fatalf("recovered offset=%d, want fence %d", got, fence.Offset)
	}
}

// 非零垃圾、零尾之后又出现非零字节、中间零页后仍有完整帧：都不是“自坏帧头起全零”，拒绝且不改文件。
func TestOpenRefusesTailThatIsNotAllZero(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xab}, 4096)
	zerosThenByte := append(make([]byte, 4096), 0x01)
	for _, tc := range []struct {
		name string
		tail []byte
	}{{"non-zero garbage", garbage}, {"zeros then non-zero", zerosThenByte}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(dir)
			segment := writeOneRecordAndClose(t, opts)
			path := segmentPath(dir, segment)
			appendRaw(t, path, tc.tail)
			expectRefusedUntouched(t, opts, path)
		})
	}

	t.Run("zeroed middle frame, later frame intact", func(t *testing.T) {
		dir := t.TempDir()
		opts := testOptions(dir)
		w, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		first, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict))
		if err != nil {
			t.Fatal(err)
		}
		second, err := w.Append(context.Background(), testRecord(2, corenest.DurabilityAsync))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Append(context.Background(), testRecord(3, corenest.DurabilityAsync)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if first.Segment != second.Segment {
			t.Fatalf("premise: records span segments %d/%d", first.Segment, second.Segment)
		}
		patchSegment(t, dir, first.Segment, first.Offset, make([]byte, second.Offset-first.Offset))
		expectRefusedUntouched(t, opts, segmentPath(dir, first.Segment))
	})
}

// 坏点落在已确认前缀内（坏帧起点早于 checkpoint fence）：即使其后全零也拒绝，且不截断。
func TestOpenRefusesZeroFillInsideAcknowledgedPrefix(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Append(context.Background(), testRecord(2, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Ack(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := segmentPath(dir, first.Segment)
	patchSegment(t, dir, first.Segment, first.Offset, make([]byte, second.Offset-first.Offset))
	appendRaw(t, path, make([]byte, 4096))
	expectRefusedUntouched(t, opts, path)
}

// 撕裂尾帧越过 checkpoint fence：拒绝打开，且拒绝前不截断文件（旧实现先截断再报错）。
func TestOpenRefusesTornTailBeforeCheckpointWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityStrict)); err != nil {
		t.Fatal(err)
	}
	second, err := w.Append(context.Background(), testRecord(2, corenest.DurabilityStrict))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Ack(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := segmentPath(dir, second.Segment)
	if err := os.Truncate(path, second.Offset-5); err != nil {
		t.Fatal(err)
	}
	err = expectRefusedUntouched(t, opts, path)
	if !bytes.Contains([]byte(err.Error()), []byte("acknowledgement is beyond recovered WAL end")) {
		t.Fatalf("Open = %v, want acknowledgement beyond recovered end", err)
	}
}

// 非最后一段的零尾不属于撕裂：Open 只修复最后一段，回放读到该段时 ErrCorrupt，文件不被改动。
func TestReplayRefusesZeroFillInEarlierSegment(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := byte(1); w.Stats().Segment < 2; i++ {
		if _, err := w.Append(context.Background(), testRecord(i, corenest.DurabilityStrict)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := segmentPath(dir, 1)
	appendRaw(t, path, make([]byte, 4096))
	before := fileSize(t, path)
	w, err = Open(opts)
	if err != nil {
		t.Fatalf("open with zero-filled earlier segment: %v", err)
	}
	defer w.Close(context.Background())
	err = w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { return nil })
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay = %v, want ErrCorrupt", err)
	}
	if fileSize(t, path) != before {
		t.Fatalf("earlier segment modified: size %d -> %d", before, fileSize(t, path))
	}
}

// 跨页半写回仍拒绝启动：最后一帧跨 4 KiB 页边界，前页（含 20 字节帧头）已写回，后页未写回、读出为 0。帧头有效、帧体完整在文件
// 长度之内，只是 payload CRC 不符，自坏点至文件末尾并非全 0：既不是“零填充尾”也不是“不完整尾帧”，必须 ErrCorrupt 拒绝启动且
// 不改动文件——即使坏点在 checkpoint fence 之后也不放宽：WAL 无法区分“后页丢写回”与真实的 payload 损坏。
func TestOpenRefusesFrameWhosePayloadPageWasNotWrittenBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ackFirst bool
	}{
		{name: "no checkpoint", ackFirst: false},
		{name: "torn frame after checkpoint fence", ackFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(dir)
			opts.SegmentBytes = 64 << 10
			opts.MaxRecordBytes = 16 << 10
			w, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			first, err := w.Append(context.Background(), bulkyRecord(1, 3000))
			if err != nil {
				t.Fatal(err)
			}
			second, err := w.Append(context.Background(), bulkyRecord(2, 3000))
			if err != nil {
				t.Fatal(err)
			}
			if tc.ackFirst {
				if err := w.Ack(context.Background(), first); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			// 前提：第二帧起点（= 第一帧的结束偏移）连同整个帧头落在第一页，帧体跨过 4096 且是最后一帧。
			if first.Segment != second.Segment {
				t.Fatalf("premise: records span segments %d/%d", first.Segment, second.Segment)
			}
			path := segmentPath(dir, second.Segment)
			if first.Offset+frameHeaderSize > tailBlockSize || second.Offset <= tailBlockSize || fileSize(t, path) != second.Offset {
				t.Fatalf("premise: second frame [%d,%d) must keep its header in the first page and cross %d; file size %d",
					first.Offset, second.Offset, tailBlockSize, fileSize(t, path))
			}
			// 模拟后页未写回：自页边界起至文件末尾清零，文件长度不变。
			patchSegment(t, dir, second.Segment, tailBlockSize, make([]byte, second.Offset-tailBlockSize))

			err = expectRefusedUntouched(t, opts, path)
			if !strings.Contains(err.Error(), "invalid frame payload checksum") {
				t.Fatalf("Open = %v, want the payload checksum refusal", err)
			}
		})
	}
}

// bulkyRecord 是带 dataBytes 字节 mutation 数据的测试记录，用来让帧跨过页边界。
func bulkyRecord(sequence byte, dataBytes int) corenest.CommitRecord {
	record := testRecord(sequence, corenest.DurabilityStrict)
	record.Mutations[0].Data = bytes.Repeat([]byte{sequence}, dataBytes)
	return record
}
