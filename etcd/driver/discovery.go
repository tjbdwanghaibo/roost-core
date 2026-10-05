package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	"log/slog"
	"sync"
	"time"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultDiscoveryRetryMinInterval = time.Second
	defaultDiscoveryRetryMaxInterval = 30 * time.Second
)

// Discovery implements fetcd.IDiscovery.
//
// 键是 prefix + serviceType + "/" + sid，prefix 自己要带结尾的 "/"（这里不补）。注册只做地址 /
// 元数据发现，etcd 租约过期 / 重新注册不代表进程存活与否：进程级存活以 App 单实例锁的 Live 为准
// （docs/feature/APP-SINGLETON-LOCK-2026-10-05.md §12）。
type Discovery struct {
	cli     *clientv3.Client
	prefix  string
	ttl     int64
	leaseID clientv3.LeaseID
	key     string // registered key

	mu               sync.Mutex
	lifecycleMu      sync.Mutex
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

// SetRetryIntervals bounds the registration retry backoff. Zero keeps the
// default; the Mod reads both values from configuration.
func (d *Discovery) SetRetryIntervals(minInterval, maxInterval time.Duration) {
	if d == nil {
		return
	}
	if minInterval > 0 {
		d.retryMinInterval = minInterval
	}
	if maxInterval > 0 {
		d.retryMaxInterval = maxInterval
	}
}

func NewDiscovery(cli *clientv3.Client, prefix string, ttl int64) *Discovery {
	d := &Discovery{
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

func (d *Discovery) Register(ctx context.Context, info *fetcd.ServiceInfo) error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.hasRegistration() {
		return fmt.Errorf("etcd Discovery: service is already registered")
	}
	d.markActive()
	if ctx == nil {
		ctx = context.Background()
	}
	reg, err := d.registerOnce(ctx, info)
	if err != nil {
		return err
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if !d.setRegistration(reg, cancel, done) {
		cancel()
		reg.cancel()
		<-reg.keepaliveDone
		if err := d.revoke(ctx, reg.leaseID); err != nil {
			return fmt.Errorf("etcd Discovery: revoke cancelled registration: %w", err)
		}
		return context.Canceled
	}
	go d.registrationLoop(loopCtx, info, reg, done)
	return nil
}

func (d *Discovery) registerOnceWithEtcd(ctx context.Context, info *fetcd.ServiceInfo) (discoveryRegistration, error) {
	// Grant lease
	resp, err := d.cli.Grant(ctx, d.ttl)
	if err != nil {
		return discoveryRegistration{}, fmt.Errorf("etcd Discovery: grant lease: %w", err)
	}

	// Build key and value
	key := fmt.Sprintf("%s%s/%d", d.prefix, info.ServiceType, info.Sid)
	value, err := json.Marshal(info)
	if err != nil {
		return discoveryRegistration{}, fmt.Errorf("etcd Discovery: marshal info: %w", err)
	}

	// Put with lease
	_, err = d.cli.Put(ctx, key, string(value), clientv3.WithLease(resp.ID))
	if err != nil {
		return discoveryRegistration{}, errors.Join(
			fmt.Errorf("etcd Discovery: put: %w", err),
			d.revokeSetupLease(resp.ID),
		)
	}

	// Start keepalive
	keepCtx, cancel := context.WithCancel(context.Background())
	ch, err := d.cli.KeepAlive(keepCtx, resp.ID)
	if err != nil {
		cancel()
		return discoveryRegistration{}, errors.Join(
			fmt.Errorf("etcd Discovery: keepalive: %w", err),
			d.revokeSetupLease(resp.ID),
		)
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

func (d *Discovery) registrationLoop(ctx context.Context, info *fetcd.ServiceInfo, reg discoveryRegistration, done chan<- struct{}) {
	defer close(done)
	d.logRegistered(reg)
	for {
		select {
		case <-ctx.Done():
			slog.Debug("etcd Discovery: keepalive stopped", "key", d.currentKey())
			return
		case <-reg.keepaliveDone:
		}
		if !d.shouldWarnLeaseLost() {
			slog.Debug("etcd Discovery: keepalive stopped", "key", d.currentKey())
			return
		}
		slog.Warn("etcd Discovery: lease lost", "key", d.currentKey())

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
			slog.Warn("etcd Discovery: register retry failed", "key", d.currentKey(), "err", err, "backoff", backoff)
			backoff = d.nextBackoff(backoff)
		}
	}
}

func (d *Discovery) logRegistered(reg discoveryRegistration) {
	slog.Info("etcd Discovery: registered", "key", reg.key, "lease", reg.leaseID)
}

func (d *Discovery) Deregister(ctx context.Context) error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	d.markStopping()
	d.cancelLoop()
	d.cancelKeepalive()
	d.waitLoopDone()
	// lease lost 之后的重注册可能在上面取消 keepalive 之后才完成、装上新的登记（registrationLoop
	// 先 setCurrentRegistration 再看 ctx）。循环已退出，这里再取消一次，确保停的是当前登记的 keepalive。
	d.cancelKeepalive()
	leaseID := d.currentLeaseID()
	if leaseID == 0 {
		return nil
	}

	// 撤销当前租约，etcd 随之删除挂在它上面的键。租约已不存在（暂停超过 lease_ttl 后过期、重注册
	// 尚未成功）时 revoke 返回 nil：键已随租约删除，注销已经达成。
	if err := d.revoke(ctx, leaseID); err != nil {
		return fmt.Errorf("etcd Discovery: revoke: %w", err)
	}
	slog.Info("etcd Discovery: deregistered", "key", d.currentKey())
	d.clearRegistration()
	return nil
}

// revoke 撤销 leaseID。租约在 etcd 一侧已不存在（过期或已被撤销）时挂在它上面的键也已删除，
// 撤销的目的已经达成，记 Info 并返回 nil；其他错误原样返回（保留 errors.Is）。
func (d *Discovery) revoke(ctx context.Context, leaseID clientv3.LeaseID) error {
	err := d.revokeLease(ctx, leaseID)
	if err != nil && isLeaseNotFound(err) {
		slog.Info("etcd Discovery: lease already gone; its keys were removed with it", "lease", leaseID, "err", err)
		return nil
	}
	return err
}

// isLeaseNotFound 判断 etcd 的 “requested lease not found”：clientv3 把服务端的 gRPC 状态转换成
// rpctypes.ErrLeaseNotFound；未经转换的 gRPC 状态按 code 与 etcd 的错误描述判定。status.FromError
// 对被包裹的状态会把描述换成整条错误文本，所以逐层展开比较。
func isLeaseNotFound(err error) bool {
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return true
	}
	want := rpctypes.ErrorDesc(rpctypes.ErrGRPCLeaseNotFound)
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := status.FromError(e); ok && s.Code() == codes.NotFound && s.Message() == want {
			return true
		}
	}
	return false
}

func (d *Discovery) revokeSetupLease(leaseID clientv3.LeaseID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.revoke(ctx, leaseID); err != nil {
		return fmt.Errorf("etcd Discovery: revoke setup lease: %w", err)
	}
	return nil
}

func (d *Discovery) revokeLeaseWithEtcd(ctx context.Context, leaseID clientv3.LeaseID) error {
	if d.cli == nil {
		return nil
	}
	_, err := d.cli.Revoke(ctx, leaseID)
	return err
}

func (d *Discovery) markActive() {
	d.mu.Lock()
	d.stopping = false
	d.mu.Unlock()
}

func (d *Discovery) markStopping() {
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
}

func (d *Discovery) shouldWarnLeaseLost() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.stopping
}

func (d *Discovery) setRegistration(reg discoveryRegistration, loopCancel context.CancelFunc, loopDone chan struct{}) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return false
	}
	d.leaseID = reg.leaseID
	d.key = reg.key
	d.keepaliveCancel = reg.cancel
	d.loopCancel = loopCancel
	d.loopDone = loopDone
	return true
}

func (d *Discovery) hasRegistration() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leaseID != 0 || d.loopDone != nil
}

func (d *Discovery) setCurrentRegistration(reg discoveryRegistration) {
	d.mu.Lock()
	d.leaseID = reg.leaseID
	d.key = reg.key
	d.keepaliveCancel = reg.cancel
	d.mu.Unlock()
}

func (d *Discovery) clearRegistration() {
	d.mu.Lock()
	d.leaseID = 0
	d.key = ""
	d.keepaliveCancel = nil
	d.mu.Unlock()
}

func (d *Discovery) currentLeaseID() clientv3.LeaseID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leaseID
}

func (d *Discovery) currentKey() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.key
}

func (d *Discovery) cancelLoop() {
	d.mu.Lock()
	cancel := d.loopCancel
	d.loopCancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *Discovery) cancelKeepalive() {
	d.mu.Lock()
	cancel := d.keepaliveCancel
	d.keepaliveCancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *Discovery) waitLoopDone() {
	d.mu.Lock()
	done := d.loopDone
	d.loopDone = nil
	d.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (d *Discovery) waitRetry(ctx context.Context, delay time.Duration) bool {
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

func (d *Discovery) nextBackoff(cur time.Duration) time.Duration {
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

func (d *Discovery) Discover(ctx context.Context, serviceType string) ([]*fetcd.ServiceInfo, error) {
	prefix := d.prefix + serviceType + "/"
	resp, err := d.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	infos := make([]*fetcd.ServiceInfo, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		info := &fetcd.ServiceInfo{}
		if err := json.Unmarshal(kv.Value, info); err != nil {
			slog.Warn("etcd Discovery: unmarshal failed", "key", string(kv.Key), "err", err)
			continue
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (d *Discovery) WatchService(ctx context.Context, serviceType string) fetcd.IServiceWatcher {
	if ctx == nil {
		ctx = context.Background()
	}
	prefix := d.prefix + serviceType + "/"
	watchCtx, cancel := context.WithCancel(ctx)
	wch := d.cli.Watch(watchCtx, prefix, clientv3.WithPrefix(), clientv3.WithPrevKV())
	return newServiceWatcher(watchCtx, wch, cancel)
}

var _ fetcd.IDiscovery = (*Discovery)(nil)

// serviceWatcher implements fetcd.IServiceWatcher.
type serviceWatcher struct {
	eventCh chan *fetcd.ServiceEvent
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	errMu   sync.RWMutex
	err     error
}

func newServiceWatcher(ctx context.Context, wch clientv3.WatchChan, cancel context.CancelFunc) *serviceWatcher {
	eventCh := make(chan *fetcd.ServiceEvent, 32)
	if ctx == nil || cancel == nil {
		ctx, cancel = context.WithCancel(context.Background())
	}
	sw := &serviceWatcher{eventCh: eventCh, cancel: cancel, done: make(chan struct{})}
	go sw.loop(ctx, wch)
	return sw
}

func (sw *serviceWatcher) EventChan() <-chan *fetcd.ServiceEvent {
	return sw.eventCh
}

func (sw *serviceWatcher) Close() error {
	sw.once.Do(func() {
		sw.cancel()
		<-sw.done
	})
	return nil
}

func (sw *serviceWatcher) Done() <-chan struct{} { return sw.done }

func (sw *serviceWatcher) Err() error {
	if sw == nil {
		return nil
	}
	sw.errMu.RLock()
	defer sw.errMu.RUnlock()
	return sw.err
}

func (sw *serviceWatcher) setErr(err error) {
	if sw == nil || err == nil {
		return
	}
	sw.errMu.Lock()
	if sw.err == nil {
		sw.err = err
	}
	sw.errMu.Unlock()
}

func (sw *serviceWatcher) loop(ctx context.Context, wch clientv3.WatchChan) {
	defer close(sw.done)
	defer close(sw.eventCh)
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-wch:
			if !ok {
				if ctx.Err() == nil {
					sw.setErr(errors.New("etcd Discovery: watch channel closed unexpectedly"))
				}
				return
			}
			if err := resp.Err(); err != nil {
				if ctx.Err() == nil {
					sw.setErr(fmt.Errorf("etcd Discovery: watch failed: %w", err))
				}
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
