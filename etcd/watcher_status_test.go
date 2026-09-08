package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

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
