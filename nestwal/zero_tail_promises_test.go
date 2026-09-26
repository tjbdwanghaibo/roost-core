package nestwal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// RR-20260926-41：断电 / 快照后最后一段尾部可能是零填充（ext4 data=writeback、部分网络盘与云盘：
// 文件长度已持久、数据块未写回，读出为 0）。
//
// 承诺：只有同时满足四个条件才截断——(1) 仅最后一段；(2) 坏点（第一个无法解析的帧起点）不早于
// checkpoint fence，即不与已确认前缀重叠；(3) 自坏帧头起至文件末尾全为 0（坏帧头所在 4 KiB 块的剩余部分
// 及其后所有块全零），而不是 CRC 不符的非零数据；(4) 截断后告警并计指标（reason 低基数）。否则仍 ErrCorrupt，
// 且拒绝打开时不改动文件。不完整尾帧（帧头不足 20 字节 / 帧体越过文件末尾）的既有截断不变，但同样先核对 fence。
//
// 旧行为（v1.17.0）：只按长度判撕裂，零尾 5 / 19 字节打开成功，20 / 4096 字节报 invalid frame header 拒绝启动；
// 尾帧越过 fence 时先截断文件再报 “acknowledgement is beyond recovered WAL end”。

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
