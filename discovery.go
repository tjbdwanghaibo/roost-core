package etcd

import (
	"context"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	defaultDiscoveryRetryMinInterval = time.Second
	defaultDiscoveryRetryMaxInterval = 30 * time.Second
)

// discovery implements fetcd.IDiscovery.
type discovery struct {
	cli     *clientv3.Client
	prefix  string
	ttl     int64
	leaseID clientv3.LeaseID
	key     string // registered key

	mu               sync.Mutex
	stopping         bool
	keepaliveCancel  context.CancelFunc
	loopCancel       context.CancelFunc
	loopDone         chan struct{}
	registerOnce     func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error)
	revokeLease      func(context.Context, clientv3.LeaseID) error
	retryMinInterval time.Duration
	retryMaxInterval time.Duration
}

type discoveryRegistration struct {
	leaseID       clientv3.LeaseID
	key           string
	keepaliveDone <-chan struct{}
	cancel        context.CancelFunc
}

func newDiscovery(cli *clientv3.Client, prefix string, ttl int64) *discovery {
	d := &discovery{
		cli:              cli,
		prefix:           prefix,
		ttl:              ttl,
		retryMinInterval: defaultDiscoveryRetryMinInterval,
		retryMaxInterval: defaultDiscoveryRetryMaxInterval,
	}
	d.registerOnce = d.registerOnceWithEtcd
	d.revokeLease = d.revokeLeaseWithEtcd
	return d
}

func (d *discovery) Register(ctx context.Context, info *fetcd.ServiceInfo) error {
	d.markActive()
	if ctx == nil {
		ctx = context.Background()
	}
	reg, err := d.registerOnce(ctx, info)
	if err != nil {
		return err
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	d.setRegistration(reg)
	done := make(chan struct{})
	d.setLoop(cancel, done)
	go d.registrationLoop(loopCtx, info, reg, done)
	return nil
}

func (d *discovery) registerOnceWithEtcd(ctx context.Context, info *fetcd.ServiceInfo) (discoveryRegistration, error) {
	// Grant lease
	resp, err := d.cli.Grant(ctx, d.ttl)
	if err != nil {
		return discoveryRegistration{}, fmt.Errorf("etcd discovery: grant lease: %w", err)
	}

	// Build key and value
	key := fmt.Sprintf("%s%s/%d", d.prefix, info.ServiceType, info.Sid)
	value, err := json.Marshal(info)
	if err != nil {
		return discoveryRegistration{}, fmt.Errorf("etcd discovery: marshal info: %w", err)
	}

	// Put with lease
	_, err = d.cli.Put(ctx, key, string(value), clientv3.WithLease(resp.ID))
	if err != nil {
		return discoveryRegistration{}, fmt.Errorf("etcd discovery: put: %w", err)
	}

	// Start keepalive
	keepCtx, cancel := context.WithCancel(context.Background())
	ch, err := d.cli.KeepAlive(keepCtx, resp.ID)
	if err != nil {
		cancel()
		return discoveryRegistration{}, fmt.Errorf("etcd discovery: keepalive: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for range ch {
			// drain keepalive responses
		}
	}()

	return discoveryRegistration{leaseID: resp.ID, key: key, keepaliveDone: done, cancel: cancel}, nil
}

func (d *discovery) registrationLoop(ctx context.Context, info *fetcd.ServiceInfo, reg discoveryRegistration, done chan<- struct{}) {
	defer close(done)
	d.logRegistered(reg)
	for {
		select {
		case <-ctx.Done():
			slog.Debug("etcd discovery: keepalive stopped", "key", d.currentKey())
			return
		case <-reg.keepaliveDone:
		}
		if !d.shouldWarnLeaseLost() {
			slog.Debug("etcd discovery: keepalive stopped", "key", d.currentKey())
			return
		}
		slog.Warn("etcd discovery: lease lost", "key", d.currentKey())

		backoff := d.retryMinInterval
		for {
			if !d.waitRetry(ctx, backoff) {
				return
			}
			next, err := d.registerOnce(ctx, info)
			if err == nil {
				d.setCurrentRegistration(next)
				d.logRegistered(next)
				reg = next
				break
			}
			slog.Warn("etcd discovery: register retry failed", "key", d.currentKey(), "err", err, "backoff", backoff)
			backoff = d.nextBackoff(backoff)
		}
	}
}

func (d *discovery) logRegistered(reg discoveryRegistration) {
	slog.Info("etcd discovery: registered", "key", reg.key, "lease", reg.leaseID)
}

func (d *discovery) Deregister(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.markStopping()
	d.cancelLoop()
	d.cancelKeepalive()
	d.waitLoopDone()
	leaseID := d.currentLeaseID()
	if leaseID == 0 {
		return nil
	}
	// Revoke lease (automatically deletes all keys attached to it)
	err := d.revokeLease(ctx, leaseID)
	if err != nil {
		return fmt.Errorf("etcd discovery: revoke: %w", err)
	}
	slog.Info("etcd discovery: deregistered", "key", d.currentKey())
	d.clearRegistration()
	return nil
}

func (d *discovery) revokeLeaseWithEtcd(ctx context.Context, leaseID clientv3.LeaseID) error {
	if d.cli == nil {
		return nil
	}
	_, err := d.cli.Revoke(ctx, leaseID)
	return err
}

func (d *discovery) markActive() {
	d.mu.Lock()
	d.stopping = false
	d.mu.Unlock()
}

func (d *discovery) markStopping() {
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
}

func (d *discovery) shouldWarnLeaseLost() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.stopping
}

func (d *discovery) setRegistration(reg discoveryRegistration) {
	d.mu.Lock()
	d.leaseID = reg.leaseID
	d.key = reg.key
	d.keepaliveCancel = reg.cancel
	d.mu.Unlock()
}

func (d *discovery) setCurrentRegistration(reg discoveryRegistration) {
	d.mu.Lock()
	d.leaseID = reg.leaseID
	d.key = reg.key
	d.keepaliveCancel = reg.cancel
	d.mu.Unlock()
}

func (d *discovery) clearRegistration() {
	d.mu.Lock()
	d.leaseID = 0
	d.key = ""
	d.keepaliveCancel = nil
	d.mu.Unlock()
}

func (d *discovery) currentLeaseID() clientv3.LeaseID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leaseID
}

func (d *discovery) currentKey() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.key
}

func (d *discovery) setLoop(cancel context.CancelFunc, done chan struct{}) {
	d.mu.Lock()
	d.loopCancel = cancel
	d.loopDone = done
	d.mu.Unlock()
}

func (d *discovery) cancelLoop() {
	d.mu.Lock()
	cancel := d.loopCancel
	d.loopCancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *discovery) cancelKeepalive() {
	d.mu.Lock()
	cancel := d.keepaliveCancel
	d.keepaliveCancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *discovery) waitLoopDone() {
	d.mu.Lock()
	done := d.loopDone
	d.loopDone = nil
	d.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (d *discovery) waitRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = defaultDiscoveryRetryMinInterval
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (d *discovery) nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		cur = d.retryMinInterval
	}
	next := cur * 2
	max := d.retryMaxInterval
	if max <= 0 {
		max = defaultDiscoveryRetryMaxInterval
	}
	if next > max {
		return max
	}
	return next
}

func (d *discovery) Discover(ctx context.Context, serviceType string) ([]*fetcd.ServiceInfo, error) {
	prefix := d.prefix + serviceType + "/"
	resp, err := d.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	infos := make([]*fetcd.ServiceInfo, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		info := &fetcd.ServiceInfo{}
		if err := json.Unmarshal(kv.Value, info); err != nil {
			slog.Warn("etcd discovery: unmarshal failed", "key", string(kv.Key), "err", err)
			continue
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (d *discovery) WatchService(ctx context.Context, serviceType string) fetcd.IServiceWatcher {
	prefix := d.prefix + serviceType + "/"
	wch := d.cli.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithPrevKV())
	return newServiceWatcher(wch)
}

var _ fetcd.IDiscovery = (*discovery)(nil)

// serviceWatcher implements fetcd.IServiceWatcher.
type serviceWatcher struct {
	eventCh chan *fetcd.ServiceEvent
	cancel  context.CancelFunc
}

func newServiceWatcher(wch clientv3.WatchChan) *serviceWatcher {
	eventCh := make(chan *fetcd.ServiceEvent, 32)
	ctx, cancel := context.WithCancel(context.Background())
	sw := &serviceWatcher{eventCh: eventCh, cancel: cancel}
	go sw.loop(ctx, wch)
	return sw
}

func (sw *serviceWatcher) EventChan() <-chan *fetcd.ServiceEvent {
	return sw.eventCh
}

func (sw *serviceWatcher) Close() error {
	sw.cancel()
	return nil
}

func (sw *serviceWatcher) loop(ctx context.Context, wch clientv3.WatchChan) {
	defer close(sw.eventCh)
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-wch:
			if !ok {
				return
			}
			for _, ev := range resp.Events {
				event := &fetcd.ServiceEvent{}
				switch ev.Type {
				case mvccpb.PUT:
					event.Type = fetcd.EventPut
					info := &fetcd.ServiceInfo{}
					if err := json.Unmarshal(ev.Kv.Value, info); err == nil {
						event.Info = info
					}
				case mvccpb.DELETE:
					event.Type = fetcd.EventDelete
					// Try to decode from PrevKv
					if ev.PrevKv != nil {
						info := &fetcd.ServiceInfo{}
						if err := json.Unmarshal(ev.PrevKv.Value, info); err == nil {
							event.Info = info
						}
					}
				}
				if event.Info != nil {
					select {
					case sw.eventCh <- event:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

var _ fetcd.IServiceWatcher = (*serviceWatcher)(nil)
