package etcd

// RR-20261004-NC-12：关闭预算必须覆盖底层 watcher，实际完成仍由 subscription 唯一负责。
import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifetimeFailureWatcher struct {
	lifetimeBlockingWatcher
	calls atomic.Int32
	cause error
}

func (w *lifetimeFailureWatcher) Close() error {
	w.calls.Add(1)
	_ = w.lifetimeBlockingWatcher.Close()
	return w.cause
}

func TestEtcdLifetimeConcurrentCloseWaitsForHandlerAndWatcherAndPreservesError(t *testing.T) {
	cause := errors.New("watcher cleanup failed")
	handlerCause := errors.New("handler failed")
	w := &lifetimeFailureWatcher{lifetimeBlockingWatcher: lifetimeBlockingWatcher{
		events: make(chan *WatchEvent, 1), entered: make(chan struct{}), release: make(chan struct{}),
	}, cause: cause}
	handlerEntered, handlerRelease := make(chan struct{}), make(chan struct{})
	sub, err := WatchCallback(context.Background(), w, func(context.Context, *WatchEvent) error {
		close(handlerEntered)
		<-handlerRelease
		return handlerCause
	})
	if err != nil {
		t.Fatal(err)
	}
	w.events <- &WatchEvent{}
	awaitChan(t, handlerEntered, "handler entry")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() { results <- sub.CloseWithContext(ctx) }()
	}
	awaitChan(t, w.entered, "unique watcher cleanup")
	for range callers {
		if err := awaitChan(t, results, "canceled close wait"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	select {
	case <-sub.Done():
		t.Fatal("Done completed before watcher and handler")
	default:
	}
	close(w.release)
	select {
	case <-sub.Done():
		t.Fatal("watcher completion alone completed Done")
	default:
	}
	close(handlerRelease)
	if err := sub.Close(); !errors.Is(err, cause) {
		t.Fatalf("Close lost cleanup failure: %v", err)
	}
	if err := sub.CloseWithContext(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("retry lost cleanup failure: %v", err)
	}
	if w.calls.Load() != 1 || !errors.Is(sub.Err(), cause) || !errors.Is(sub.Err(), handlerCause) {
		t.Fatalf("calls=%d Err=%v", w.calls.Load(), sub.Err())
	}
}

func TestEtcdLifetimeParentCancellationStartsCleanupBeforeHandlerReturns(t *testing.T) {
	w := &lifetimeBlockingWatcher{events: make(chan *WatchEvent, 1), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerEntered, handlerRelease := make(chan struct{}), make(chan struct{})
	sub, err := WatchCallback(ctx, w, func(context.Context, *WatchEvent) error {
		close(handlerEntered)
		<-handlerRelease
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.events <- &WatchEvent{}
	awaitChan(t, handlerEntered, "handler entry")
	cancel()
	awaitChan(t, w.entered, "parent cancellation starting watcher cleanup")
	close(w.release)
	close(handlerRelease)
	if err := sub.CloseWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type lifetimeBlockingWatcher struct {
	events  chan *WatchEvent
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *lifetimeBlockingWatcher) EventChan() <-chan *WatchEvent { return w.events }
func (w *lifetimeBlockingWatcher) Close() error {
	w.once.Do(func() { close(w.entered); <-w.release; close(w.events) })
	return nil
}

func TestEtcdLifetimeCallbackCloseBudgetIncludesOwnedWatcher(t *testing.T) {
	w := &lifetimeBlockingWatcher{events: make(chan *WatchEvent), entered: make(chan struct{}), release: make(chan struct{})}
	sub, err := WatchCallback(context.Background(), w, func(context.Context, *WatchEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- sub.CloseWithContext(ctx) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("owned watcher.Close not entered")
	}
	returned := false
	var closeErr error
	select {
	case closeErr = <-done:
		returned = true
	case <-time.After(100 * time.Millisecond):
	}
	close(w.release)
	if !returned {
		select {
		case closeErr = <-done:
		case <-time.After(time.Second):
			t.Fatal("close did not finish after release")
		}
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("returned_before_watcher_release=%v err=%v", returned, closeErr)
	if !returned || !errors.Is(closeErr, context.Canceled) {
		t.Fatal("CloseWithContext blocked inside the owned watcher.Close before checking its budget")
	}
}

func TestEtcdLifetimeCallbackErrorAndExplicitCloseOwnership(t *testing.T) {
	w := newCallbackTestWatcher()
	cause := errors.New("callback refused")
	sub, err := WatchCallback(context.Background(), w, func(context.Context, *WatchEvent) error { return cause })
	if err != nil {
		t.Fatal(err)
	}
	w.events <- &WatchEvent{KV: &KV{ModRevision: 1}}
	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("error did not terminate subscription")
	}
	if !errors.Is(sub.Err(), cause) {
		t.Fatal(sub.Err())
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	w2 := newCallbackTestWatcher()
	sub2, err := WatchCallback(context.Background(), w2, func(context.Context, *WatchEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := sub2.CloseWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub2.Err() != nil {
		t.Fatal(sub2.Err())
	}
}
