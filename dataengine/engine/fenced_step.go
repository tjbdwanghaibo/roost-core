package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// 原生 saga 步骤（本地 mutation + lease fence receipt）的实体屏障与跳过后的驱逐（RR-20260926-30）。
//
// 这类记录准入 WAL 时 Nest 内存已经应用了它；投影时租约若已失效，整笔记录被跳过，内存却仍含其效果。
// 两条规则保证之后不会出现“以被跳过效果为基础”的 WAL 记录：
//
//  1. 屏障：记录准入后、投影结果确定前，写到同一实体的其他事务在 reserve（heldMu 下，与准入同一
//     临界区）以 coredata.ErrFencedEntityPending 拒绝。同一步骤的重投拿的是新 token、是另一条记录，
//     同样被挡；屏障解除后它再来即可。
//  2. 驱逐：记录被跳过（或 Store 报告不了结果）时，屏障保持到受影响的内存实体被驱逐（不持久化）为止。
//     之后的访问从 Mongo 重载，Sync 在重载后对原订阅者强制全量（entitysync.Manager.Rebind）。
//
// 屏障与记录的 entityProjection 同生命周期：投影成功、驱逐完成、discard、fatal/关闭（completeAllTickets）、
// WAL 写入结果未知（signalWhenDurable）都经 finishEntitiesLocked 解除，没有单独的释放路径可漏。
// 重启时不需要屏障：启动恢复在 Runtime 就绪、实体可加载之前排空 WAL，被跳过记录之后不会有同实体
// 依赖记录（本进程准入时已被屏障挡住），实体随后从 Mongo 读取。

// FenceOutcomeProjectionStore 是 ProjectionStore 的可选能力：投影单笔记录，并报告它是否因 lease fence
// 失效被整笔跳过。未实现它的 Store 投影完原生步骤后，Projector 按“可能被跳过”处理，驱逐受影响实体——
// 驱逐一个其实已落库的实体只多一次重载，留下被跳过的内存则会让后续事务 fatal。
type FenceOutcomeProjectionStore interface {
	ProjectionStore
	ProjectFenced(ctx context.Context, record coredata.CommitRecord) (skipped bool, err error)
}

type projectionOutcome uint8

const (
	projectionApplied projectionOutcome = iota
	projectionSkipped
	projectionOutcomeUnknown
)

// recordEntityIDs 是记录写到的完整 Entity ID；DAO 共用 Entity ID，trackEntitiesLocked 用同一规则。
func recordEntityIDs(record coredata.CommitRecord) []int64 {
	ids := make([]int64, 0, len(record.Mutations))
	for _, mutation := range record.Mutations {
		id := mutation.Key.ID
		if id == 0 {
			id = mutation.EntityID
		}
		if id != 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func hasLeaseFence(record coredata.CommitRecord) bool {
	for _, receipt := range record.Receipts {
		if receipt.Namespace == coredata.LeaseFenceReceiptNamespace {
			return true
		}
	}
	return false
}

// checkFencedEntitiesLocked 在 reserve 的 heldMu 临界区内判断：新记录写到的实体上是否有别的原生步骤
// 还没有确定结果。检查与登记在同一把锁下完成，不存在“查过之后才登记”的窗口。
func (p *Projector) checkFencedEntitiesLocked(record coredata.CommitRecord) error {
	if len(p.fencedEntities) == 0 {
		return nil
	}
	for _, id := range recordEntityIDs(record) {
		if owner, ok := p.fencedEntities[id]; ok && owner != record.ID {
			p.fencedRejected.Add(1)
			return fmt.Errorf("%w: entity %d waits for transaction %s", coredata.ErrFencedEntityPending, id, owner.String())
		}
	}
	return nil
}

// startStaleEviction 在记录投影完成后调用。原生步骤被跳过（或结果未知）时保留它的 entityProjection
// ——也就是保留屏障与冷加载等待——并交给驱逐 worker；返回 false 表示按普通投影完成处理。
func (p *Projector) startStaleEviction(id coredata.TransactionID, outcome projectionOutcome) bool {
	if outcome == projectionApplied {
		return false
	}
	p.heldMu.RLock()
	pending := p.pendingTransactions[id]
	fenced := pending != nil && pending.fenced
	var ids []int64
	if fenced {
		ids = append(ids, pending.ids...)
	}
	p.heldMu.RUnlock()
	if !fenced {
		// 不是本进程这次准入的原生步骤：启动恢复或 ack 丢失后的重放。前者没有常驻实体；后者第一次
		// 投影时已经驱逐过，之后常驻的实体都从 Mongo 加载，不含被跳过的效果。
		return false
	}
	metrics.IncCounter("dataengine.fence.evictions.started.total", nil, 1)
	p.evictMu.Lock()
	p.evictQueue = append(p.evictQueue, staleEviction{id: id, entities: ids})
	if !p.evictRunning {
		p.evictRunning = true
		p.evictWG.Add(1)
		go p.runEvictions()
	}
	p.evictMu.Unlock()
	return true
}

type staleEviction struct {
	id       coredata.TransactionID
	entities []int64
}

// runEvictions 一次只处理一笔：驱逐通过快池续行执行，同时最多一个由 DataEngine 发起，
// 队列空了就退出，不常驻 goroutine。它在快池之外，允许同步等待快池（nest.RunLocal 的契约）。
func (p *Projector) runEvictions() {
	defer p.evictWG.Done()
	for {
		p.evictMu.Lock()
		if len(p.evictQueue) == 0 {
			p.evictRunning = false
			p.evictMu.Unlock()
			return
		}
		item := p.evictQueue[0]
		p.evictQueue = p.evictQueue[1:]
		p.evictMu.Unlock()
		p.evictUntilDone(item)
	}
}

// evictUntilDone 重试到驱逐成功或 Projector 关闭。失败期间屏障保持：宁可让该实体的写入暂时
// 可重试地失败，也不在含被跳过效果的内存上继续接受事务。关闭时 completeAllTickets 统一解除。
func (p *Projector) evictUntilDone(item staleEviction) {
	backoff := p.opts.RetryMin
	for {
		if p.ctx.Err() != nil {
			return
		}
		var err error
		if p.evictEntities == nil {
			err = errors.New("dataengine projector: no entity evictor is configured")
		} else {
			err = p.evictEntities(p.ctx, item.entities)
		}
		if err == nil {
			p.staleEvictions.Add(1)
			p.completeProjection(item.id, nil)
			return
		}
		metrics.IncCounter("dataengine.fence.evictions.failed.total", nil, 1)
		slog.Warn("dataengine: evicting entities of a skipped lease-fenced step failed; the entities stay fenced",
			"transaction", item.id.String(), "entities", item.entities, "err", err)
		timer := time.NewTimer(jitterDuration(backoff))
		select {
		case <-timer.C:
		case <-p.ctx.Done():
			timer.Stop()
			return
		}
		backoff = min(backoff*2, p.opts.RetryMax)
	}
}

func (p *Projector) waitEvictions(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.evictWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BindLocalExecutor 由 Nest 构造时调用（corenest.LocalExecutorBinder），让驱逐在快池持锁执行。
// 未绑定时（独立使用 DataEngine、没有 Nest）按 entity.RunLocal 的约定就地执行。
func (p *Projector) BindLocalExecutor(run func(func()) error) {
	if p == nil || run == nil {
		return
	}
	p.localExecutor.Store(&run)
}

func (p *Projector) localRun() func(func()) error {
	if run := p.localExecutor.Load(); run != nil {
		return *run
	}
	return nil
}

// evictStaleEntities 驱逐被跳过的原生步骤留下的常驻实体：不持久化，只是忘掉这份内存。
// 驱逐在快池执行（EntityManager.Destroy 要取 Entity 锁，本 goroutine 不是快 worker）；
// 只驱逐开始时看到的那个对象——期间若已被卸载并重新加载，新对象来自 Mongo，不含被跳过的效果。
func (runtime *Runtime) evictStaleEntities(ctx context.Context, ids []int64) error {
	var errs []error
	for _, id := range ids {
		resident := runtime.access.Manager().Get(id)
		if resident == nil {
			continue
		}
		// 与 Remote 持久拒绝后的卸载（RR-20260926-39）同一入口：ManagerAccess.Unload 只卸载内存、
		// 已被别的路径卸载或替换时返回 nil。
		var destroyErr error
		runErr := entity.RunLocal(entity.WithLocalExecutor(ctx, runtime.Projector.localRun()), func() {
			destroyErr = runtime.access.Unload(ctx, resident)
		})
		if err := errors.Join(runErr, destroyErr); err != nil {
			errs = append(errs, fmt.Errorf("entity %d: %w", id, err))
			continue
		}
		slog.Info("dataengine: evicted an entity whose lease-fenced step was skipped", "entity", id)
	}
	return errors.Join(errs...)
}

var _ FenceOutcomeProjectionStore = (*MongoStore)(nil)
var _ corenest.LocalExecutorBinder = (*Projector)(nil)
