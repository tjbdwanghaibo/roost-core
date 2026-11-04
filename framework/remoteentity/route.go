package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 路由登记挂在 Manager 的生命周期上（联盟服务方案 §2.7）。登记的是“本进程持有、已加载、独占”的登记 kind 实体
// （Config.RouteKinds，由声明了 service= 的 Remote 托管 kind 经 ModOption 传入）：
//
//   - 登记：写准入（beginWrite，覆盖冷加载后的第一次写与首写认领）、ClaimRemoteOwnership、路由 RPC 的服务端前置检查
//     （ServeRoutedRequest 冷加载之后）与起服预加载（PreloadOwnedEntities）。同一个值只写一次 Redis，之后由续期循环续。
//   - 删除：转移出去（TransferRemoteOwnership 进入 Draining 之后）、写准入发现所有权已是别人（fence）、正常停机。
//   - 续期：每 RouteRenewInterval 对登记过的实体批量 pipeline 续期；“值不同”就不再续，“不存在”先在实体的写门下向
//     权威确认所有权仍是本 sid（独占）再登记。
//
// 所有登记与删除都在实体的写门下做（续期的“不存在”分支也先取写门），与所有权转换串行：转移持有写门直到结束，
// 所以不会出现“转移刚删掉登记，并发的登记又把旧代际写回去、续期循环再把它续下去”。Redis 失败只记指标与日志，不影响
// 写准入（准入看的是所有权记录与 fence）；登记失败的实体仍记在 held 里，下一轮续期得到“不存在”时重新登记。

// routeRegistry 是 Manager 的路由登记状态。nil 表示没有登记 kind（Config.RouteKinds 为空），全部入口无操作。
type routeRegistry struct {
	store    routeStore
	kinds    map[entity.EntityKind]struct{}
	interval time.Duration
	batch    int

	mu sync.Mutex
	// held 是本进程登记过（或登记失败、等续期重试）的实体 → 登记值。
	held map[int64]string
	// 续期循环：Start 时启动一次，Stop 时停；stopped 之后不再启动。
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

func newRouteRegistry(store routeStore, cfg *Config) *routeRegistry {
	if store == nil || cfg == nil || len(cfg.RouteKinds) == 0 {
		return nil
	}
	r := &routeRegistry{
		store:    store,
		kinds:    make(map[entity.EntityKind]struct{}, len(cfg.RouteKinds)),
		interval: cfg.RouteRenewInterval,
		batch:    cfg.RouteRenewBatch,
		held:     make(map[int64]string),
	}
	if r.interval <= 0 {
		r.interval = DefaultRouteRenewInterval
	}
	if r.batch <= 0 {
		r.batch = DefaultRouteRenewBatch
	}
	for _, kind := range cfg.RouteKinds {
		r.kinds[kind] = struct{}{}
	}
	return r
}

func (r *routeRegistry) routed(kind entity.EntityKind) bool {
	if r == nil {
		return false
	}
	_, ok := r.kinds[kind]
	return ok
}

func (r *routeRegistry) heldValue(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[id]
}

// RouteHeld 报告本进程是否持有 id 的路由登记（测试与诊断用）。
func (m *Manager) RouteHeld(id int64) bool {
	if m == nil || m.routes == nil {
		return false
	}
	return m.routes.heldValue(id) != ""
}

func routeMetric(name, result string) {
	metrics.IncCounter(name, metrics.Labels{"result": result}, 1)
}

// registerRoute 在 lease 表示本 sid 独占时登记 id 的路由。调用方持有实体的写门。已登记同一个值时不碰 Redis。
func (m *Manager) registerRoute(ctx context.Context, id int64, kind entity.EntityKind, lease entity.RemoteEntityMarkerLease) {
	r := m.routes
	if !r.routed(kind) || lease.Shared || lease.OwnerSid != m.localSid || lease.MarkerEpoch == 0 {
		return
	}
	value := RouteValue(m.localSid, lease.MarkerEpoch)
	if r.heldValue(id) == value {
		return
	}
	written, err := r.store.Register(ctx, id, m.localSid, lease.MarkerEpoch)
	switch {
	case err != nil:
		// 登记失败不影响写准入；仍记在 held 里，续期得到“不存在”时重新登记。
		routeMetric("remote_entity.route_register_total", "error")
		slog.Warn("remote_entity: route registration failed; callers fall back to the ownership record", "entity", id, "err", err)
	case !written:
		// Redis 里已有更新代际的登记：本进程是迟到的旧 owner，不登记、不续期。
		routeMetric("remote_entity.route_register_total", "stale")
		r.mu.Lock()
		delete(r.held, id)
		r.mu.Unlock()
		return
	default:
		routeMetric("remote_entity.route_register_total", "ok")
	}
	r.mu.Lock()
	r.held[id] = value
	r.mu.Unlock()
}

// dropRoute 不再持有 id 的登记，并按值删除 Redis 里的那一条。调用方持有实体的写门。value 是本进程没登记过（例如同
// sid 重启后还没登记就转移出去）时要删的值；held 里有记录时以 held 为准。
func (m *Manager) dropRoute(ctx context.Context, id int64, value string) {
	r := m.routes
	if !r.routed(entity.ResolveEntityID(id).Kind) {
		return
	}
	r.mu.Lock()
	if held := r.held[id]; held != "" {
		value = held
	}
	delete(r.held, id)
	r.mu.Unlock()
	if value == "" {
		return
	}
	if err := r.store.Delete(ctx, []RouteEntry{{ID: id, Value: value}}); err != nil {
		routeMetric("remote_entity.route_delete_total", "error")
		slog.Warn("remote_entity: route deletion failed; the registration expires by its TTL", "entity", id, "err", err)
		return
	}
	routeMetric("remote_entity.route_delete_total", "ok")
}

// withEntityGate 在实体的写门与所有权读锁下运行 fn（与写准入、所有权转换串行），整个过程受 OpTimeout 约束。
func (m *Manager) withEntityGate(parent context.Context, id int64, fn func(context.Context, *remoteEntityWrapper) error) error {
	meta := entity.ResolveEntityID(id)
	if meta.FullID == 0 || !entity.IsEntityKindRemoteManaged(meta.Kind) {
		return fmt.Errorf("%w: invalid entity %d", entity.ErrRemoteRejected, id)
	}
	ctx, cancel := m.ownershipContext(parent)
	defer cancel()
	wrapper := m.getOrCreate(meta.FullID, meta.Category, meta.Kind)
	if wrapper == nil {
		return entity.ErrRemoteOverloaded
	}
	defer wrapper.release()
	if err := wrapper.acquireWriteGate(ctx); err != nil {
		return err
	}
	wrapper.ownershipMu.RLock()
	defer wrapper.releaseWriteGate()
	return fn(ctx, wrapper)
}

// routedOwnership 是前置检查、预加载与续期重登记共用的一步：向权威确认所有权，owner 是本 sid（独占）就保证实体已
// 加载并登记路由。调用方持有写门。返回权威里的 owner（没有记录时 found=false）与是否由本进程服务。
func (m *Manager) routedOwnership(ctx context.Context, w *remoteEntityWrapper) (owner int32, found bool, err error) {
	if err := w.refreshMarked(ctx); err != nil {
		return 0, false, fmt.Errorf("remote_entity: read ownership %d: %w", w.id, err)
	}
	lease, found := w.cachedOwnership()
	if !found {
		return 0, false, nil
	}
	if lease.OwnerSid != m.localSid {
		// 所有权已是别人：本进程不再持有它的登记（只删自己写的值）。
		m.dropRoute(ctx, w.id, "")
		return lease.OwnerSid, true, nil
	}
	if lease.Shared {
		return lease.OwnerSid, true, nil
	}
	if w.lookupLocalEntity() == nil {
		loaded, err := w.loadEntity(ctx)
		if err != nil {
			return lease.OwnerSid, true, fmt.Errorf("remote_entity: load entity %d: %w", w.id, err)
		}
		if loaded == nil {
			return lease.OwnerSid, true, fmt.Errorf("%w: entity %d has an ownership record but no data", entity.ErrRemoteRejected, w.id)
		}
		w.attachEntity(loaded)
	}
	m.registerRoute(ctx, w.id, w.kind, lease)
	return lease.OwnerSid, true, nil
}

// ServeRoutedRequest 是路由 RPC 服务端前置检查的框架一步（wiring/remoteentity.RPCRouteCheck 调它，联盟服务方案 §2.3）：
//
//   - 实体的路由由本进程持有（已加载、独占、已登记）：直接返回本 sid，不做 I/O；
//   - 否则在写门下读持久所有权记录：没有记录 found=false（非创建类请求不得认领不存在的实体）；owner 是别人返回它
//     （调用方答改投）；owner 是本 sid 就冷加载、登记路由，再返回本 sid。
//
// 进 Nest 之后写准入照常按所有权记录 + fence 裁决，这一步只是省掉一次白进 Nest。归属于别的服务的 kind 返回
// ErrRemoteKindNotOwned。要 I/O，只能在 I/O 段（RPC handler）调用。
func (m *Manager) ServeRoutedRequest(ctx context.Context, id int64) (owner int32, found bool, err error) {
	if err := fctx.BlockingError("remoteentity.ServeRoutedRequest"); err != nil {
		return 0, false, err
	}
	if m == nil || m.ownershipStore == nil || m.cfg == nil {
		return 0, false, entity.ErrRemoteWriteCapabilityDisabled
	}
	meta := entity.ResolveEntityID(id)
	if meta.FullID == 0 || !entity.IsEntityKindRemoteManaged(meta.Kind) {
		return 0, false, fmt.Errorf("%w: invalid entity %d", entity.ErrRemoteRejected, id)
	}
	if err := m.rejectForeignKind(meta.FullID, meta.Kind); err != nil {
		return 0, false, err
	}
	if m.routes.routed(meta.Kind) && m.routes.heldValue(meta.FullID) != "" {
		return m.localSid, true, nil
	}
	err = m.withEntityGate(ctx, meta.FullID, func(ctx context.Context, w *remoteEntityWrapper) error {
		var serveErr error
		owner, found, serveErr = m.routedOwnership(ctx, w)
		return serveErr
	})
	return owner, found, err
}

// PreloadKinds 返回配置的起服预加载 kind（Config.PreloadKinds 的副本）。
func (m *Manager) PreloadKinds() []entity.EntityKind {
	if m == nil || m.cfg == nil {
		return nil
	}
	return append([]entity.EntityKind(nil), m.cfg.PreloadKinds...)
}

// PreloadReport 是一次起服预加载的结果。
type PreloadReport struct {
	// Listed 是所有权记录里 owner 是本 sid、kind 在列表里的实体数。
	Listed int
	// Loaded 是加载并登记了的实体数。
	Loaded int
	// Skipped 是枚举之后所有权已变（转走或进了共享模式）的实体数。
	Skipped int
	// Failed 是加载失败、留给按需冷加载的实体数（错误已记日志与指标）。
	Failed int
}

const (
	preloadPage        = 1000
	preloadConcurrency = 16
)

// PreloadOwnedEntities 是起服预加载（实体类型的显式策略 preload=startup，联盟服务方案 §2.7）：按 owner 枚举持久所有权
// 记录里 owner 是本 sid、kind 在 kinds 里的独占实体，只加载、不写，加载完成后登记路由。
//
// 枚举（所有权记录查询）失败返回错误，调用方让启动失败，由进程管理拉起重试——与 DataEngine 恢复失败一样。单个实体
// 加载失败只记错误与指标、跳过它：它留给按需冷加载，到时同样会报错并答给调用方，不让一个坏文档挡住整个服务。
// 加载并发上限 16。要在 DataEngine 恢复与 Nest 启动之后、路由 RPC 的 owner 在总线上注册 handler 之前调用
// （wiring/remoteentity.RemotePreloadMod）。
func (m *Manager) PreloadOwnedEntities(ctx context.Context, kinds []entity.EntityKind) (PreloadReport, error) {
	var report PreloadReport
	if len(kinds) == 0 {
		return report, nil
	}
	if m == nil || m.ownershipStore == nil || m.cfg == nil || m.backend == nil {
		return report, entity.ErrRemoteWriteCapabilityDisabled
	}
	lister, ok := m.ownershipStore.(OwnedEntityLister)
	if !ok {
		return report, fmt.Errorf("remote_entity: ownership store %T cannot enumerate owned entities for preload", m.ownershipStore)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, preloadConcurrency)
	)
	count := func(field *int) {
		mu.Lock()
		*field++
		mu.Unlock()
	}
	after := int64(0)
	for {
		ids, next, err := lister.ListOwnedRemoteEntities(ctx, m.localSid, kinds, after, preloadPage)
		if err != nil {
			wg.Wait()
			return report, fmt.Errorf("remote_entity: enumerate owned entities for preload: %w", err)
		}
		for _, id := range ids {
			count(&report.Listed)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return report, ctx.Err()
			}
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				defer func() { <-sem }()
				var owner int32
				var found bool
				err := m.withEntityGate(ctx, id, func(ctx context.Context, w *remoteEntityWrapper) error {
					var loadErr error
					owner, found, loadErr = m.routedOwnership(ctx, w)
					return loadErr
				})
				switch {
				case err != nil:
					count(&report.Failed)
					routeMetric("remote_entity.preload_total", "error")
					slog.Error("remote_entity: preload failed; the entity is left to on-demand loading", "entity", id, "err", err)
				case !found || owner != m.localSid:
					count(&report.Skipped)
					routeMetric("remote_entity.preload_total", "skipped")
				default:
					count(&report.Loaded)
					routeMetric("remote_entity.preload_total", "ok")
				}
			}(id)
		}
		if next == 0 {
			break
		}
		after = next
	}
	wg.Wait()
	return report, nil
}

// startRouteRenewal 启动续期循环（Assembly.Start 调用）；已启动或已停止时无操作。
func (m *Manager) startRouteRenewal() {
	r := m.routes
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil || r.stopped {
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go m.runRouteRenewal(loopCtx, r.interval, r.done)
}

func (m *Manager) runRouteRenewal(ctx context.Context, interval time.Duration, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.renewRoutes(ctx)
		}
	}
}

// renewRoutes 续期一遍：按批 pipeline 执行续期脚本。“值不同”不再续；“不存在”在写门下向权威确认后重新登记；整批出错
// （Redis 不可用）只记指标，下一轮再试。
func (m *Manager) renewRoutes(ctx context.Context) {
	r := m.routes
	if r == nil {
		return
	}
	r.mu.Lock()
	entries := make([]RouteEntry, 0, len(r.held))
	for id, value := range r.held {
		entries = append(entries, RouteEntry{ID: id, Value: value})
	}
	r.mu.Unlock()
	var missing []int64
	for start := 0; start < len(entries); start += r.batch {
		batch := entries[start:min(start+r.batch, len(entries))]
		batchCtx, cancel := context.WithTimeout(ctx, m.cfg.OpTimeout)
		results, err := r.store.Renew(batchCtx, batch)
		cancel()
		if err != nil || len(results) != len(batch) {
			routeMetric("remote_entity.route_renew_total", "error")
			if ctx.Err() == nil {
				slog.Warn("remote_entity: route renewal failed; registrations may expire and callers fall back to the ownership record",
					"entities", len(batch), "err", err)
			}
			continue
		}
		for i, result := range results {
			switch result {
			case RouteRenewed:
				routeMetric("remote_entity.route_renew_total", "renewed")
			case RouteReplaced:
				routeMetric("remote_entity.route_renew_total", "replaced")
				r.mu.Lock()
				if r.held[batch[i].ID] == batch[i].Value {
					delete(r.held, batch[i].ID)
				}
				r.mu.Unlock()
			default:
				routeMetric("remote_entity.route_renew_total", "missing")
				missing = append(missing, batch[i].ID)
			}
		}
	}
	for _, id := range missing {
		if ctx.Err() != nil {
			return
		}
		m.reregisterRoute(ctx, id)
	}
}

// reregisterRoute 处理续期的“不存在”：在写门下向权威确认所有权，仍是本 sid（独占）才重新登记；否则不再持有。
func (m *Manager) reregisterRoute(ctx context.Context, id int64) {
	r := m.routes
	err := m.withEntityGate(ctx, id, func(ctx context.Context, w *remoteEntityWrapper) error {
		before := r.heldValue(id)
		if before == "" {
			return nil // 期间已被转移或 fence 撤销
		}
		if err := w.refreshMarked(ctx); err != nil {
			return err
		}
		lease, found := w.cachedOwnership()
		r.mu.Lock()
		delete(r.held, id)
		r.mu.Unlock()
		if found && lease.OwnerSid == m.localSid && !lease.Shared {
			m.registerRoute(ctx, id, w.kind, lease)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		slog.Warn("remote_entity: could not confirm ownership to re-register a missing route; retrying next round", "entity", id, "err", err)
	}
}

// stopRouteRenewal 停续期循环并按值删除本进程持有的全部登记（正常停机）。等循环退出受 ctx 约束，超时返回 ctx 错误、
// 之后再调会接着等；删除失败只记日志（登记按 TTL 过期）。
func (m *Manager) stopRouteRenewal(ctx context.Context) error {
	r := m.routes
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.stopped = true
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	entries := make([]RouteEntry, 0, len(r.held))
	for id, value := range r.held {
		entries = append(entries, RouteEntry{ID: id, Value: value})
	}
	r.held = make(map[int64]string)
	r.mu.Unlock()
	var errs []error
	for start := 0; start < len(entries); start += r.batch {
		if err := r.store.Delete(ctx, entries[start:min(start+r.batch, len(entries))]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		routeMetric("remote_entity.route_delete_total", "error")
		slog.Warn("remote_entity: could not delete route registrations on shutdown; they expire by their TTL", "entities", len(entries), "err", err)
	}
	return nil
}
