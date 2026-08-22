package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
)

const (
	defaultMirrorRetryMin        = 100 * time.Millisecond
	defaultMirrorRetryMax        = 5 * time.Second
	defaultMirrorSubscriberQueue = 64
	maxMirrorSubscriberQueue     = 65536
)

type mirrorClient interface {
	GetPrefixSnapshot(ctx context.Context, prefix string) (*fetcd.PrefixSnapshot, error)
	WatchPrefix(ctx context.Context, prefix string, opts ...fetcd.WatchOption) fetcd.IWatcher
	Put(ctx context.Context, key, value string) error
	PutWithLease(ctx context.Context, key, value string, leaseID int64) error
	Delete(ctx context.Context, key string) error
	Txn(ctx context.Context, cmp fetcd.Cmp, onSuccess, onFailure []fetcd.Op) (*fetcd.TxnResponse, error)
}

type mirrorClientAdapter struct {
	fetcd.IEtcd
	fetcd.IPrefixSnapshotReader
}

type localMirrorItem[T any] struct {
	value T
	kv    fetcd.KV
}

type localMirror[T any] struct {
	client mirrorClient
	cfg    fetcd.LocalMirrorConfig[T]

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu        sync.RWMutex
	items     map[string]localMirrorItem[T]
	revision  int64
	synced    bool
	lastError error
	stateCh   chan struct{}

	callbackMu     sync.Mutex
	subscribers    map[uint64]*mirrorSubscription[T]
	nextSubscriber uint64
}

// NewLocalMirror creates a typed local mirror backed by a consistent etcd
// prefix snapshot and a watch starting at snapshot.Revision+1.
func NewLocalMirror[T any](ctx context.Context, client fetcd.IEtcd, cfg fetcd.LocalMirrorConfig[T]) (fetcd.ILocalMirror[T], error) {
	if client == nil {
		return nil, errors.New("etcd local mirror: client is nil")
	}
	snapshotReader, ok := client.(fetcd.IPrefixSnapshotReader)
	if !ok {
		return nil, errors.New("etcd local mirror: client does not support revisioned prefix snapshots")
	}
	return newLocalMirror(ctx, mirrorClientAdapter{IEtcd: client, IPrefixSnapshotReader: snapshotReader}, cfg)
}

// JSONLocalMirrorConfig returns a production-safe JSON codec. Each read is a
// deep copy, including nested maps, slices, and pointers.
func JSONLocalMirrorConfig[T any](prefix string) fetcd.LocalMirrorConfig[T] {
	return fetcd.LocalMirrorConfig[T]{
		Prefix: prefix,
		Decode: func(_ string, value string) (T, error) {
			var out T
			err := json.Unmarshal([]byte(value), &out)
			return out, err
		},
		Encode: func(value T) (string, error) {
			data, err := json.Marshal(value)
			return string(data), err
		},
		Clone: func(value T) (T, error) {
			var out T
			data, err := json.Marshal(value)
			if err != nil {
				return out, err
			}
			err = json.Unmarshal(data, &out)
			return out, err
		},
	}
}

func newLocalMirror[T any](ctx context.Context, client mirrorClient, cfg fetcd.LocalMirrorConfig[T]) (*localMirror[T], error) {
	if client == nil {
		return nil, errors.New("etcd local mirror: client is nil")
	}
	if cfg.Prefix == "" {
		return nil, fmt.Errorf("%w: prefix is empty", fetcd.ErrMirrorInvalidConfig)
	}
	if cfg.Decode == nil || cfg.Encode == nil || cfg.Clone == nil {
		return nil, fmt.Errorf("%w: Decode, Encode, and Clone are required", fetcd.ErrMirrorInvalidConfig)
	}
	if cfg.RetryMinInterval <= 0 {
		cfg.RetryMinInterval = defaultMirrorRetryMin
	}
	if cfg.RetryMaxInterval <= 0 {
		cfg.RetryMaxInterval = defaultMirrorRetryMax
	}
	if cfg.RetryMaxInterval < cfg.RetryMinInterval {
		return nil, fmt.Errorf("%w: retry max interval is smaller than retry min interval", fetcd.ErrMirrorInvalidConfig)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mirrorCtx, cancel := context.WithCancel(ctx)
	m := &localMirror[T]{
		client:      client,
		cfg:         cfg,
		ctx:         mirrorCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		items:       make(map[string]localMirrorItem[T]),
		stateCh:     make(chan struct{}),
		subscribers: make(map[uint64]*mirrorSubscription[T]),
	}
	if err := m.reload(); err != nil {
		cancel()
		return nil, err
	}
	go m.run()
	return m, nil
}

func (m *localMirror[T]) Get(key string) (T, bool, error) {
	entry, ok, err := m.GetEntry(key)
	return entry.Value, ok, err
}

func (m *localMirror[T]) GetEntry(key string) (fetcd.LocalMirrorEntry[T], bool, error) {
	if err := m.validateKey(key); err != nil {
		var zero fetcd.LocalMirrorEntry[T]
		return zero, false, err
	}
	m.mu.RLock()
	item, ok := m.items[key]
	stateErr := m.readStateErrorLocked()
	m.mu.RUnlock()
	if !ok {
		var zero fetcd.LocalMirrorEntry[T]
		return zero, false, stateErr
	}
	value, err := m.cfg.Clone(item.value)
	if err != nil {
		var zero fetcd.LocalMirrorEntry[T]
		return zero, false, fmt.Errorf("etcd local mirror: clone %q: %w", key, err)
	}
	return fetcd.LocalMirrorEntry[T]{
		Key:            item.kv.Key,
		Value:          value,
		CreateRevision: item.kv.CreateRevision,
		ModRevision:    item.kv.ModRevision,
		Version:        item.kv.Version,
		Lease:          item.kv.Lease,
	}, true, stateErr
}

func (m *localMirror[T]) Snapshot() (map[string]T, error) {
	m.mu.RLock()
	items := make(map[string]localMirrorItem[T], len(m.items))
	for key, item := range m.items {
		items[key] = item
	}
	stateErr := m.readStateErrorLocked()
	m.mu.RUnlock()
	out := make(map[string]T, len(items))
	for key, item := range items {
		value, err := m.cfg.Clone(item.value)
		if err != nil {
			return nil, fmt.Errorf("etcd local mirror: clone %q: %w", key, err)
		}
		out[key] = value
	}
	return out, stateErr
}

func (m *localMirror[T]) Revision() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision
}

func (m *localMirror[T]) LastError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastError
}

func (m *localMirror[T]) Status() fetcd.LocalMirrorStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fetcd.LocalMirrorStatus{Revision: m.revision, Synced: m.synced, LastError: m.lastError}
}

func (m *localMirror[T]) WaitForSync(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		m.mu.RLock()
		if m.synced {
			m.mu.RUnlock()
			return nil
		}
		stateCh := m.stateCh
		m.mu.RUnlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.done:
			return fetcd.ErrMirrorClosed
		case <-stateCh:
		}
	}
}

func (m *localMirror[T]) Publish(ctx context.Context, key string, value T) error {
	return m.PublishWithOptions(ctx, key, value, fetcd.LocalMirrorPublishOptions{})
}

func (m *localMirror[T]) PublishWithOptions(ctx context.Context, key string, value T, options fetcd.LocalMirrorPublishOptions) error {
	if err := m.validateWrite(ctx, key); err != nil {
		return err
	}
	if options.LeaseID < 0 {
		return fmt.Errorf("%w: lease id is negative", fetcd.ErrMirrorInvalidConfig)
	}
	encoded, err := m.encode(value)
	if err != nil {
		return err
	}
	if options.LeaseID != 0 {
		return m.client.PutWithLease(ctx, key, encoded, options.LeaseID)
	}
	return m.client.Put(ctx, key, encoded)
}

func (m *localMirror[T]) Delete(ctx context.Context, key string) error {
	if err := m.validateWrite(ctx, key); err != nil {
		return err
	}
	return m.client.Delete(ctx, key)
}

func (m *localMirror[T]) PublishIfRevision(ctx context.Context, key string, expectedRevision int64, value T) (bool, error) {
	return m.PublishIfRevisionWithOptions(ctx, key, expectedRevision, value, fetcd.LocalMirrorPublishOptions{})
}

func (m *localMirror[T]) PublishIfRevisionWithOptions(ctx context.Context, key string, expectedRevision int64, value T, options fetcd.LocalMirrorPublishOptions) (bool, error) {
	if err := m.validateRevisionWrite(ctx, key, expectedRevision); err != nil {
		return false, err
	}
	if options.LeaseID < 0 {
		return false, fmt.Errorf("%w: lease id is negative", fetcd.ErrMirrorInvalidConfig)
	}
	encoded, err := m.encode(value)
	if err != nil {
		return false, err
	}
	return m.compareAndApply(ctx, key, expectedRevision, fetcd.Op{Type: fetcd.OpPut, Key: key, Value: encoded, Lease: options.LeaseID})
}

func (m *localMirror[T]) DeleteIfRevision(ctx context.Context, key string, expectedRevision int64) (bool, error) {
	if err := m.validateRevisionWrite(ctx, key, expectedRevision); err != nil {
		return false, err
	}
	return m.compareAndApply(ctx, key, expectedRevision, fetcd.Op{Type: fetcd.OpDelete, Key: key})
}

func (m *localMirror[T]) compareAndApply(ctx context.Context, key string, expectedRevision int64, op fetcd.Op) (bool, error) {
	target := fetcd.CmpModRevision
	if expectedRevision == 0 {
		target = fetcd.CmpVersion
	}
	resp, err := m.client.Txn(ctx, fetcd.Cmp{
		Key: key, Target: target, Op: fetcd.CmpEqual, Value: expectedRevision,
	}, []fetcd.Op{op}, nil)
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, errors.New("etcd local mirror: nil transaction response")
	}
	return resp.Succeeded, nil
}

func (m *localMirror[T]) encode(value T) (string, error) {
	cloned, err := m.cfg.Clone(value)
	if err != nil {
		return "", fmt.Errorf("etcd local mirror: clone publish value: %w", err)
	}
	encoded, err := m.cfg.Encode(cloned)
	if err != nil {
		return "", fmt.Errorf("etcd local mirror: encode publish value: %w", err)
	}
	return encoded, nil
}

func (m *localMirror[T]) validateWrite(ctx context.Context, key string) error {
	if ctx == nil {
		return errors.New("etcd local mirror: publish context is nil")
	}
	if err := m.validateKey(key); err != nil {
		return err
	}
	select {
	case <-m.ctx.Done():
		return fetcd.ErrMirrorClosed
	case <-m.done:
		return fetcd.ErrMirrorClosed
	default:
	}
	return nil
}

func (m *localMirror[T]) validateKey(key string) error {
	if !strings.HasPrefix(key, m.cfg.Prefix) {
		return fmt.Errorf("%w: key %q prefix %q", fetcd.ErrMirrorKeyOutsidePrefix, key, m.cfg.Prefix)
	}
	return nil
}

func (m *localMirror[T]) validateRevisionWrite(ctx context.Context, key string, expectedRevision int64) error {
	if expectedRevision < 0 {
		return errors.New("etcd local mirror: expected revision is negative")
	}
	return m.validateWrite(ctx, key)
}

func (m *localMirror[T]) Done() <-chan struct{} { return m.done }

func (m *localMirror[T]) Close() error {
	m.once.Do(m.cancel)
	<-m.done
	return nil
}

func (m *localMirror[T]) run() {
	defer func() {
		m.setStatus(false, fetcd.ErrMirrorClosed)
		m.stopSubscriptions(fetcd.ErrMirrorClosed)
		close(m.done)
	}()
	backoff := m.cfg.RetryMinInterval
	for {
		if m.ctx.Err() != nil {
			return
		}
		revision := m.Revision()
		watcher := m.client.WatchPrefix(m.ctx, m.cfg.Prefix, fetcd.WatchOption{WithRevision: revision + 1, CreatedNotify: true})
		if watcher == nil {
			m.setStatus(false, errors.New("etcd local mirror: client returned nil watcher"))
		} else {
			if !m.waitWatcherReady(watcher) {
				_ = watcher.Close()
				return
			}
			if m.ctx.Err() != nil {
				_ = watcher.Close()
				return
			}
			if watchErr, ok := watcher.(fetcd.IWatcherError); ok && watchErr.WatchError() != nil {
				m.setStatus(false, watchErr.WatchError())
				_ = watcher.Close()
			} else {
				m.setStatus(true, nil)
				err := m.consume(watcher)
				if watchErr, ok := watcher.(fetcd.IWatcherError); ok && watchErr.WatchError() != nil {
					err = watchErr.WatchError()
				}
				_ = watcher.Close()
				if m.ctx.Err() != nil {
					return
				}
				if err != nil {
					m.setStatus(false, err)
				}
			}
		}
		if !m.waitRetry(backoff) {
			return
		}
		for {
			if err := m.reload(); err == nil {
				backoff = m.cfg.RetryMinInterval
				break
			} else {
				m.setStatus(false, err)
			}
			if !m.waitRetry(backoff) {
				return
			}
			backoff = nextMirrorBackoff(backoff, m.cfg.RetryMaxInterval)
		}
	}
}

func (m *localMirror[T]) waitWatcherReady(watcher fetcd.IWatcher) bool {
	ready, ok := watcher.(fetcd.IWatcherReady)
	if !ok {
		return true
	}
	select {
	case <-m.ctx.Done():
		return false
	case <-ready.Ready():
		return true
	}
}

func (m *localMirror[T]) consume(watcher fetcd.IWatcher) error {
	for {
		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		case event, ok := <-watcher.EventChan():
			if !ok {
				return errors.New("etcd local mirror: watch closed")
			}
			if err := m.apply(event); err != nil {
				return err
			}
		}
	}
}

func (m *localMirror[T]) apply(event *fetcd.WatchEvent) error {
	if event == nil || event.KV == nil {
		return errors.New("etcd local mirror: watch event has no KV")
	}
	kv := *event.KV
	if !strings.HasPrefix(kv.Key, m.cfg.Prefix) {
		return fmt.Errorf("etcd local mirror: watched key %q is outside prefix %q", kv.Key, m.cfg.Prefix)
	}
	m.mu.RLock()
	stale := kv.ModRevision > 0 && kv.ModRevision < m.revision
	m.mu.RUnlock()
	if stale {
		return nil
	}
	var item localMirrorItem[T]
	if event.Type == fetcd.EventPut {
		value, err := m.decode(kv)
		if err != nil {
			return err
		}
		item = localMirrorItem[T]{value: value, kv: kv}
	}
	m.callbackMu.Lock()
	defer m.callbackMu.Unlock()
	m.mu.Lock()
	if kv.ModRevision > 0 && kv.ModRevision < m.revision {
		m.mu.Unlock()
		return nil
	}
	previous, existed := m.items[kv.Key]
	switch event.Type {
	case fetcd.EventPut:
		m.items[kv.Key] = item
	case fetcd.EventDelete:
		delete(m.items, kv.Key)
	default:
		m.mu.Unlock()
		return fmt.Errorf("etcd local mirror: unknown watch event type %d", event.Type)
	}
	if kv.ModRevision > m.revision {
		m.revision = kv.ModRevision
	}
	m.lastError = nil
	revision := m.revision
	m.mu.Unlock()

	change := mirrorInternalChange[T]{key: kv.Key, revision: revision}
	if existed {
		previousCopy := previous
		change.previous = &previousCopy
	}
	if event.Type == fetcd.EventPut {
		itemCopy := item
		change.kind = fetcd.LocalMirrorPut
		change.entry = &itemCopy
	} else {
		change.kind = fetcd.LocalMirrorDelete
	}
	m.dispatchLocked(change)
	return nil
}

func (m *localMirror[T]) reload() error {
	snapshot, err := m.client.GetPrefixSnapshot(m.ctx, m.cfg.Prefix)
	if err != nil {
		return fmt.Errorf("etcd local mirror: load prefix %q: %w", m.cfg.Prefix, err)
	}
	if snapshot == nil || snapshot.Revision < 0 {
		return errors.New("etcd local mirror: invalid prefix snapshot")
	}
	items := make(map[string]localMirrorItem[T], len(snapshot.KVs))
	for _, kvPtr := range snapshot.KVs {
		if kvPtr == nil {
			return errors.New("etcd local mirror: prefix snapshot contains nil KV")
		}
		kv := *kvPtr
		if !strings.HasPrefix(kv.Key, m.cfg.Prefix) {
			return fmt.Errorf("etcd local mirror: snapshot key %q is outside prefix %q", kv.Key, m.cfg.Prefix)
		}
		value, err := m.decode(kv)
		if err != nil {
			return err
		}
		items[kv.Key] = localMirrorItem[T]{value: value, kv: kv}
	}
	m.callbackMu.Lock()
	defer m.callbackMu.Unlock()
	m.mu.Lock()
	if snapshot.Revision < m.revision {
		m.mu.Unlock()
		return fmt.Errorf("etcd local mirror: snapshot revision regressed from %d to %d", m.revision, snapshot.Revision)
	}
	m.items = items
	m.revision = snapshot.Revision
	m.lastError = nil
	m.mu.Unlock()
	m.dispatchLocked(mirrorInternalChange[T]{
		kind:     fetcd.LocalMirrorSnapshot,
		snapshot: cloneInternalItems(items),
		revision: snapshot.Revision,
	})
	return nil
}

func (m *localMirror[T]) decode(kv fetcd.KV) (T, error) {
	value, err := m.cfg.Decode(kv.Key, kv.Value)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("etcd local mirror: decode %q at revision %d: %w", kv.Key, kv.ModRevision, err)
	}
	cloned, err := m.cfg.Clone(value)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("etcd local mirror: clone decoded %q: %w", kv.Key, err)
	}
	return cloned, nil
}

func (m *localMirror[T]) setStatus(synced bool, err error) {
	m.mu.Lock()
	changed := m.synced != synced || !errors.Is(m.lastError, err) || !errors.Is(err, m.lastError)
	m.synced = synced
	m.lastError = err
	if changed {
		close(m.stateCh)
		m.stateCh = make(chan struct{})
	}
	m.mu.Unlock()
}

func (m *localMirror[T]) readStateErrorLocked() error {
	if m.synced {
		return nil
	}
	if m.lastError != nil {
		return m.lastError
	}
	return fetcd.ErrMirrorNotSynced
}

func (m *localMirror[T]) waitRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextMirrorBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > max {
		return max
	}
	return next
}

var _ fetcd.ILocalMirror[any] = (*localMirror[any])(nil)
var _ fetcd.ILocalMirrorSubscriber[any] = (*localMirror[any])(nil)
