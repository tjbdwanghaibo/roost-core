package etcd

import (
	"context"
	"errors"
	"fmt"
	"sync"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watcher implements fetcd.IWatcher by wrapping clientv3 watch channel.
type watcher struct {
	eventCh   chan *fetcd.WatchEvent
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}
	once      sync.Once
	readyOnce sync.Once
	mu        sync.RWMutex
	err       error
}

func newWatcher(ctx context.Context, wch clientv3.WatchChan, cancel context.CancelFunc) *watcher {
	eventCh := make(chan *fetcd.WatchEvent, 64)
	if cancel == nil {
		watchCtx, derivedCancel := context.WithCancel(ctx)
		ctx = watchCtx
		cancel = derivedCancel
	}
	w := &watcher{
		eventCh: eventCh,
		cancel:  cancel,
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
	}
	go w.loop(ctx, wch)
	return w
}

func (w *watcher) Ready() <-chan struct{} { return w.ready }

func (w *watcher) EventChan() <-chan *fetcd.WatchEvent {
	return w.eventCh
}

func (w *watcher) Close() error {
	w.once.Do(func() {
		w.cancel()
		<-w.done
	})
	return nil
}

// WatchError reports the terminal etcd watch error, if any.
func (w *watcher) WatchError() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

func (w *watcher) setError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}

func (w *watcher) loop(ctx context.Context, wch clientv3.WatchChan) {
	defer func() {
		w.signalReady()
		close(w.eventCh)
		close(w.done)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-wch:
			if !ok {
				if ctx.Err() == nil {
					w.setError(fetcd.ErrWatchClosed)
				}
				return
			}
			if resp.Canceled {
				if resp.CompactRevision != 0 {
					w.setError(fmt.Errorf("%w: %d", fetcd.ErrWatchCompacted, resp.CompactRevision))
				} else if err := resp.Err(); err != nil {
					w.setError(fmt.Errorf("%w: %w", fetcd.ErrWatchCanceled, err))
				} else {
					w.setError(fetcd.ErrWatchCanceled)
				}
				w.signalReady()
				return
			}
			if err := resp.Err(); err != nil && !errors.Is(err, context.Canceled) {
				w.setError(err)
				w.signalReady()
				return
			}
			w.signalReady()
			for _, ev := range resp.Events {
				event := &fetcd.WatchEvent{}
				if ev.Kv != nil {
					event.KV = convertMvccKV(ev.Kv)
				}
				switch ev.Type {
				case mvccpb.PUT:
					event.Type = fetcd.EventPut
				case mvccpb.DELETE:
					event.Type = fetcd.EventDelete
				}
				if ev.PrevKv != nil {
					event.PrevKV = convertMvccKV(ev.PrevKv)
				}
				select {
				case w.eventCh <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (w *watcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}

func convertMvccKV(kv *mvccpb.KeyValue) *fetcd.KV {
	return &fetcd.KV{
		Key:            string(kv.Key),
		Value:          string(kv.Value),
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          kv.Lease,
	}
}

var _ fetcd.IWatcher = (*watcher)(nil)
var _ fetcd.IWatcherError = (*watcher)(nil)
var _ fetcd.IWatcherReady = (*watcher)(nil)
