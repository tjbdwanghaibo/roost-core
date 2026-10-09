package nestwal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 旧格式必须明确拒绝，保留原始日志，不消费也不推进确认点。
func TestRetiredFormatsLeaveWALUntouched(t *testing.T) {
	for _, version := range []byte{5, 6} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			dir := t.TempDir()
			payload, err := encodeRecord(canonicalRecord(dataengine.MutationPut))
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint16(payload, uint16(version))
			raw := encodeFrame(payload)
			path := filepath.Join(dir, segmentName(1))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			w, err := Open(testOptions(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(context.Background())
			called := false
			err = w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { called = true; return nil })
			if !errors.Is(err, ErrUnsupportedRecordVersion) {
				t.Fatalf("codec %d: want unsupported version, got %v", version, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if called || w.checkpoint.fence != (corenest.CommitFence{}) || !bytes.Equal(raw, after) {
				t.Fatal("old WAL was consumed or modified")
			}
		})
	}
}
