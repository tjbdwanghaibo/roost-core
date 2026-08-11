package etcd

import (
	"context"
	"errors"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestWatcherReadyWaitsForServerResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := make(chan clientv3.WatchResponse, 1)
	w := newWatcher(ctx, raw, cancel)
	select {
	case <-w.Ready():
		t.Fatal("watcher reported ready before a server response")
	default:
	}
	raw <- clientv3.WatchResponse{Created: true}
	select {
	case <-w.Ready():
	case <-time.After(time.Second):
		t.Fatal("watcher did not report the server creation response")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherReadyReportsCompactedStartRevision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := make(chan clientv3.WatchResponse, 1)
	w := newWatcher(ctx, raw, cancel)
	raw <- clientv3.WatchResponse{Canceled: true, CompactRevision: 12}
	select {
	case <-w.Ready():
	case <-time.After(time.Second):
		t.Fatal("failed watch did not unblock readiness waiters")
	}
	select {
	case _, ok := <-w.EventChan():
		if ok {
			t.Fatal("watch event channel remained open after compaction")
		}
	case <-time.After(time.Second):
		t.Fatal("watch event channel did not close after compaction")
	}
	if err := w.WatchError(); !errors.Is(err, fetcd.ErrWatchCompacted) {
		t.Fatalf("WatchError=%v, want ErrWatchCompacted", err)
	}
}

func TestWatcherReadyReportsUnexpectedChannelClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := make(chan clientv3.WatchResponse)
	w := newWatcher(ctx, raw, cancel)
	close(raw)
	select {
	case <-w.Ready():
	case <-time.After(time.Second):
		t.Fatal("closed watch did not unblock readiness waiters")
	}
	if err := w.WatchError(); !errors.Is(err, fetcd.ErrWatchClosed) {
		t.Fatalf("WatchError=%v, want ErrWatchClosed", err)
	}
}
