package syncstream

import (
	"errors"
	"os"
	"testing"
)

// U-0212 · C5 · RR-20260916-01:写入 / 发布的结果不确定之后,journal 不能继续用落后的内存状态写。
// 旧实现 flushBatch 的 Write/Sync 出错只是返回错误,History 回滚内存(latest 仍是 1),调用方重试
// Append 又成功写了一条 sequence 2——WAL 里两条 sequence 2,重启 ErrInvalidSnapshot;Checkpoint 的
// 发布出错也不置状态,新 generation 可能已经落盘,后续 Append 却继续写旧 WAL,重启后成功的追加消失。
// 承诺:Write / Sync / 发布这三步出错即进入 fail-stop(ErrHistoryJournalFailed),拒绝后续 Record /
// Checkpoint,由重开 journal 从真正落盘的内容恢复;副作用之前的错误(临时文件、下一代 WAL 创建)
// 仍可重试。注入方式:让 syncFile / publish 先真做、再报错——这是"副作用已完成但调用方收到错误"
// 唯一能稳定构造的形态,不是硬件故障实验。

func failStopHistory(t *testing.T, dir string) (*FileHistoryJournal, *History) {
	t.Helper()
	j, err := NewFileHistoryJournal(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatal(err)
	}
	return j, h
}

func reopenHistory(t *testing.T, dir string) *History {
	t.Helper()
	j, err := NewFileHistoryJournal(dir, 99)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatalf("restart after an indeterminate outcome must recover: %v", err)
	}
	return h
}

func TestJournalPromiseIndeterminateSyncStopsTheJournal(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	// 第二条:真 fsync 成功,回复却丢了。
	injected := errors.New("fsync reply lost")
	armed := true
	j.syncFile = func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		if armed {
			armed = false
			return injected
		}
		return nil
	}
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("indeterminate sync must fail-stop: %v", err)
	}
	// 直接重试不能再成功:否则 WAL 里出现两条 sequence 2。
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two-retry")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("retry after an indeterminate outcome must be refused: %v", err)
	}
	if err := h.Checkpoint(); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("checkpoint on a failed journal must be refused: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// 重开:落盘的是两条(1、2),各出现一次。
	restored := reopenHistory(t, dir)
	if got := restored.Status(Observer{}, s).LatestSequence; got != 2 {
		t.Fatalf("recovered latest=%d want 2 (the synced record is durable exactly once)", got)
	}
}

func TestJournalPromiseIndeterminatePublishStopsTheJournal(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	// 发布真的完成了(rename 已落),函数却报错。
	j.publish = func(from, to, directory string) error {
		if err := durableReplace(from, to, directory); err != nil {
			return err
		}
		return errors.New("directory sync reply lost")
	}
	if err := h.Checkpoint(); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("indeterminate publish must fail-stop: %v", err)
	}
	// 旧实现这里会成功,却写进旧 generation 的 WAL,重启后消失。
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("append after an indeterminate publish must be refused: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	restored := reopenHistory(t, dir)
	if got := restored.Status(Observer{}, s).LatestSequence; got != 1 {
		t.Fatalf("recovered latest=%d want 1", got)
	}
}

// 对照:副作用之前的失败(临时文件建不出来)不是不确定结果,修复后可以重试。
func TestJournalPromisePreSideEffectFailureIsRetryable(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	defer j.Close()
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := h.Checkpoint()
	_ = os.Chmod(dir, 0o700)
	if err == nil {
		t.Skip("checkpoint succeeded in a read-only directory (running as root?)")
	}
	if errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("a failure before any side effect must not fail-stop: %v", err)
	}
	if err := h.Checkpoint(); err != nil {
		t.Fatalf("retry after a pre-side-effect failure: %v", err)
	}
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); err != nil {
		t.Fatal(err)
	}
}
