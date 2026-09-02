package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
)

type mirrorTestRecord struct {
	Count  int               `json:"count"`
	Labels map[string]string `json:"labels"`
}

func mirrorTestConfig() fetcd.LocalMirrorConfig[mirrorTestRecord] {
	return fetcd.LocalMirrorConfig[mirrorTestRecord]{
		Prefix: "/sync/",
		Decode: func(_ string, value string) (mirrorTestRecord, error) {
			var out mirrorTestRecord
			err := json.Unmarshal([]byte(value), &out)
			return out, err
		},
		Encode: func(value mirrorTestRecord) (string, error) {
			data, err := json.Marshal(value)
			return string(data), err
		},
		Clone: func(value mirrorTestRecord) (mirrorTestRecord, error) {
			out := mirrorTestRecord{Count: value.Count}
			if value.Labels != nil {
				out.Labels = make(map[string]string, len(value.Labels))
				for key, item := range value.Labels {
					out.Labels[key] = item
				}
			}
			return out, nil
		},
		RetryMinInterval: time.Millisecond,
		RetryMaxInterval: 5 * time.Millisecond,
	}
}

type mirrorTestWatcher struct {
	events    chan *fetcd.WatchEvent
	once      sync.Once
	readyOnce sync.Once
	closed    chan struct{}
	ready     chan struct{}
	mu        sync.RWMutex
	err       error
}

func newMirrorTestWatcher() *mirrorTestWatcher {
	return &mirrorTestWatcher{events: make(chan *fetcd.WatchEvent, 256), closed: make(chan struct{}), ready: make(chan struct{})}
}

func (w *mirrorTestWatcher) EventChan() <-chan *fetcd.WatchEvent { return w.events }
func (w *mirrorTestWatcher) Ready() <-chan struct{}              { return w.ready }
func (w *mirrorTestWatcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}
func (w *mirrorTestWatcher) Close() error {
	w.once.Do(func() {
		w.signalReady()
		close(w.closed)
	})
	return nil
}
func (w *mirrorTestWatcher) WatchError() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}
func (w *mirrorTestWatcher) setError(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

type mirrorTestClient struct {
	mu              sync.Mutex
	snapshot        *fetcd.PrefixSnapshot
	snapshotErr     error
	watchers        []*mirrorTestWatcher
	watchRevisions  chan int64
	puts            []fetcd.Op
	deletes         []string
	txns            []mirrorTestTxn
	txnSucceeded    bool
	delayWatchReady bool
}

type mirrorTestTxn struct {
	cmp     fetcd.Cmp
	success []fetcd.Op
	failure []fetcd.Op
}

func newMirrorTestClient(snapshot *fetcd.PrefixSnapshot) *mirrorTestClient {
	return &mirrorTestClient{snapshot: snapshot, watchRevisions: make(chan int64, 8), txnSucceeded: true}
}

func (c *mirrorTestClient) GetPrefixSnapshot(context.Context, string) (*fetcd.PrefixSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshotErr != nil {
		return nil, c.snapshotErr
	}
	copySnapshot := &fetcd.PrefixSnapshot{Revision: c.snapshot.Revision, KVs: make([]*fetcd.KV, len(c.snapshot.KVs))}
	for i, kv := range c.snapshot.KVs {
		copyKV := *kv
		copySnapshot.KVs[i] = &copyKV
	}
	return copySnapshot, nil
}

func (c *mirrorTestClient) WatchPrefix(_ context.Context, _ string, opts ...fetcd.WatchOption) fetcd.IWatcher {
	revision := int64(0)
	for _, opt := range opts {
		if opt.WithRevision != 0 {
			revision = opt.WithRevision
		}
	}
	watcher := newMirrorTestWatcher()
	c.mu.Lock()
	c.watchers = append(c.watchers, watcher)
	delayReady := c.delayWatchReady
	c.mu.Unlock()
	c.watchRevisions <- revision
	if !delayReady {
		watcher.signalReady()
	}
	return watcher
}

func (c *mirrorTestClient) Put(_ context.Context, key, value string) error {
	c.mu.Lock()
	c.puts = append(c.puts, fetcd.Op{Type: fetcd.OpPut, Key: key, Value: value})
	c.mu.Unlock()
	return nil
}

func (c *mirrorTestClient) PutWithLease(_ context.Context, key, value string, leaseID int64) error {
	c.mu.Lock()
	c.puts = append(c.puts, fetcd.Op{Type: fetcd.OpPut, Key: key, Value: value, Lease: leaseID})
	c.mu.Unlock()
	return nil
}

func (c *mirrorTestClient) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	c.deletes = append(c.deletes, key)
	c.mu.Unlock()
	return nil
}

func (c *mirrorTestClient) Txn(_ context.Context, cmp fetcd.Cmp, success, failure []fetcd.Op) (*fetcd.TxnResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.txns = append(c.txns, mirrorTestTxn{cmp: cmp, success: append([]fetcd.Op(nil), success...), failure: append([]fetcd.Op(nil), failure...)})
	return &fetcd.TxnResponse{Succeeded: c.txnSucceeded, Revision: c.snapshot.Revision + 1}, nil
}

func (c *mirrorTestClient) watcher(index int) *mirrorTestWatcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watchers[index]
}

func (c *mirrorTestClient) setSnapshot(snapshot *fetcd.PrefixSnapshot) {
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
}

func (c *mirrorTestClient) setSnapshotError(err error) {
	c.mu.Lock()
	c.snapshotErr = err
	c.mu.Unlock()
}

func TestLocalMirrorAppliesWatchEventsAndReturnsIndependentValues(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{
		Key: "/sync/a", Value: `{"count":1,"labels":{"owner":"one"}}`, ModRevision: 5,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatalf("newLocalMirror: %v", err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	if revision := <-client.watchRevisions; revision != 6 {
		t.Fatalf("watch revision=%d, want snapshot revision+1=6", revision)
	}
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	initial, ok, err := mirror.Get("/sync/a")
	if err != nil || !ok || initial.Count != 1 {
		t.Fatalf("initial value=%+v ok=%v err=%v", initial, ok, err)
	}
	initial.Labels["owner"] = "mutated"
	again, _, _ := mirror.Get("/sync/a")
	if again.Labels["owner"] != "one" {
		t.Fatalf("caller mutation leaked into mirror: %+v", again)
	}

	watcher := client.watcher(0)
	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					value, ok, getErr := mirror.Get("/sync/a")
					if getErr == nil && ok && value.Labels != nil {
						value.Labels["reader"] = "private"
					}
					_, _ = mirror.Snapshot()
				}
			}
		}()
	}
	for i := 6; i <= 50; i++ {
		watcher.events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{
			Key: "/sync/a", Value: fmt.Sprintf(`{"count":%d,"labels":{"owner":"writer"}}`, i), ModRevision: int64(i),
		}}
	}
	waitMirrorValue(t, mirror, "/sync/a", 50)
	watcher.events <- &fetcd.WatchEvent{Type: fetcd.EventDelete, KV: &fetcd.KV{Key: "/sync/a", ModRevision: 51}}
	waitMirrorMissing(t, mirror, "/sync/a")
	close(stopReaders)
	readers.Wait()
}

func TestLocalMirrorWaitsForServerWatchReadiness(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 2})
	client.delayWatchReady = true
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	if status := mirror.Status(); status.Synced {
		t.Fatalf("mirror reported synced before server watch readiness: %+v", status)
	}
	client.watcher(0).signalReady()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mirror.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
}

func TestLocalMirrorResnapshotsAfterWatchCloses(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3, KVs: []*fetcd.KV{{
		Key: "/sync/old", Value: `{"count":1}`, ModRevision: 3,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatalf("newLocalMirror: %v", err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	if revision := <-client.watchRevisions; revision != 4 {
		t.Fatalf("first watch revision=%d, want 4", revision)
	}
	client.setSnapshot(&fetcd.PrefixSnapshot{Revision: 10, KVs: []*fetcd.KV{{
		Key: "/sync/new", Value: `{"count":10}`, ModRevision: 10,
	}}})
	close(client.watcher(0).events)
	select {
	case revision := <-client.watchRevisions:
		if revision != 11 {
			t.Fatalf("reconnected watch revision=%d, want 11", revision)
		}
	case <-time.After(time.Second):
		t.Fatal("mirror did not reconnect after watch closed")
	}
	waitMirrorValue(t, mirror, "/sync/new", 10)
	waitMirrorMissing(t, mirror, "/sync/old")
}

func TestLocalMirrorPublishesAndUsesRevisionCAS(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{
		Key: "/sync/a", Value: `{"count":1}`, CreateRevision: 2, ModRevision: 5, Version: 3,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatalf("newLocalMirror: %v", err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	entry, ok, err := mirror.GetEntry("/sync/a")
	if err != nil || !ok || entry.ModRevision != 5 || entry.Value.Count != 1 {
		t.Fatalf("entry=%+v ok=%v err=%v", entry, ok, err)
	}
	if err := mirror.Publish(context.Background(), "/sync/a", mirrorTestRecord{Count: 2}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	client.mu.Lock()
	if len(client.puts) != 1 || client.puts[0].Key != "/sync/a" {
		t.Fatalf("puts=%+v", client.puts)
	}
	client.mu.Unlock()
	if err := mirror.PublishWithOptions(context.Background(), "/sync/leased", mirrorTestRecord{Count: 4}, fetcd.LocalMirrorPublishOptions{LeaseID: 7}); err != nil {
		t.Fatalf("PublishWithOptions: %v", err)
	}
	client.mu.Lock()
	if len(client.puts) != 2 || client.puts[1].Lease != 7 {
		t.Fatalf("leased puts=%+v", client.puts)
	}
	client.mu.Unlock()
	beforeWatch, _, _ := mirror.Get("/sync/a")
	if beforeWatch.Count != 1 {
		t.Fatalf("Publish mutated local state before watch delivery: %+v", beforeWatch)
	}
	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{
		Key: "/sync/a", Value: `{"count":2}`, CreateRevision: 2, ModRevision: 6, Version: 4,
	}}
	waitMirrorValue(t, mirror, "/sync/a", 2)

	succeeded, err := mirror.PublishIfRevisionWithOptions(context.Background(), "/sync/a", 6, mirrorTestRecord{Count: 3}, fetcd.LocalMirrorPublishOptions{LeaseID: 9})
	if err != nil || !succeeded {
		t.Fatalf("PublishIfRevision succeeded=%v err=%v", succeeded, err)
	}
	succeeded, err = mirror.DeleteIfRevision(context.Background(), "/sync/a", 6)
	if err != nil || !succeeded {
		t.Fatalf("DeleteIfRevision succeeded=%v err=%v", succeeded, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.txns) != 2 {
		t.Fatalf("txns=%+v", client.txns)
	}
	if got := client.txns[0].cmp; got.Target != fetcd.CmpModRevision || got.Op != fetcd.CmpEqual || got.Value != int64(6) {
		t.Fatalf("publish cmp=%+v", got)
	}
	if len(client.txns[0].success) != 1 || client.txns[0].success[0].Type != fetcd.OpPut || client.txns[0].success[0].Lease != 9 {
		t.Fatalf("publish ops=%+v", client.txns[0].success)
	}
	if len(client.txns[1].success) != 1 || client.txns[1].success[0].Type != fetcd.OpDelete {
		t.Fatalf("delete ops=%+v", client.txns[1].success)
	}
}

func TestLocalMirrorUsesNativeEtcdPrefixSemantics(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3, KVs: []*fetcd.KV{{
		Key: "/sync/", Value: `{"count":1}`, ModRevision: 3,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, ok, err := mirror.Get("/sync/")
	if err != nil || !ok || value.Count != 1 {
		t.Fatalf("exact prefix value=%+v ok=%v err=%v", value, ok, err)
	}
	if err := mirror.Publish(context.Background(), "/sync/", mirrorTestRecord{Count: 2}); err != nil {
		t.Fatalf("publish exact prefix key: %v", err)
	}
}

func TestLocalMirrorReportsWatchFailuresAndRejectsOutOfScopeKeys(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{Key: "/sync/a", Value: `{"count":1}`, ModRevision: 5}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)

	if _, _, err := mirror.Get("/other/a"); !errors.Is(err, fetcd.ErrMirrorKeyOutsidePrefix) {
		t.Fatalf("outside key error=%v", err)
	}
	if err := mirror.Publish(context.Background(), "/other/a", mirrorTestRecord{}); !errors.Is(err, fetcd.ErrMirrorKeyOutsidePrefix) {
		t.Fatalf("outside publish error=%v", err)
	}

	snapshotErr := errors.New("etcd unavailable")
	client.setSnapshotError(snapshotErr)
	watcher := client.watcher(0)
	watcher.setError(fetcd.ErrWatchCompacted)
	close(watcher.events)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := mirror.Status()
		if !status.Synced && errors.Is(status.LastError, snapshotErr) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := mirror.Status(); status.Synced || !errors.Is(status.LastError, snapshotErr) {
		t.Fatalf("status after failed resnapshot=%+v", status)
	}
	client.setSnapshotError(nil)
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
}

func TestLocalMirrorCloseMarksViewUnavailable(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 1})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	awaitWatchStarted(t, client)
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	if status := mirror.Status(); status.Synced || !errors.Is(status.LastError, fetcd.ErrMirrorClosed) {
		t.Fatalf("status after close=%+v", status)
	}
	if err := mirror.WaitForSync(context.Background()); !errors.Is(err, fetcd.ErrMirrorClosed) {
		t.Fatalf("WaitForSync after Close = %v", err)
	}
	if err := mirror.Publish(context.Background(), "/sync/a", mirrorTestRecord{}); !errors.Is(err, fetcd.ErrMirrorClosed) {
		t.Fatalf("Publish after Close = %v", err)
	}
}

func TestLocalMirrorIgnoresStaleMalformedWatchValue(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{Key: "/sync/a", Value: `{"count":1}`, ModRevision: 5}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatal(err)
	}

	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: "not-json", ModRevision: 4}}
	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: `{"count":2}`, ModRevision: 6}}
	waitMirrorValue(t, mirror, "/sync/a", 2)
	if status := mirror.Status(); !status.Synced || status.Revision != 6 || status.LastError != nil {
		t.Fatalf("stale event changed mirror status: %+v", status)
	}
	value, ok, err := mirror.Get("/sync/a")
	if err != nil || !ok || value.Count != 2 {
		t.Fatalf("stale event changed mirror value: value=%+v ok=%v err=%v", value, ok, err)
	}
	select {
	case revision := <-client.watchRevisions:
		t.Fatalf("stale event unexpectedly restarted watch at revision %d", revision)
	default:
	}
}

func TestLocalMirrorSubscriptionDeliversAtomicSnapshotAndOrderedChanges(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{
		Key: "/sync/a", Value: `{"count":1,"labels":{"owner":"one"}}`, ModRevision: 5,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	if err := mirror.WaitForSync(context.Background()); err != nil {
		t.Fatal(err)
	}

	changes := make(chan fetcd.LocalMirrorChange[mirrorTestRecord], 3)
	subscription, err := fetcd.SubscribeLocalMirror[mirrorTestRecord](mirror, context.Background(), func(_ context.Context, change fetcd.LocalMirrorChange[mirrorTestRecord]) error {
		// Calling back into the mirror proves handlers do not run under its state lock.
		_, _, _ = mirror.Get("/sync/a")
		changes <- change
		return nil
	}, fetcd.LocalMirrorSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })

	initial := receiveMirrorChange(t, changes)
	if initial.Type != fetcd.LocalMirrorSnapshot || initial.Revision != 5 || initial.Snapshot["/sync/a"].Count != 1 {
		t.Fatalf("initial change=%+v", initial)
	}
	initial.Snapshot["/sync/a"].Labels["owner"] = "mutated"
	current, _, _ := mirror.Get("/sync/a")
	if current.Labels["owner"] != "one" {
		t.Fatalf("callback mutation leaked into mirror: %+v", current)
	}

	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{
		Key: "/sync/a", Value: `{"count":2,"labels":{"owner":"two"}}`, ModRevision: 6,
	}}
	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventDelete, KV: &fetcd.KV{Key: "/sync/a", ModRevision: 7}}
	put := receiveMirrorChange(t, changes)
	deleted := receiveMirrorChange(t, changes)
	if put.Type != fetcd.LocalMirrorPut || put.Key != "/sync/a" || put.Revision != 6 || put.Entry.Value.Count != 2 || put.Previous.Value.Count != 1 {
		t.Fatalf("put change=%+v", put)
	}
	if deleted.Type != fetcd.LocalMirrorDelete || deleted.Key != "/sync/a" || deleted.Revision != 7 || deleted.Entry != nil || deleted.Previous.Value.Count != 2 {
		t.Fatalf("delete change=%+v", deleted)
	}
}

func TestLocalMirrorSubscriptionReceivesResnapshot(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3, KVs: []*fetcd.KV{{
		Key: "/sync/old", Value: `{"count":1}`, ModRevision: 3,
	}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	changes := make(chan fetcd.LocalMirrorChange[mirrorTestRecord], 2)
	subscription, err := mirror.Subscribe(context.Background(), func(_ context.Context, change fetcd.LocalMirrorChange[mirrorTestRecord]) error {
		changes <- change
		return nil
	}, fetcd.LocalMirrorSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	_ = receiveMirrorChange(t, changes)

	client.setSnapshot(&fetcd.PrefixSnapshot{Revision: 10, KVs: []*fetcd.KV{{
		Key: "/sync/new", Value: `{"count":10}`, ModRevision: 10,
	}}})
	close(client.watcher(0).events)
	select {
	case <-client.watchRevisions:
	case <-time.After(time.Second):
		t.Fatal("mirror did not reconnect")
	}
	resnapshot := receiveMirrorChange(t, changes)
	if resnapshot.Type != fetcd.LocalMirrorSnapshot || resnapshot.Revision != 10 || len(resnapshot.Snapshot) != 1 || resnapshot.Snapshot["/sync/new"].Count != 10 {
		t.Fatalf("resnapshot=%+v", resnapshot)
	}
}

func TestLocalMirrorSlowSubscriptionIsIsolated(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	started := make(chan struct{})
	release := make(chan struct{})
	subscription, err := mirror.Subscribe(context.Background(), func(_ context.Context, _ fetcd.LocalMirrorChange[mirrorTestRecord]) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil
	}, fetcd.LocalMirrorSubscribeOptions{QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	watcher := client.watcher(0)
	watcher.events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: `{"count":1}`, ModRevision: 6}}
	waitMirrorValue(t, mirror, "/sync/a", 1)
	watcher.events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: `{"count":2}`, ModRevision: 7}}
	waitMirrorValue(t, mirror, "/sync/a", 2)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !errors.Is(subscription.Err(), fetcd.ErrMirrorSubscriberSlow) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(subscription.Err(), fetcd.ErrMirrorSubscriberSlow) {
		t.Fatal("subscriber queue did not report overflow")
	}
	close(release)
	select {
	case <-subscription.Done():
	case <-time.After(time.Second):
		t.Fatal("slow subscription did not terminate")
	}
	if !errors.Is(subscription.Err(), fetcd.ErrMirrorSubscriberSlow) {
		t.Fatalf("Err()=%v", subscription.Err())
	}
	if status := mirror.Status(); status.Revision != 7 {
		t.Fatalf("slow subscriber blocked mirror: %+v", status)
	}
}

func TestLocalMirrorSubscriptionContainsHandlerPanic(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 1})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	subscription, err := mirror.Subscribe(context.Background(), func(context.Context, fetcd.LocalMirrorChange[mirrorTestRecord]) error {
		panic("broken callback")
	}, fetcd.LocalMirrorSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-subscription.Done():
	case <-time.After(time.Second):
		t.Fatal("panicking subscription did not terminate")
	}
	if !errors.Is(subscription.Err(), fetcd.ErrWatchCallbackPanic) {
		t.Fatalf("Err()=%v", subscription.Err())
	}
	if status := mirror.Status(); status.Revision != 1 {
		t.Fatalf("callback panic affected mirror: %+v", status)
	}
}

func receiveMirrorChange(t *testing.T, changes <-chan fetcd.LocalMirrorChange[mirrorTestRecord]) fetcd.LocalMirrorChange[mirrorTestRecord] {
	t.Helper()
	select {
	case change := <-changes:
		return change
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mirror callback")
		return fetcd.LocalMirrorChange[mirrorTestRecord]{}
	}
}

func TestLocalMirrorResnapshotsAfterMalformedWatchValue(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{Key: "/sync/a", Value: `{"count":1}`, ModRevision: 5}}})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	awaitWatchStarted(t, client)
	client.setSnapshot(&fetcd.PrefixSnapshot{Revision: 7, KVs: []*fetcd.KV{{Key: "/sync/a", Value: `{"count":2}`, ModRevision: 7}}})
	client.watcher(0).events <- &fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: "not-json", ModRevision: 6}}
	select {
	case revision := <-client.watchRevisions:
		if revision != 8 {
			t.Fatalf("watch revision=%d, want 8", revision)
		}
	case <-time.After(time.Second):
		t.Fatal("mirror did not resnapshot after malformed event")
	}
	waitMirrorValue(t, mirror, "/sync/a", 2)
}

// awaitWatchStarted blocks until the mirror's watcher has registered, with an
// upper bound. A bare `<-client.watchRevisions` made a mirror that never
// started its watch fail as a go test timeout — ten minutes and a stack dump
// instead of a sentence — so every wait in this file is bounded, matching
// waitMirrorValue below.
func awaitWatchStarted(t *testing.T, client *mirrorTestClient) {
	t.Helper()
	select {
	case <-client.watchRevisions:
	case <-time.After(5 * time.Second):
		t.Fatal("mirror never started its etcd watch")
	}
}

func waitMirrorValue(t *testing.T, mirror fetcd.ILocalMirror[mirrorTestRecord], key string, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		value, ok, err := mirror.Get(key)
		if err == nil && ok && value.Count == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mirror never reached %s count=%d", key, count)
}

func waitMirrorMissing(t *testing.T, mirror fetcd.ILocalMirror[mirrorTestRecord], key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, ok, err := mirror.Get(key)
		if err == nil && !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mirror key %s remained present", key)
}
