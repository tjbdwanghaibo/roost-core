package skillsync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

// RR-20261006-04（O6）：PutRecord 先写 outbox-*.tmp 再原子替换，只在本次调用里删除自己的临时文件；
// 写入中途进程崩溃会留下临时文件，修前打开 store 时不扫描它们，每次崩溃泄漏一个且不计入上限。
// 承诺：打开 store 时删掉崩溃遗留的、确认是本 outbox 生成的临时文件（os.CreateTemp 的
// “outbox-<十进制数>.tmp” 普通文件），不动其他任何文件（名字不完全匹配的、目录、符号链接、.packet），
// 已有记录照常装载。
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
