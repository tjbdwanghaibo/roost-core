package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
)

// ReplayPass 按 WAL 物理顺序读取，在仍持有 Entity 锁的事务前停下。
// 分段投影成功后才推进 ack；Mongo 已提交而 ack 失败时，下次依靠幂等身份重放。
func (projector *Projector) ReplayPass(ctx context.Context) (processed int, resultErr error) {
	if !projector.operations.Begin() {
		return 0, ErrRuntimeStopped
	}
	defer projector.operations.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := projector.replayGate.acquire(ctx); err != nil {
		return 0, err
	}
	defer projector.replayGate.release()
	records, fences, replayErr := projector.readReplayBatch(ctx)
	if len(records) == 0 {
		return 0, replayErr
	}
	_, batchCapable := projector.store.(BatchProjectionStore)
	// processed 是已成功投影的连续前缀；acked 只在 checkpoint 成功后前移。
	acked := 0
	lastAck := projector.now()
	checkpoint := func() error {
		if processed == acked {
			return nil
		}
		ackStart := hoptrace.Now()
		if err := projector.ack(ctx, fences[processed-1]); err != nil {
			projector.observeAckError(err)
			return err
		}
		hoptrace.Since("h09.de.checkpoint", ackStart)
		projector.acknowledge(records[acked:processed])
		acked = processed
		lastAck = projector.now()
		return nil
	}
	// 所有退出路径都只尝试确认已成功的前缀。ack 失败不能被 held 哨兵掩盖。
	ackFailed := false
	defer func() {
		if ackFailed {
			return
		}
		if err := checkpoint(); err != nil {
			if errors.Is(resultErr, errProjectorTransactionHeld) {
				resultErr = err
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	for _, window := range projector.planReplayWindows(records) {
		if window.lanes != nil {
			// 并行窗口：先确认之前的成功前缀，窗口结束后只确认窗口内的连续成功前缀（设计 §6）。
			if err := checkpoint(); err != nil {
				ackFailed = true
				return processed, err
			}
			prefix, err := projector.projectParallelWindow(ctx, records[window.start:window.end], fences[window.start:window.end], window)
			processed += prefix
			if err != nil {
				// fatal 已由 projectParallelWindow 逐通道判定（isFatalProjection 统一唤醒全部待投影等待方）。
				return processed, err
			}
			if err := checkpoint(); err != nil {
				ackFailed = true
				return processed, err
			}
			if window.start+prefix < window.end {
				// 某通道提前停下（见 projectLane）：前缀之后的记录本轮不再投影，下一轮从确认前缀重读。
				return processed, nil
			}
			continue
		}
		segments, err := planProjectionSegments(records[window.start:window.end], fences[window.start:window.end], projector.opts.ReplayBatchRecords, projector.opts.ReplayBatchBytes)
		if err != nil {
			return processed, err
		}
		for _, segment := range segments {
			// 批量写报告无法自行判定结果（ErrProjectionBatchNeedsPerRecord）时锁定 perRecord：
			// 本段剩余记录改走单条路径，由它比较已存版本与 _last_tx，区分已应用的重放和
			// 真实冲突。锁定而不是只重试失败单元，避免反复发出注定再次退回的批量写。
			perRecord := false
			for start := 0; start < len(segment.records); {
				end := len(segment.records)
				if segment.batch && (!batchCapable || perRecord) {
					end = start + 1
				}
				unit := projectionSegment{
					records: segment.records[start:end],
					fences:  segment.fences[start:end],
					batch:   segment.batch,
				}
				projectStart := hoptrace.Now()
				err := projector.projectSegment(ctx, unit)
				if err != nil {
					if errors.Is(err, ErrProjectionBatchNeedsPerRecord) && len(unit.records) > 1 {
						perRecord = true
						continue
					}
					projector.isFatalProjection(err)
					return processed, fmt.Errorf("dataengine projector: segment first_transaction=%s records=%d: %w", unit.records[0].ID.String(), len(unit.records), err)
				}
				projector.finishProjectedUnit(unit.records, nil, projectStart, nil)
				processed += len(unit.records)
				// checkpoint 只决定崩溃后从哪里重放，进程外没有读者；已投影未确认的记录靠持久事务标记幂等重放
				// （批次、多 mutation、effect / 回执 / Remote 记录都先查标记，标记与业务写同一 Mongo 事务提交，
				// 重放时不重复暂存回执与 effect）。所以这些单元按条数 / 间隔合并确认。
				// 例外：单 mutation 快路只保存文档末次事务 ID，后续版本覆盖它后旧记录无法判定幂等，必须立即确认；
				// 非批量 Store 没有承诺这种重放安全，也逐单元确认。
				replaySafe := batchCapable && (len(unit.records) > 1 || !projectsOnFastPath(unit.records[0]))
				if !replaySafe || processed-acked >= projector.opts.CheckpointRecords || projector.now().Sub(lastAck) >= projector.opts.CheckpointInterval {
					if err := checkpoint(); err != nil {
						ackFailed = true
						return processed, err
					}
				}
				start = end
			}
		}
	}
	return processed, replayErr
}

// replayReadRecords 是一轮回放最多读取的记录数：能并行投影时为 ReplayBatchRecords × ProjectionWorkers（每条通道仍能
// 凑满一个批，而不是把一个批拆成几份），否则 ReplayBatchRecords。字节上限仍是 ReplayReadBytes，不随通道数放大。
func (projector *Projector) replayReadRecords() int {
	if store, ok := projector.store.(ParallelProjectionStore); ok && store.SupportsParallelProjection() && projector.opts.ProjectionWorkers > 1 {
		return saturatingMul(projector.opts.ReplayBatchRecords, projector.opts.ProjectionWorkers)
	}
	return projector.opts.ReplayBatchRecords
}

// planReplayWindows 决定本轮是否并行：ProjectionWorkers > 1 且 Store 声明 ParallelProjectionStore 时按冲突键切窗口，
// 否则整轮一个串行段（与改动前相同）。Remote 记录另需 Store 声明 Remote 适配器可并发调用，否则 Remote 记录之间串行。
func (projector *Projector) planReplayWindows(records []coredata.CommitRecord) []replayWindow {
	store, ok := projector.store.(ParallelProjectionStore)
	if !ok || !store.SupportsParallelProjection() {
		return []replayWindow{{start: 0, end: len(records)}}
	}
	remote, ok := projector.store.(RemoteParallelProjectionStore)
	remoteParallel := ok && remote.SupportsRemoteParallelProjection()
	return planReplayWindows(records, projector.opts.ProjectionWorkers, remoteParallel)
}

// readReplayBatch 按 WAL 物理顺序收集本轮可投影的连续记录：遇到仍持有实体锁的事务、
// 达到条数（replayReadRecords：串行时 ReplayBatchRecords，并行时乘以 ProjectionWorkers，让每条通道仍能凑满一个批）
// 或读取字节上限（ReplayReadBytes，不随通道数放大）时停止。返回的错误已去掉“批次已满”哨兵，可能是
// errProjectorTransactionHeld 或 WAL 读取错误；记录为空时调用方不做投影。
func (projector *Projector) readReplayBatch(ctx context.Context) ([]corenest.CommitRecord, []corenest.CommitFence, error) {
	var records []corenest.CommitRecord
	var fences []corenest.CommitFence
	readBytes := 0
	readRecords := projector.replayReadRecords()
	replayErr := projector.wal.Replay(ctx, func(fence corenest.CommitFence, record corenest.CommitRecord) error {
		// 上一条已被本轮接收，必须让它的 consume 返回 nil，WAL 才能如实计数。
		// 读到下一条时停止；这条 lookahead 不保留、不确认。
		if len(records) >= readRecords || readBytes >= projector.opts.ReplayReadBytes {
			return errProjectorBatchComplete
		}
		if projector.isHeld(record.ID) {
			return errProjectorTransactionHeld
		}
		size := projectionRecordLogicalBytes(record)
		if len(records) > 0 && size > projector.opts.ReplayReadBytes-readBytes {
			// WAL 已解码这一条，但不保留、不确认它；下一轮从成功前缀后重新读取。
			return errProjectorBatchComplete
		}
		// 空闲或首条事务尚未解锁时不需要批次空间。等到确实有可投影
		// 记录才分配，避免后台每次无进展轮询都申请整个批次的容量。
		if records == nil {
			records = make([]corenest.CommitRecord, 0, min(readRecords, 1024))
			fences = make([]corenest.CommitFence, 0, min(readRecords, 1024))
		}
		records = append(records, record)
		fences = append(fences, fence)
		readBytes = saturatingAdd(readBytes, size)
		return nil
	})
	if errors.Is(replayErr, errProjectorBatchComplete) {
		replayErr = nil
	}
	return records, fences, replayErr
}

// projectSegment 投影一个单元：单笔段交给 Store.Project（特殊记录总是单独成段，isLocalBatchRecord 排除
// effect / 回执 / Remote 写），多笔段是连续的普通本地记录，Store 支持时一次 ProjectBatch。
func (projector *Projector) projectSegment(ctx context.Context, segment projectionSegment) error {
	if len(segment.records) == 1 {
		record := segment.records[0]
		if err := projector.store.Project(ctx, record); err != nil {
			return fmt.Errorf("transaction %s: %w", record.ID.String(), err)
		}
		return nil
	}
	if store, ok := projector.store.(BatchProjectionStore); ok {
		return store.ProjectBatch(ctx, segment.records)
	}
	for i := range segment.records {
		if err := projector.store.Project(ctx, segment.records[i]); err != nil {
			return fmt.Errorf("transaction %s: %w", segment.records[i].ID.String(), err)
		}
	}
	return nil
}

func (projector *Projector) run() {
	defer close(projector.done)
	backoff := projector.opts.RetryMin
	for {
		processed, err := projector.ReplayPass(projector.ctx)
		if errors.Is(err, errProjectorTransactionHeld) {
			backoff = projector.opts.RetryMin
			if !projector.wait(projector.opts.RetryMin) {
				return
			}
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			projector.recordFailure(err)
			if projector.isFatalProjection(err) {
				return
			}
			if !projector.wait(jitterDuration(backoff)) {
				return
			}
			backoff = min(backoff*2, projector.opts.RetryMax)
			continue
		}
		if projector.ctx.Err() != nil {
			return
		}
		projector.setLastError(nil)
		backoff = projector.opts.RetryMin
		if processed > 0 {
			continue
		}
		if !projector.wait(projector.opts.IdlePoll) {
			return
		}
	}
}

func (projector *Projector) acknowledge(records []coredata.CommitRecord) {
	projector.heldMu.Lock()
	for _, record := range records {
		id := record.ID
		if _, ok := projector.admitted[id]; !ok {
			continue
		}
		delete(projector.admitted, id)
		projector.walUnacked.Add(^uint64(0))
	}
	metrics.SetGauge("dataengine.projection.pending", nil, int64(projector.walUnacked.Load()))
	projector.heldMu.Unlock()
}

func (projector *Projector) wait(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-projector.kick:
		return true
	case <-timer.C:
		return true
	case <-projector.ctx.Done():
		return false
	}
}

func jitterDuration(duration time.Duration) time.Duration {
	if duration <= 1 {
		return duration
	}
	half := duration / 2
	return half + time.Duration(rand.Int64N(int64(duration-half)))
}

// traceProjectedUnit 是投影的逐跳计时（只在性能夹具安装了 hoptrace 时调用）：WAL 准入 → 本单元开始投影的等待
// （含等事务释放实体锁与投影器唤醒）、单条带回执 / effect 的投影事务耗时，并给已暂存的 effect 打上时刻，供 outbox
// 发布时计算排队时间。
func traceProjectedUnit(records []coredata.CommitRecord, start time.Time) {
	elapsed := time.Since(start)
	for i := range records {
		hoptrace.SinceMarkAt("h09.de.project_wait", "tx-committed/", records[i].ID.String(), start)
	}
	if len(records) == 1 && (len(records[0].Effects) > 0 || len(records[0].Receipts) > 0) {
		hoptrace.Observe("h09.de.project_txn", elapsed)
	} else {
		hoptrace.Observe("h09.de.project_batch", elapsed)
	}
	for i := range records {
		for j := range records[i].Effects {
			hoptrace.Mark("effect-staged/", records[i].Effects[j].ID)
		}
	}
}

func hasEffects(records []coredata.CommitRecord) bool {
	for i := range records {
		if len(records[i].Effects) > 0 {
			return true
		}
	}
	return false
}
