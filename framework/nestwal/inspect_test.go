package nestwal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func TestInspectAcceptsDurableCheckpointBoundary(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
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
	report, err := Inspect(context.Background(), InspectOptions{Dir: dir})
	if err != nil || !report.Complete || len(report.Checkpoints) == 0 {
		t.Fatalf("valid checkpoint: %+v, %v", report, err)
	}
}

func TestInspectRefusesRunningWriterWithoutCreatingSnapshot(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	target := filepath.Join(t.TempDir(), "snapshot")
	if _, err := Inspect(context.Background(), InspectOptions{Dir: dir, SnapshotDir: target}); !errors.Is(err, ErrLocked) {
		t.Fatalf("active WAL inspection error = %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active writer inspection created destination: %v", err)
	}
}

func TestInspectPreservesCorruptRecordAndSuffix(t *testing.T) {
	for _, damage := range []string{"torn", "crc", "codec"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			writeOneRecordAndClose(t, testOptions(dir))
			path := filepath.Join(dir, segmentName(1))
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bad := []byte{1, 2, 3}
			if damage == "crc" {
				bad = bytes.Clone(first)
				bad[len(bad)-1] ^= 1
			}
			if damage == "codec" {
				// frame CRC合法，内部record版本非法，必须走真实decoder而不是只验CRC。
				bad = encodeFrame([]byte{255, 255, 0, 0})
			}
			original := append(bytes.Clone(first), bad...)
			original = append(original, first...) // 坏记录后还存在合法后缀，禁止跳过或丢弃。
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "isolation")
			report, err := Inspect(context.Background(), InspectOptions{Dir: dir, SnapshotDir: target})
			if !errors.Is(err, ErrCorrupt) || !report.Complete || len(report.Segments) != 1 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			entry := report.Segments[0]
			if entry.Records != 1 || entry.LastGoodOffset != int64(len(first)) || entry.Error == "" {
				t.Fatalf("inspector skipped bad record or lost its location: %+v", entry)
			}
			for _, name := range []string{path, filepath.Join(target, "wal", segmentName(1))} {
				raw, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(raw, original) {
					t.Fatalf("source/snapshot changed bytes: %s err=%v", name, err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(target, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var files []snapshotFile
			if err := json.Unmarshal(raw, &files); err != nil {
				t.Fatal(err)
			}
			matched := false
			for _, file := range files {
				if file.Path == segmentName(1) {
					digest := sha256.Sum256(original)
					matched = file.SHA256 == hex.EncodeToString(digest[:]) && file.Bytes == int64(len(original))
				}
			}
			if !matched {
				t.Fatal("snapshot manifest does not describe the complete damaged source")
			}
			if _, err := os.Stat(filepath.Join(target, "INCOMPLETE")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed diagnostic snapshot left incomplete marker: %v", err)
			}
			if _, err := Inspect(context.Background(), InspectOptions{Dir: dir, SnapshotDir: target}); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing snapshot was not refused: %v", err)
			}
		})
	}
}

func TestInspectValidWALCancellationAndUnsafeDestinations(t *testing.T) {
	dir := t.TempDir()
	writeOneRecordAndClose(t, testOptions(dir))
	report, err := Inspect(context.Background(), InspectOptions{Dir: dir})
	if err != nil || !report.Complete || len(report.Segments) != 1 || report.Segments[0].Records != 1 {
		t.Fatalf("valid WAL report=%+v err=%v", report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, InspectOptions{Dir: dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inspection = %v", err)
	}
	if _, err := Inspect(context.Background(), InspectOptions{Dir: dir, SnapshotDir: filepath.Join(dir, "copy")}); err == nil {
		t.Fatal("snapshot was allowed inside its own source")
	}
	if err := os.Symlink(filepath.Join(dir, segmentName(1)), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), InspectOptions{Dir: dir}); err == nil {
		t.Fatal("inspection followed a symlink")
	}
}

func TestInspectNeverRepairsTornTailOrCheckpoint(t *testing.T) {
	dir := t.TempDir()
	writeOneRecordAndClose(t, testOptions(dir))
	path := filepath.Join(dir, segmentName(1))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	end := int64(len(original))
	original = append(original, 1, 2, 3)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointName(0)), []byte("broken checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(context.Background(), InspectOptions{Dir: dir})
	if !errors.Is(err, ErrCorrupt) || !report.Complete || report.Segments[0].LastGoodOffset != end || len(report.Problems) == 0 {
		t.Fatalf("torn tail/checkpoint were hidden: report=%+v err=%v", report, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("inspection repaired source tail: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, checkpointName(0)))
	if err != nil || string(got) != "broken checkpoint" {
		t.Fatalf("inspection rewrote checkpoint: %v", err)
	}
}
