package etcd

import (
	"context"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watcher implements fetcd.IWatcher by wrapping clientv3 watch channel.
type watcher struct {
	eventCh chan *fetcd.WatchEvent
	cancel  context.CancelFunc
}

func newWatcher(wch clientv3.WatchChan) *watcher {
	eventCh := make(chan *fetcd.WatchEvent, 64)
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{
		eventCh: eventCh,
		cancel:  cancel,
	}
	go w.loop(ctx, wch)
	return w
}

func (w *watcher) EventChan() <-chan *fetcd.WatchEvent {
	return w.eventCh
}

func (w *watcher) Close() error {
	w.cancel()
	return nil
}

func (w *watcher) loop(ctx context.Context, wch clientv3.WatchChan) {
	defer close(w.eventCh)
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-wch:
			if !ok {
				return
			}
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
