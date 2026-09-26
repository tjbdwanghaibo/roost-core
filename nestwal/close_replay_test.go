package nestwal

import (
	"context"
	"errors"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"testing"
	"time"
)

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

func TestOpenRuntimeZeroOptionsOwnsWAL(t *testing.T) {
	opts := testOptions(t.TempDir())
	r, err := OpenRuntime(opts, MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error { return nil }), nil, CommitterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = r.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeat shutdown: %v", err)
	}
	other, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
}

// RR-20260926-17（复核后重写为负对照）：Committer.Close 必须等外部 Flush 退出，
// 才能关闭 WAL、交出目录。否则 Flush 在 WAL 关闭后才 ack，checkpoint 丢失，新拥有者
// 重开目录会重放旧实例已应用的记录。旧版本本测试用已取消 ctx 调 Close，修复前后
// 都返回 Canceled，不是负对照；这里用活 ctx，并在重开 WAL 后检查 checkpoint。
func TestCommitterCloseWaitsForExternalFlush(t *testing.T) {
	opts := testOptions(t.TempDir())
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	c, err := NewCommitter(w, MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error {
		close(entered)
		<-release
		return nil
	}), nil, DefaultCommitterOptions())
	if err != nil {
		t.Fatal(err)
	}
	c.cancel()
	<-c.done // 无后台工作；后面的回放只属于外部 Flush。
	record := testRecord(1, corenest.DurabilityStrict)
	record.Effects = nil
	if _, err = w.Append(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- c.Close(context.Background()) }()
	// 负向观察需要有界等待：修复后 Close 在 release 之前不可能返回，这段等待只影响
	// 缺陷版本被发现的概率，不影响修复版本的结果。
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while the external Flush was still applying", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err = c.replayPass(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("new replay after Close started=%v, want ErrClosed", err)
	}
	close(release)
	// Close 已停止准入，Flush 的下一轮回放被拒（ErrClosed）；已应用那一轮的 ack 必须已经落盘。
	if err = <-flushed; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("external Flush=%v", err)
	}
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err = w.Append(context.Background(), record); !errors.Is(err, ErrClosed) {
		t.Fatalf("WAL not closed: %v", err)
	}
	reopened, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	replayed := 0
	if err = reopened.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { replayed++; return nil }); err != nil {
		t.Fatal(err)
	}
	if replayed != 0 {
		t.Fatalf("new owner replays %d record(s) the closed committer already applied: checkpoint was lost", replayed)
	}
}
