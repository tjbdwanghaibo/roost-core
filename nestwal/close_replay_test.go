package nestwal

import (
	"context"
	"errors"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
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
