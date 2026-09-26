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

func TestCommitterCloseWaitsForExternalFlush(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
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
	done := make(chan error, 1)
	go func() { done <- c.Flush(context.Background()) }()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = c.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("close=%v", err)
	}
	if err = w.Sync(context.Background()); err != nil {
		t.Fatalf("WAL closed before flush returned: %v", err)
	}
	close(release)
	err = <-done
	if err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err = c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Append(context.Background(), record); !errors.Is(err, ErrClosed) {
		t.Fatalf("WAL not closed: %v", err)
	}
}
