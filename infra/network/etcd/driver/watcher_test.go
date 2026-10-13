package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
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
		t.Fatalf("WatchError=%v, want fetcd.ErrWatchCompacted", err)
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
		t.Fatalf("WatchError=%v, want fetcd.ErrWatchClosed", err)
	}
}

func TestServiceWatcherReportsUnexpectedChannelClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	watch := make(chan clientv3.WatchResponse)
	watcher := newServiceWatcher(ctx, watch, cancel)
	close(watch)
	select {
	case <-watcher.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher did not terminate")
	}
	if err := watcher.Err(); err == nil || !strings.Contains(err.Error(), "closed unexpectedly") {
		t.Fatalf("terminal error=%v", err)
	}
}

func TestServiceWatcherCloseIsNotReportedAsFailure(t *testing.T) {
	watcher := newServiceWatcher(context.Background(), nil, nil)
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Err(); err != nil {
		t.Fatalf("normal close reported error: %v", err)
	}
}
