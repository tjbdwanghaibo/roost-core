package syncstream

import "testing"

type snapshotStore struct {
	s   HistorySnapshot
	err error
}

func (s snapshotStore) Load() (HistorySnapshot, error) { return s.s, s.err }
func (s snapshotStore) Save(HistorySnapshot) error     { return s.err }

// U-0213 · C8 · RR-20260915-08:绑定了 journal 的 History,Import / Restore 只替换内存不发布到 journal。
// 之后 Append 成功、重启:同 epoch 时第一包又变回 old(运行时明明已替换),换 epoch 时 WAL 混入不同
// epoch 而 ErrInvalidSnapshot。承诺:持久化实例的替换先经 journal.Checkpoint 发布,成功才切换内存;
// 失败保持旧状态。构造期加载走内部路径,不经 journal。
func TestHistoryPromiseImportOnAJournaledHistoryIsDurable(t *testing.T) {
	for _, mode := range []string{"import_same_epoch", "restore_same_epoch", "import_new_epoch", "restore_new_epoch", "checkpoint_same_epoch", "checkpoint_new_epoch"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			j, err := NewFileHistoryJournal(dir, 7)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			h, err := NewHistoryWithJournal(HistoryOptions{}, j)
			if err != nil {
				t.Fatal(err)
			}
			s := Stream{Topic: "state"}
			if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("old")}); err != nil {
				t.Fatal(err)
			}
			epoch := uint64(7)
			if mode == "import_new_epoch" || mode == "restore_new_epoch" || mode == "checkpoint_new_epoch" {
				epoch = 8
			}
			replacement := NewHistory(HistoryOptions{Epoch: epoch})
			replacement.Append(Packet{Stream: s, Full: true, Payload: []byte("replacement")})
			snapshot := replacement.Export()
			if mode == "restore_same_epoch" || mode == "restore_new_epoch" {
				err = h.Restore(snapshotStore{s: snapshot})
			} else {
				err = h.Import(snapshot)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "checkpoint_same_epoch" || mode == "checkpoint_new_epoch" {
				if err := h.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Append(Packet{Stream: s, Payload: []byte("next")}); err != nil {
				t.Fatal(err)
			}
			j.Close()
			j2, err := NewFileHistoryJournal(dir, 99)
			if err != nil {
				t.Fatal(err)
			}
			defer j2.Close()
			restored, err := NewHistoryWithJournal(HistoryOptions{}, j2)
			if err != nil {
				t.Fatalf("successful replacement and append cannot restart: %v", err)
			}
			snap := restored.Export()
			if string(snap.Streams[0].Packets[0].Payload) != "replacement" {
				t.Fatalf("replacement reverted on restart: %q", snap.Streams[0].Packets[0].Payload)
			}
			if restored.Epoch() != epoch {
				t.Fatalf("epoch after restart=%d want %d", restored.Epoch(), epoch)
			}
		})
	}
}
