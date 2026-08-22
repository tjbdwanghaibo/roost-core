package etcd

import (
	"context"
	"fmt"
	"sync"

	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
)

type mirrorInternalChange[T any] struct {
	kind     fetcd.LocalMirrorChangeType
	key      string
	entry    *localMirrorItem[T]
	previous *localMirrorItem[T]
	snapshot map[string]localMirrorItem[T]
	revision int64
}

type mirrorSubscription[T any] struct {
	id       uint64
	handler  fetcd.LocalMirrorHandler[T]
	clone    func(T) (T, error)
	capacity int
	onDone   func(uint64)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	notify chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	queue    []mirrorInternalChange[T]
	head     int
	count    int
	err      error
	stopped  bool
}

func (m *localMirror[T]) Subscribe(ctx context.Context, handler fetcd.LocalMirrorHandler[T], options fetcd.LocalMirrorSubscribeOptions) (fetcd.IWatchSubscription, error) {
	if handler == nil {
		return nil, fmt.Errorf("%w: handler is nil", fetcd.ErrMirrorInvalidSubscriber)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capacity := options.QueueCapacity
	if capacity == 0 {
		capacity = defaultMirrorSubscriberQueue
	}
	if capacity < 0 || capacity > maxMirrorSubscriberQueue {
		return nil, fmt.Errorf("%w: queue capacity must be between 1 and %d", fetcd.ErrMirrorInvalidConfig, maxMirrorSubscriberQueue)
	}

	m.callbackMu.Lock()
	defer m.callbackMu.Unlock()
	select {
	case <-m.ctx.Done():
		return nil, fetcd.ErrMirrorClosed
	default:
	}

	m.nextSubscriber++
	subscriptionCtx, cancel := context.WithCancel(context.Background())
	subscription := &mirrorSubscription[T]{
		id:       m.nextSubscriber,
		handler:  handler,
		clone:    m.cfg.Clone,
		capacity: capacity,
		onDone:   m.removeSubscriber,
		ctx:      subscriptionCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
		notify:   make(chan struct{}, 1),
		queue:    make([]mirrorInternalChange[T], capacity),
	}

	if !options.SkipInitialSnapshot {
		m.mu.RLock()
		items := cloneInternalItems(m.items)
		revision := m.revision
		m.mu.RUnlock()
		subscription.queue[0] = mirrorInternalChange[T]{
			kind: fetcd.LocalMirrorSnapshot, snapshot: items, revision: revision,
		}
		subscription.count = 1
	}
	m.subscribers[subscription.id] = subscription
	go subscription.run()
	go subscription.monitor(ctx, m.done)
	subscription.signal()
	return subscription, nil
}

func (m *localMirror[T]) dispatchLocked(change mirrorInternalChange[T]) {
	for id, subscription := range m.subscribers {
		if !subscription.enqueue(change) {
			delete(m.subscribers, id)
		}
	}
}

func (m *localMirror[T]) removeSubscriber(id uint64) {
	m.callbackMu.Lock()
	delete(m.subscribers, id)
	m.callbackMu.Unlock()
}

func (m *localMirror[T]) stopSubscriptions(err error) {
	m.callbackMu.Lock()
	for id, subscription := range m.subscribers {
		subscription.stop(err)
		delete(m.subscribers, id)
	}
	m.callbackMu.Unlock()
}

func cloneInternalItems[T any](items map[string]localMirrorItem[T]) map[string]localMirrorItem[T] {
	copyItems := make(map[string]localMirrorItem[T], len(items))
	for key, item := range items {
		copyItems[key] = item
	}
	return copyItems
}

func (s *mirrorSubscription[T]) Done() <-chan struct{} { return s.done }

func (s *mirrorSubscription[T]) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *mirrorSubscription[T]) Close() error {
	s.stop(nil)
	<-s.done
	return nil
}

func (s *mirrorSubscription[T]) CloseWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.stop(nil)
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *mirrorSubscription[T]) enqueue(change mirrorInternalChange[T]) bool {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false
	}
	if s.count >= s.capacity {
		s.mu.Unlock()
		s.stop(fetcd.ErrMirrorSubscriberSlow)
		return false
	}
	tail := (s.head + s.count) % s.capacity
	s.queue[tail] = change
	s.count++
	s.mu.Unlock()
	s.signal()
	return true
}

func (s *mirrorSubscription[T]) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *mirrorSubscription[T]) stop(err error) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.err = err
		s.mu.Unlock()
		s.cancel()
		s.signal()
	})
}

func (s *mirrorSubscription[T]) monitor(callerCtx context.Context, mirrorDone <-chan struct{}) {
	select {
	case <-callerCtx.Done():
		s.stop(callerCtx.Err())
	case <-mirrorDone:
		s.stop(fetcd.ErrMirrorClosed)
	case <-s.done:
	}
}

func (s *mirrorSubscription[T]) run() {
	defer func() {
		s.onDone(s.id)
		close(s.done)
	}()
	for {
		change, ok := s.next()
		if ok {
			publicChange, err := s.cloneChange(change)
			if err != nil {
				s.stop(err)
				return
			}
			if err := invokeMirrorHandler(s.ctx, s.handler, publicChange); err != nil {
				s.stop(err)
				return
			}
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.notify:
		}
	}
}

func (s *mirrorSubscription[T]) next() (mirrorInternalChange[T], bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.count == 0 {
		var zero mirrorInternalChange[T]
		return zero, false
	}
	change := s.queue[s.head]
	var zero mirrorInternalChange[T]
	s.queue[s.head] = zero
	s.head = (s.head + 1) % s.capacity
	s.count--
	return change, true
}

func (s *mirrorSubscription[T]) cloneChange(change mirrorInternalChange[T]) (fetcd.LocalMirrorChange[T], error) {
	out := fetcd.LocalMirrorChange[T]{Type: change.kind, Key: change.key, Revision: change.revision}
	if change.entry != nil {
		entry, err := s.cloneEntry(*change.entry)
		if err != nil {
			return out, err
		}
		out.Entry = &entry
	}
	if change.previous != nil {
		previous, err := s.cloneEntry(*change.previous)
		if err != nil {
			return out, err
		}
		out.Previous = &previous
	}
	if change.snapshot != nil {
		out.Snapshot = make(map[string]T, len(change.snapshot))
		for key, item := range change.snapshot {
			value, err := s.clone(item.value)
			if err != nil {
				return out, fmt.Errorf("etcd local mirror: clone callback snapshot %q: %w", key, err)
			}
			out.Snapshot[key] = value
		}
	}
	return out, nil
}

func (s *mirrorSubscription[T]) cloneEntry(item localMirrorItem[T]) (fetcd.LocalMirrorEntry[T], error) {
	value, err := s.clone(item.value)
	if err != nil {
		return fetcd.LocalMirrorEntry[T]{}, fmt.Errorf("etcd local mirror: clone callback value %q: %w", item.kv.Key, err)
	}
	return fetcd.LocalMirrorEntry[T]{
		Key: item.kv.Key, Value: value, CreateRevision: item.kv.CreateRevision,
		ModRevision: item.kv.ModRevision, Version: item.kv.Version, Lease: item.kv.Lease,
	}, nil
}

func invokeMirrorHandler[T any](ctx context.Context, handler fetcd.LocalMirrorHandler[T], change fetcd.LocalMirrorChange[T]) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", fetcd.ErrWatchCallbackPanic, recovered)
		}
	}()
	return handler(ctx, change)
}

var _ fetcd.IWatchSubscription = (*mirrorSubscription[any])(nil)
