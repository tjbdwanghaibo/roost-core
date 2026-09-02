package etcd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type callbackTestWatcher struct {
	events chan *WatchEvent
	once   sync.Once
	err    error
}

func newCallbackTestWatcher() *callbackTestWatcher {
	return &callbackTestWatcher{events: make(chan *WatchEvent, 8)}
}

func (w *callbackTestWatcher) EventChan() <-chan *WatchEvent { return w.events }
func (w *callbackTestWatcher) WatchError() error             { return w.err }
func (w *callbackTestWatcher) Close() error {
	w.once.Do(func() { close(w.events) })
	return nil
}

func TestWatchCallbackDeliversInOrderAndReportsHandlerError(t *testing.T) {
	watcher := newCallbackTestWatcher()
	handlerErr := errors.New("handler failed")
	var revisions []int64
	subscription, err := WatchCallback(context.Background(), watcher, func(_ context.Context, event *WatchEvent) error {
		revisions = append(revisions, event.KV.ModRevision)
		if event.KV.ModRevision == 2 {
			return handlerErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	watcher.events <- &WatchEvent{KV: &KV{ModRevision: 1}}
	watcher.events <- &WatchEvent{KV: &KV{ModRevision: 2}}
	select {
	case <-subscription.Done():
	case <-time.After(time.Second):
		t.Fatal("callback subscription did not stop")
	}
	if !errors.Is(subscription.Err(), handlerErr) {
		t.Fatalf("Err()=%v, want handler error", subscription.Err())
	}
	if len(revisions) != 2 || revisions[0] != 1 || revisions[1] != 2 {
		t.Fatalf("revisions=%v", revisions)
	}
}

func TestWatchCallbackRecoversPanicAndExplicitCloseIsClean(t *testing.T) {
	watcher := newCallbackTestWatcher()
	subscription, err := WatchCallback(context.Background(), watcher, func(context.Context, *WatchEvent) error {
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	watcher.events <- &WatchEvent{KV: &KV{ModRevision: 1}}
	awaitChan(t, subscription.Done(), "the watch subscription to terminate")
	if !errors.Is(subscription.Err(), ErrWatchCallbackPanic) {
		t.Fatalf("panic Err()=%v", subscription.Err())
	}

	watcher = newCallbackTestWatcher()
	subscription, err = WatchCallback(context.Background(), watcher, func(context.Context, *WatchEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
	if subscription.Err() != nil {
		t.Fatalf("explicit close Err()=%v", subscription.Err())
	}
}

func TestWatchCallbackReportsWatcherAndContextTermination(t *testing.T) {
	watcher := newCallbackTestWatcher()
	watcher.err = ErrWatchCompacted
	subscription, err := WatchCallback(context.Background(), watcher, func(context.Context, *WatchEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_ = watcher.Close()
	awaitChan(t, subscription.Done(), "the watch subscription to terminate")
	if !errors.Is(subscription.Err(), ErrWatchCompacted) {
		t.Fatalf("watch Err()=%v", subscription.Err())
	}

	ctx, cancel := context.WithCancel(context.Background())
	watcher = newCallbackTestWatcher()
	subscription, err = WatchCallback(ctx, watcher, func(context.Context, *WatchEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitChan(t, subscription.Done(), "the watch subscription to terminate")
	if !errors.Is(subscription.Err(), context.Canceled) {
		t.Fatalf("context Err()=%v", subscription.Err())
	}
}

func TestSubscribeLocalMirrorRejectsUnsupportedMirror(t *testing.T) {
	var mirror ILocalMirror[int]
	_, err := SubscribeLocalMirror(mirror, context.Background(), func(context.Context, LocalMirrorChange[int]) error { return nil }, LocalMirrorSubscribeOptions{})
	if !errors.Is(err, ErrMirrorSubscribeUnsupported) {
		t.Fatalf("error=%v", err)
	}
}

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}
