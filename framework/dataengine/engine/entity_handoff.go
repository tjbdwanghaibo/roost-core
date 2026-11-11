package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 交出实体（单个玩家换服的源服一侧，docs/maintenance/FEATURE-2026-11-11-player-rehome.md §3.2）。
//
// 把一个实体的写权交给另一个进程之前，本进程要做到两件事：自己写过的都已经在库里、而且不会再被重放；从此不再写它。
// HandOffEntity 的前半段就是运维卸载（entity_unload.go，屏障、排空、卸载都用它的同一套入口），差别有三处：
//
//  1. 实体不在内存里也立屏障。运维卸载对不常驻的实体什么也不做（下一次访问本来就冷加载）；交出要挡住的正是
//     “之后被冷加载回来再写”。
//  2. 排空之后还要等 WAL 的 checkpoint 盖过这个实体的记录。投影完成（WaitEntityProjection 放行）先于 checkpoint：
//     单 mutation 快路记录不写事务标记，重放只靠文档上的 _version 与 _last_tx 认出“已应用”；下一个写者推进文档之后
//     两者都对不上，重放得到 fatal 的 ErrProjectionConflict——本进程下一轮回放 fail-stop，重启后启动排空失败、起不来
//     （TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument）。等到 checkpoint 之后，本进程的 WAL 里
//     不再有这个实体的记录可重放。
//  3. 屏障不在返回时撤销，保持到 Release 或进程退出。屏障只在内存里，不新增持久状态：进程重启后它不存在，
//     “重启后还该不该写这个实体”由调用方的权威（换服是 account 的绑定）在准入时回答。
//
// 步骤（1、4 在业务池，2、3 在调用方 goroutine 上等待）：
//
//  1. 进行中屏障：与运维卸载同一张表（beginEntityUnload）。常驻时先取实体锁（等持有它的事务结束），不常驻时直接登记。
//     这一段写到该实体的事务拿到可重试的 coredata.ErrEntityUnloading：交出还可能失败、撤销。它同时让交出、Release、
//     运维卸载在同一个实体上互斥（后来者 ErrEntityUnloadBusy）。
//  2. 排空：等屏障之前已准入、写到该实体的记录投影完成。
//  3. 等 checkpoint：见 waitProjectedRecordsConfirmed。
//  4. 卸载此刻常驻的实例（ManagerAccess.Unload）。
//  5. 登记交出屏障，再撤进行中屏障（两张表有重叠，中间没有空档）。之后写到该实体的记录在 WAL 准入处以
//     ErrEntityHandedOff 拒绝，Nest 在 Guard 内回滚；新建、删除它的记录走同一处准入，同样被拒。
//
// 第 1～4 步任何一步失败都是确定的“没有交出”：屏障撤销，实体与正常运行时一样（第 4 步之前失败时仍常驻），沿用运维
// 卸载的错误（ErrEntityUnloadBusy / ErrEntityUnloadTimeout / ErrEntityUnloadNotPersistent）。
//
// 交出之后实体仍然可以被加载（读不受限）：仍有 Sync 订阅者时卸载后重载（unload_resync）会把它读回来，任何一次访问
// 也会冷加载它。这样的实例是只读的，而且在下一个写者写过之后落后于库；所以调用方应当先断开会话、离开场景、退订，
// 再交出。Release 因此不只是撤屏障：它先卸载此刻常驻的实例，否则屏障一撤，落后的实例上的第一笔写入就是版本冲突
// （fatal）。重复交出同样把期间加载回来的实例再卸载一次。
//
// Remote 托管实体不支持：它的归属由 Remote 运行时的迁移协议（sharing / draining）交接，保持中的屏障会挡住
// Remote 自己的收尾记录。

var (
	// ErrEntityHandedOff 表示这个实体已被本进程交出（Runtime.HandOffEntity），写到它的事务在 WAL 准入处被拒绝：没有写
	// WAL，Nest 在 Guard 内整体回滚，调用方拿到的错误同时满足 errors.Is(err, nest.ErrCommitRejected)。
	//
	// 与 coredata.ErrEntityUnloading 不同，它不是“稍后重试”：屏障保持到显式 Release 或进程退出，在本进程重试没有意义，
	// 调用方应当按“这个实体不归本进程”处理（换服：把请求退回 / 指到它当前绑定的服）。两个错误互不满足对方的
	// errors.Is；交出还在进行、结论未定的那一段返回的是 ErrEntityUnloading。
	ErrEntityHandedOff = errors.New("dataengine: entity was handed off by this process; it accepts no writes here until the hand-off is released")
	// ErrEntityHandOffUnsupported：Remote 托管实体的归属由 Remote 运行时交接，不经 HandOffEntity。
	ErrEntityHandOffUnsupported = errors.New("dataengine hand-off: a remote-managed entity changes owner through the Remote runtime")
)

// handOffCheckpointPoll 是等 checkpoint 时复查的间隔。用轮询而不是让 checkpoint 路径通知：交出很少发生，
// 等的通常只是一次 checkpoint（毫秒级），不为它在每次 checkpoint 上加一次广播。
const handOffCheckpointPoll = 2 * time.Millisecond

// EntityHandOff 是一次成立的交出：屏障在 Release 之前一直保持。导出字段是 HandOffEntity 这次调用的结果，只读。
// 方法可以并发调用；nil 可以直接 Release（HandOffEntity 失败时返回 nil）。
type EntityHandOff struct {
	EntityID int64
	// Resident：调用开始时实体常驻本进程内存。
	Resident bool
	// Unloaded：这次调用把常驻实例卸载了（或它在调用期间被框架的其他路径卸载）。不常驻时为 false。
	Unloaded bool
	// Drained：进行中屏障登记时该实体在途、这次调用等到投影完成的事务数。
	Drained int
	// AlreadyHandedOff：调用时实体已处于交出状态，这次调用没有新立屏障，返回的是同一份交出。
	AlreadyHandedOff bool

	runtime  *Runtime
	token    uint64
	released atomic.Bool
}

// HandOffEntity 交出实体 id（完整 Entity ID）：排空它的在途投影、等 checkpoint 盖过它的记录、把它从本进程内存卸载，
// 并从此拒绝写到它的事务，直到返回值的 Release 被调用或进程退出（见本文件开头）。实体不常驻时同样立屏障。
//
// 会等待，只能在业务池之外调用（io 池、独立 goroutine）；在业务 worker 上调用直接返回 fctx.ErrBlockingInBusinessWorker，
// 不做任何事。ctx 约束取实体锁、等投影与等 checkpoint：调用时已结束则原样返回它的错误、什么也不做；等待中到期或被
// 取消返回 ErrEntityUnloadBusy / ErrEntityUnloadTimeout（不包裹 ctx 的错误，结论是确定的“没有交出”）。
// 失败时返回 nil 与错误，屏障已撤销；实体此前已处于交出状态时那份交出不受影响。
//
// 对已交出的实体再次调用是幂等的（AlreadyHandedOff）：返回同一份交出，期间被加载回来的实例再卸载一次；任一返回值
// Release 即解除。
func (runtime *Runtime) HandOffEntity(ctx context.Context, id int64) (handOff *EntityHandOff, err error) {
	if err := fctx.BlockingError("dataengine.HandOffEntity"); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime == nil || runtime.Projector == nil || runtime.access == nil || runtime.access.Manager() == nil {
		return nil, errors.New("dataengine hand-off: runtime is not configured")
	}
	if !runtime.Ready() {
		return nil, coredata.ErrRecoveryIncomplete
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("dataengine hand-off: entity %d: nothing was done: %w", id, err)
	}
	// 不是完整 Entity ID（例如只传了唯一号）时屏障立在一个没有任何记录会写的 ID 上，调用方却以为交出了：拒绝。
	if _, err := entity.NormalizeFullID(id, entity.EntityKindNone); err != nil {
		return nil, fmt.Errorf("dataengine hand-off: %w", err)
	}
	if entity.IsEntityKindRemoteManaged(entity.GetEntityKindFromID(id)) {
		return nil, fmt.Errorf("%w: entity %d", ErrEntityHandOffUnsupported, id)
	}
	defer func() {
		metrics.IncCounter("dataengine.entity_handoff.total", metrics.Labels{"result": handOffOutcomeLabel(handOff, err)}, 1)
	}()
	projector := runtime.Projector
	result := &EntityHandOff{EntityID: id, runtime: runtime}
	local := entity.WithLocalExecutor(ctx, projector.localRun())

	// 1. 进行中屏障。
	release, err := runtime.raiseHandOffBarrier(ctx, local, result)
	if err != nil {
		return nil, err
	}
	defer release()

	// 2. 排空。
	result.Drained = projector.pendingEntityProjections(id)
	if err := projector.WaitEntityProjection(ctx, id); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: entity %d: %d of %d admitted transaction(s) still unprojected: %v",
				ErrEntityUnloadTimeout, id, projector.pendingEntityProjections(id), result.Drained, err)
		}
		return nil, fmt.Errorf("dataengine hand-off: entity %d: wait for its projections: %w", id, err)
	}

	// 3. 等 checkpoint。
	if err := projector.waitProjectedRecordsConfirmed(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: entity %d: its transactions are projected but the WAL checkpoint has not confirmed them: %v",
				ErrEntityUnloadTimeout, id, err)
		}
		return nil, fmt.Errorf("dataengine hand-off: entity %d: wait for the WAL checkpoint: %w", id, err)
	}

	// 4. 卸载此刻常驻的实例（不常驻时是在屏障登记之后被加载进来的；屏障下它没有新记录，同样来自库）。
	unloaded, err := runtime.unloadResident(ctx, local, id)
	if err != nil {
		return nil, err
	}
	result.Unloaded = unloaded

	// 5. 保持。release（defer）随后撤进行中屏障。
	result.token, result.AlreadyHandedOff = projector.holdEntityHandOff(id)
	slog.Info("dataengine: entity handed off; this process refuses writes to it until the hand-off is released",
		"entity", id, "resident", result.Resident, "drained", result.Drained, "already", result.AlreadyHandedOff)
	return result, nil
}

// Release 解除交出：卸载此刻常驻的实例（交出期间被加载回来的实例可能已落后于库，见本文件开头），再撤屏障；之后的
// 访问照常冷加载、照常写。调用方在确认本进程（重新）拥有这个实体的写权之后才调用：交出被放弃（换服 Abort），或它
// 后来又回到本进程。
//
// 会等待（等在途的冷加载落定、取实体锁），只能在业务池之外调用；在业务 worker 上返回 fctx.ErrBlockingInBusinessWorker。
// 常驻实例在 ctx 内卸载不掉（持有它的事务没结束）、或同一实体上另一次交出 / 卸载 / Release 正在进行时返回
// ErrEntityUnloadBusy，屏障保持，可以重试。返回 nil 即已解除；重复调用、对 nil 调用、交出已被另一个返回值解除、
// Runtime 已停机时都返回 nil。
func (handOff *EntityHandOff) Release(ctx context.Context) error {
	if handOff == nil || handOff.runtime == nil || handOff.released.Load() {
		return nil
	}
	if err := fctx.BlockingError("dataengine.EntityHandOff.Release"); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime, id := handOff.runtime, handOff.EntityID
	projector := runtime.Projector
	if !projector.entityHandOffHeld(id, handOff.token) {
		handOff.released.Store(true)
		return nil
	}
	drop := func() {
		if projector.releaseEntityHandOff(id, handOff.token) {
			slog.Info("dataengine: entity hand-off released; this process accepts writes to it again", "entity", id)
		}
		handOff.released.Store(true)
	}
	if !runtime.Ready() {
		// 停机中或已停机：不再有准入，屏障只剩内存里的登记。
		drop()
		return nil
	}
	// 进行中屏障：这一段的写入拿到可重试的 ErrEntityUnloading（Release 之后重试会成功），并与交出、运维卸载互斥。
	release, err := projector.beginEntityUnload(id)
	if errors.Is(err, ErrEntityUnloadBusy) {
		return fmt.Errorf("dataengine hand-off release: %w; the hand-off is still in force", err)
	}
	if err != nil {
		// 投影器已 fatal / 关闭、WAL 已 terminal：准入本来就全部拒绝，进程正在 fail-stop。
		drop()
		return nil
	}
	defer release()
	// 此刻在途的冷加载读库可能早于下一个写者的最后一笔写入：等它落定，下面一并卸载。
	if runtime.Repository != nil {
		if err := runtime.Repository.awaitLoadFlight(ctx, id); err != nil {
			return fmt.Errorf("%w: entity %d: a cold load is still in flight; the hand-off is still in force: %v", ErrEntityUnloadBusy, id, err)
		}
	}
	local := entity.WithLocalExecutor(ctx, projector.localRun())
	if _, err := runtime.unloadResident(ctx, local, id); err != nil {
		return fmt.Errorf("dataengine hand-off release: the hand-off is still in force: %w", err)
	}
	drop()
	return nil
}

// raiseHandOffBarrier 是第 1 步：常驻时走运维卸载的同一个入口（业务池持实体锁确认可以卸载，再登记），不常驻或取锁
// 期间实例已离开内存时直接登记。
func (runtime *Runtime) raiseHandOffBarrier(ctx, local context.Context, result *EntityHandOff) (func(), error) {
	id := result.EntityID
	if resident := runtime.access.Manager().Get(id); resident != nil {
		result.Resident = true
		var release func()
		var barrierErr error
		if runErr := entity.RunLocal(local, func() { release, barrierErr = runtime.raiseUnloadBarrier(ctx, resident) }); runErr != nil {
			if release != nil {
				release()
			}
			return nil, fmt.Errorf("dataengine hand-off: entity %d: run on the business pool: %w", id, runErr)
		}
		if barrierErr != nil {
			return nil, barrierErr
		}
		if release != nil {
			return release, nil
		}
	}
	return runtime.Projector.beginEntityUnload(id)
}

// unloadResident 在业务池卸载实体 id 此刻常驻的实例，返回是否卸载了一个实例；不常驻时什么也不做。调用方持有该实体的
// 进行中屏障并已排空它的投影，所以“内存 == 库”。不是自动持久化的实例不卸载（它不写库，卸载就是丢弃）。
// 实例仍常驻时返回 ErrEntityUnloadBusy（持有它的事务在 ctx 内没结束）；实例已离开内存、只是生命周期回调失败时只记录。
func (runtime *Runtime) unloadResident(ctx, local context.Context, id int64) (unloaded bool, err error) {
	var unloadErr error
	resident := false
	runErr := entity.RunLocal(local, func() {
		current := runtime.access.Manager().Get(id)
		if current == nil || !current.AutoPersist() {
			return
		}
		unloadErr = unloadOnce(ctx, runtime.access, current)
		unloaded = current.IsRemoved()
		resident = !unloaded
	})
	if runErr != nil {
		return false, fmt.Errorf("dataengine hand-off: entity %d: run on the business pool: %w", id, runErr)
	}
	if resident {
		if errors.Is(unloadErr, context.DeadlineExceeded) || errors.Is(unloadErr, context.Canceled) {
			return false, fmt.Errorf("%w: entity %d is held by a running transaction: %v", ErrEntityUnloadBusy, id, unloadErr)
		}
		return false, fmt.Errorf("dataengine hand-off: entity %d: unload: %w", id, unloadErr)
	}
	if unloadErr != nil {
		slog.Error("dataengine hand-off: a lifecycle hook failed while unloading the entity", "entity", id, "err", unloadErr)
	}
	return unloaded, nil
}

func handOffOutcomeLabel(handOff *EntityHandOff, err error) string {
	switch {
	case err == nil && handOff != nil && handOff.AlreadyHandedOff:
		return "already"
	case err == nil:
		return "handed_off"
	case errors.Is(err, ErrEntityUnloadBusy):
		return "busy"
	case errors.Is(err, ErrEntityUnloadTimeout):
		return "timeout"
	default:
		return "failed"
	}
}

// awaitLoadFlight 等实体 id 此刻在途的那次冷加载结束（成功、失败都算）；没有在途加载时立即返回。
func (repository *EntityRepository) awaitLoadFlight(ctx context.Context, id int64) error {
	repository.flightMu.Lock()
	flight := repository.flights[id]
	repository.flightMu.Unlock()
	if flight == nil {
		return nil
	}
	select {
	case <-flight.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// holdEntityHandOff 在 heldMu 下登记实体 id 的交出屏障，返回它的令牌；已登记时返回原令牌与 already=true。
// 调用方此时仍持有该实体的进行中屏障，登记之后再撤后者，准入处没有空档。
func (p *Projector) holdEntityHandOff(id int64) (token uint64, already bool) {
	p.heldMu.Lock()
	defer p.heldMu.Unlock()
	if token, held := p.handedOffEntities[id]; held {
		return token, true
	}
	if p.handedOffEntities == nil {
		p.handedOffEntities = make(map[int64]uint64)
	}
	p.handOffToken++
	p.handedOffEntities[id] = p.handOffToken
	metrics.SetGauge("dataengine.entity_handoff.held", nil, int64(len(p.handedOffEntities)))
	return p.handOffToken, false
}

// releaseEntityHandOff 撤销令牌为 token 的交出屏障，返回是否撤销了；令牌对不上（已被解除、之后又有新的一次交出）时
// 什么也不做。
func (p *Projector) releaseEntityHandOff(id int64, token uint64) bool {
	p.heldMu.Lock()
	defer p.heldMu.Unlock()
	if current, held := p.handedOffEntities[id]; !held || current != token {
		return false
	}
	delete(p.handedOffEntities, id)
	metrics.SetGauge("dataengine.entity_handoff.held", nil, int64(len(p.handedOffEntities)))
	return true
}

func (p *Projector) entityHandOffHeld(id int64, token uint64) bool {
	p.heldMu.RLock()
	defer p.heldMu.RUnlock()
	current, held := p.handedOffEntities[id]
	return held && current == token
}

// checkHandedOffEntitiesLocked 在 reserve 的 heldMu 临界区内拒绝写到已交出实体的新记录。排在卸载屏障的检查之后：
// 交出、Release 进行中的实体两张表里都有，先拿到的是可重试的 ErrEntityUnloading。
func (p *Projector) checkHandedOffEntitiesLocked(record coredata.CommitRecord) error {
	if len(p.handedOffEntities) == 0 {
		return nil
	}
	for i := range record.Mutations {
		id := record.Mutations[i].Key.ID
		if _, handedOff := p.handedOffEntities[id]; handedOff {
			p.handOffRejected.Add(1)
			metrics.IncCounter("dataengine.entity_handoff.admission_rejected.total", nil, 1)
			return fmt.Errorf("%w: entity %d", ErrEntityHandedOff, id)
		}
	}
	return nil
}

// waitProjectedRecordsConfirmed 等调用时“已投影、checkpoint 还没确认”的记录全部被 WAL checkpoint 确认。
//
// 调用方（HandOffEntity）持有实体的屏障并已排空它的投影，所以该实体在本进程准入过的记录此刻要么已确认，要么就在
// 这个集合里；集合清空即 checkpoint 已盖过它的最后一条记录。集合不按实体取（投影完成后投影器不再记录事务属于哪个
// 实体），而是取全部已投影未确认的记录：checkpoint 只按 WAL 顺序前进，确认该实体的记录本来就要先确认排在它前面的
// 记录；集合的大小以一轮回放读取的记录数为限，与积压无关。
//
// 只读投影器的登记，不驱动回放、不改 checkpoint 的时机；取集合要在读锁下遍历一次 admitted（未确认记录数）。
// 投影 fatal、WAL terminal、投影器关闭时返回对应错误（本进程不会再确认）；投影或 checkpoint 只是暂时失败时一直等到
// ctx 结束。
func (p *Projector) waitProjectedRecordsConfirmed(ctx context.Context) error {
	if err := p.confirmationStopped(); err != nil {
		return err
	}
	unconfirmed := p.unconfirmedProjections()
	if len(unconfirmed) == 0 {
		return nil
	}
	ticker := time.NewTicker(handOffCheckpointPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.ctx.Done():
			return ErrRuntimeStopped
		}
		if err := p.confirmationStopped(); err != nil {
			return err
		}
		if unconfirmed = p.stillUnconfirmed(unconfirmed); len(unconfirmed) == 0 {
			return nil
		}
	}
}

// confirmationStopped 报告本进程是否已不会再确认记录：投影 fatal、WAL terminal、投影器关闭。它是 projectionUsable 去掉
// WAL 健康检查（读目录，checkpoint 之后还要读一帧 WAL）的部分，轮询里每次都查；调用方在此之前刚经
// WaitEntityProjection 查过完整的 projectionUsable。
func (p *Projector) confirmationStopped() error {
	if fatal := p.fatal(); fatal != nil {
		return fatal
	}
	if terminal := p.walTerminal(); terminal != nil {
		return terminal
	}
	if p.ctx.Err() != nil {
		return ErrRuntimeStopped
	}
	return nil
}

// unconfirmedProjections 是此刻已投影、checkpoint 未确认的事务：仍在 admitted 里（checkpoint 成功后才移出），
// 而投影完成时已从 pendingTransactions 移出。两张表都在 heldMu 下维护，后者是前者的子集。
func (p *Projector) unconfirmedProjections() []coredata.TransactionID {
	p.heldMu.RLock()
	defer p.heldMu.RUnlock()
	if len(p.admitted) == len(p.pendingTransactions) {
		return nil
	}
	unconfirmed := make([]coredata.TransactionID, 0, len(p.admitted)-len(p.pendingTransactions))
	for id := range p.admitted {
		if _, pending := p.pendingTransactions[id]; !pending {
			unconfirmed = append(unconfirmed, id)
		}
	}
	return unconfirmed
}

// stillUnconfirmed 留下 ids 里仍未被 checkpoint 确认的事务（就地过滤）。
func (p *Projector) stillUnconfirmed(ids []coredata.TransactionID) []coredata.TransactionID {
	p.heldMu.RLock()
	defer p.heldMu.RUnlock()
	remaining := ids[:0]
	for _, id := range ids {
		if _, unacked := p.admitted[id]; unacked {
			remaining = append(remaining, id)
		}
	}
	return remaining
}
